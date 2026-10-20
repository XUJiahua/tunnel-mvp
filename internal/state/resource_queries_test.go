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

func TestResourceQueriesCommittedValues(t *testing.T) {
	for _, kind := range []string{JSON, SQLite} {
		t.Run(kind, func(t *testing.T) {
			initial := sample(t)
			initial.Services[0].CustomDomain = "platform.example.com"
			initial.Services[0].HealthPath = "healthz"
			initial.Services[0].HealthMethod = "GET"
			initial.Services[0].HealthExpectedStatus = 200
			initial.Services[0].ConnectionMode = "session"
			initial.Sites[0].LegacyDisabled = true
			s := mustOpen(t, t.TempDir(), kind, initial)
			site, found, err := s.SiteByID("site-a")
			if err != nil || !found || !reflect.DeepEqual(site, initial.Sites[0]) {
				t.Fatal("site mismatch", found, err)
			}
			svc, found, err := s.ServiceByID("platform")
			if err != nil || !found || !reflect.DeepEqual(svc, initial.Services[0]) {
				t.Fatal("service mismatch", found, err)
			}
			byDomain, found, err := s.ServiceByDomain("platform.example.com")
			if err != nil || !found || byDomain != svc {
				t.Fatal("domain mismatch", found, err)
			}
			if _, found, err = s.ServiceByDomain(""); err != nil || found {
				t.Fatal("empty domain matched")
			}
			if _, found, err = s.ServiceByID("platform' OR 1=1 --"); err != nil || found {
				t.Fatal("query was not parameterized")
			}
			if err = s.Update(func(st *State) error {
				st.Services[0].CustomDomain = "new.example.com"
				st.Services[0].URL = "http://127.0.0.1:3100/moved/"
				st.Sites[0].Name = "Renamed"
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if _, found, err = s.ServiceByDomain("platform.example.com"); err != nil || found {
				t.Fatal("old domain retained")
			}
			byDomain, found, err = s.ServiceByDomain("new.example.com")
			if err != nil || !found || byDomain.URL != "http://127.0.0.1:3100/moved/" {
				t.Fatal("new commit missing", err)
			}
			if err = s.Update(func(st *State) error { st.Services[0].SiteID = "missing"; return nil }); err == nil {
				t.Fatal("invalid update accepted")
			}
			after, found, err := s.ServiceByID("platform")
			if err != nil || !found || after != byDomain {
				t.Fatal("rejected state leaked")
			}
			if err = s.Close(); err != nil {
				t.Fatal(err)
			}
			if _, found, err = s.ServiceByID("platform"); err == nil || found {
				t.Fatal("closed store read accepted")
			}
			if _, found, err = s.SiteByID("site-a"); err == nil || found {
				t.Fatal("closed site read accepted")
			}
		})
	}
}

func TestResourceQueriesSQLiteRollbackAndFailure(t *testing.T) {
	s := mustOpen(t, t.TempDir(), SQLite, sample(t))
	b := s.disk.(*sqliteBackend)
	if _, err := b.db.Exec(`CREATE TRIGGER reject_resource BEFORE INSERT ON services WHEN NEW.name='rejected' BEGIN SELECT RAISE(ABORT,'test durable failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := s.Update(func(st *State) error { st.Services[0].Name = "rejected"; return nil }); err == nil {
		t.Fatal("trigger did not reject")
	}
	svc, found, err := s.ServiceByID("platform")
	if err != nil || !found || svc.Name != "平台" {
		t.Fatal("failed transaction leaked", err)
	}
	if err := b.db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, found, err := s.ServiceByID("platform"); err == nil || found {
		t.Fatal("SQL error fell back to snapshot")
	}
}

func TestResourceQueriesSQLiteDomainIndexUpgrade(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, SQLite, func() (State, error) { return sample(t), nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.disk.(*sqliteBackend).db.Exec("DROP INDEX services_custom_domain"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s = mustOpen(t, dir, SQLite, State{})
	b := s.disk.(*sqliteBackend)
	rows, err := b.db.Query("EXPLAIN QUERY PLAN SELECT id FROM services WHERE custom_domain=? AND custom_domain!=''", "demo.example.com")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	indexed := false
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		indexed = indexed || strings.Contains(detail, "services_custom_domain")
	}
	if rows.Err() != nil || !indexed {
		t.Fatal("domain lookup is not indexed", rows.Err())
	}
}

func TestResourceQueriesConcurrentUpdates(t *testing.T) {
	for _, kind := range []string{JSON, SQLite} {
		t.Run(kind, func(t *testing.T) {
			s := mustOpen(t, t.TempDir(), kind, sample(t))
			var wg sync.WaitGroup
			for i := 0; i < 4; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for j := 0; j < 30; j++ {
						v, found, err := s.ServiceByID("platform")
						if err != nil || !found || v.ID != "platform" || v.SiteID != "site-a" {
							t.Error("noncommitted lookup", err)
							return
						}
					}
				}()
			}
			for i := 0; i < 10; i++ {
				if err := s.Update(func(st *State) error { st.Services[0].Name = fmt.Sprintf("revision-%d", i); return nil }); err != nil {
					t.Fatal(err)
				}
			}
			wg.Wait()
		})
	}
}

func BenchmarkResourceQueries(b *testing.B) {
	hash, _ := bcrypt.GenerateFromPassword([]byte("benchmark-password"), bcrypt.MinCost)
	initial := State{Users: []User{{ID: "admin", Username: "admin", Role: "admin", PasswordHash: string(hash)}}, Sites: []Site{{ID: "site-a", Name: "Site", TokenHash: strings.Repeat("a", 64)}}}
	// Use a stable valid timestamp without including setup in timed operations.
	initial.Sites[0].CreatedAt = time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 1000; i++ {
		id := fmt.Sprintf("app-%04d", i)
		initial.Services = append(initial.Services, Service{ID: id, Name: id, SiteID: "site-a", URL: "http://127.0.0.1:3000", CustomDomain: id + ".example.com"})
		initial.Users = append(initial.Users, User{ID: fmt.Sprintf("user-%04d", i), Username: fmt.Sprintf("user-%04d", i), Role: "viewer", PasswordHash: string(hash), ServiceIDs: []string{id}})
	}
	s, err := Open(b.TempDir(), SQLite, func() (State, error) { return initial, nil })
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	for _, mode := range []string{"snapshot-id", "sql-id", "sql-domain"} {
		b.Run(mode, func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				switch mode {
				case "snapshot-id":
					found := false
					for _, v := range s.Snapshot().Services {
						if v.ID == "app-0999" {
							found = true
							break
						}
					}
					if !found {
						b.Fatal("missing")
					}
				case "sql-id":
					if _, ok, err := s.ServiceByID("app-0999"); err != nil || !ok {
						b.Fatal(err)
					}
				case "sql-domain":
					if _, ok, err := s.ServiceByDomain("app-0999.example.com"); err != nil || !ok {
						b.Fatal(err)
					}
				}
			}
		})
	}
}
