package state

import (
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

func TestUserContextProjectionOwnershipAndCommit(t *testing.T) {
	for _, kind := range []string{JSON, SQLite} {
		t.Run(kind, func(t *testing.T) {
			initial := organizationSample(t)
			initial.Users[1].Identity = &Identity{Issuer: "https://id.example.com/realms/test", Subject: "reader-subject"}
			initial.Roles = append(initial.Roles, Role{ID: "other-role", OrganizationID: "org-b", Name: "Unrelated", Permissions: []string{"audit:read"}})
			initial.Groups = append(initial.Groups, Group{ID: "other-group", Name: "Unrelated", MemberIDs: []string{"admin"}})
			s := mustOpen(t, t.TempDir(), kind, initial)
			full := s.Snapshot()
			want := State{Users: []User{full.Users[1]}, Roles: []Role{full.Roles[0]}, Groups: []Group{full.Groups[0]}}
			got, found, err := s.UserContext("reader")
			if err != nil || !found || !reflect.DeepEqual(got, want) {
				t.Fatal("projection differs", found, err)
			}
			got.Users[0].Identity.Subject = "mutated"
			got.Users[0].ServiceIDs[0] = "metrics"
			got.Roles[0].Permissions[0] = "roles:manage"
			got.Groups[0].MemberIDs[0] = "admin"
			got, found, err = s.UserContext("reader")
			if err != nil || !found || !reflect.DeepEqual(got, want) {
				t.Fatal("projection aliases committed state")
			}
			if _, found, err = s.UserContext("reader' OR 1=1 --"); err != nil || found {
				t.Fatal("unparameterized or missing user matched")
			}
			if err = s.Update(func(st *State) error {
				st.Users[1].ServiceIDs = nil
				st.Groups[0].ServiceIDs = []string{"metrics"}
				st.Roles[0].Permissions = []string{"audit:read"}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			got, found, err = s.UserContext("reader")
			if err != nil || !found || len(got.Users[0].ServiceIDs) != 0 || got.Groups[0].ServiceIDs[0] != "metrics" || got.Roles[0].Permissions[0] != "audit:read" {
				t.Fatal("new authorization revision missing", err)
			}
			if err = s.Update(func(st *State) error { st.Groups[0].MemberIDs = nil; st.Users[1].Disabled = true; return nil }); err != nil {
				t.Fatal(err)
			}
			got, found, err = s.UserContext("reader")
			if err != nil || !found || !got.Users[0].Disabled || len(got.Groups) != 0 {
				t.Fatal("withdrawal/disable missing", err)
			}
			s.Close()
			if _, found, err = s.UserContext("reader"); err == nil || found {
				t.Fatal("closed store projection accepted")
			}
		})
	}
}

func TestUserContextSQLAtomicFailureAndMetrics(t *testing.T) {
	s := mustOpen(t, t.TempDir(), SQLite, organizationSample(t))
	b := s.disk.(*sqliteBackend)
	before, _, err := s.UserContext("reader")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = b.db.Exec(`CREATE TRIGGER reject_role BEFORE INSERT ON roles WHEN NEW.id='operator' BEGIN SELECT RAISE(ABORT,'test durable failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err = s.Update(func(st *State) error { st.Groups[0].MemberIDs = nil; st.Roles[0].Name = "rejected"; return nil }); err == nil {
		t.Fatal("durable failure accepted")
	}
	after, found, err := s.UserContext("reader")
	if err != nil || !found || !reflect.DeepEqual(after, before) {
		t.Fatal("partial transaction authorization leaked", err)
	}
	if s.QueryReadFailures() != 0 {
		t.Fatal("rollback/not-found counted as read error")
	}
	if _, err = b.db.Exec("DROP TABLE groups"); err != nil {
		t.Fatal(err)
	}
	if _, found, err = s.UserContext("reader"); err == nil || found {
		t.Fatal("partial user accepted after group read failure")
	}
	if s.QueryReadFailures() != 1 {
		t.Fatal("missing projection error counter")
	}
	if err = b.db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, found, err = s.ServiceByID("platform"); err == nil || found {
		t.Fatal("failed resource read accepted")
	}
	if s.QueryReadFailures() != 2 {
		t.Fatal("resource query not counted")
	}
}

func TestUserContextConcurrentRoleRevision(t *testing.T) {
	for _, kind := range []string{JSON, SQLite} {
		t.Run(kind, func(t *testing.T) {
			initial := organizationSample(t)
			initial.Roles[0].Name = "A"
			initial.Groups[0].Name = "A"
			s := mustOpen(t, t.TempDir(), kind, initial)
			var wg sync.WaitGroup
			for i := 0; i < 4; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for j := 0; j < 30; j++ {
						st, found, err := s.UserContext("reader")
						if err != nil || !found || len(st.Roles) != 1 || len(st.Groups) != 1 || st.Roles[0].Name != st.Groups[0].Name {
							t.Error("mixed authorization revision", err)
							return
						}
					}
				}()
			}
			for i := 0; i < 10; i++ {
				name := fmt.Sprintf("rev-%d", i)
				if err := s.Update(func(st *State) error { st.Roles[0].Name = name; st.Groups[0].Name = name; return nil }); err != nil {
					t.Fatal(err)
				}
			}
			wg.Wait()
		})
	}
}

func BenchmarkUserContext(b *testing.B) {
	hash, _ := bcrypt.GenerateFromPassword([]byte("benchmark-password"), bcrypt.MinCost)
	initial := State{Users: []User{{ID: "admin", Username: "admin", Role: "admin", PasswordHash: string(hash)}}, Sites: []Site{{ID: "site-a", Name: "Site", TokenHash: strings.Repeat("a", 64), CreatedAt: time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)}}, Roles: []Role{{ID: "operator", Name: "Operator", Permissions: []string{"audit:read"}}}}
	for i := 0; i < 1000; i++ {
		id := fmt.Sprintf("user-%04d", i)
		app := fmt.Sprintf("app-%04d", i)
		initial.Users = append(initial.Users, User{ID: id, Username: id, Role: "viewer", RoleID: "operator", PasswordHash: string(hash), ServiceIDs: []string{app}})
		initial.Services = append(initial.Services, Service{ID: app, Name: app, SiteID: "site-a", URL: "http://127.0.0.1:3000"})
	}
	initial.Groups = []Group{{ID: "readers", Name: "Readers", MemberIDs: []string{"user-0999"}, ServiceIDs: []string{"app-0999"}}}
	s, err := Open(b.TempDir(), SQLite, func() (State, error) { return initial, nil })
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	for _, mode := range []string{"snapshot", "sql-projection"} {
		b.Run(mode, func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if mode == "snapshot" {
					found := false
					for _, u := range s.Snapshot().Users {
						if u.ID == "user-0999" {
							found = true
							break
						}
					}
					if !found {
						b.Fatal("missing")
					}
				} else {
					st, found, err := s.UserContext("user-0999")
					if err != nil || !found || len(st.Roles) != 1 || len(st.Groups) != 1 {
						b.Fatal("projection missing", err)
					}
				}
			}
		})
	}
}
