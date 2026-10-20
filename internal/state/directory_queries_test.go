package state

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func directorySample(t *testing.T) State {
	st := organizationSample(t)
	for i := 0; i < 9; i++ {
		org := "org-a"
		if i%2 == 0 {
			org = "org-b"
		}
		id := fmt.Sprintf("site-%d", i)
		st.Sites = append(st.Sites, Site{ID: id, Name: id, OrganizationID: org, TokenHash: strings.Repeat("b", 64), CreatedAt: st.Sites[0].CreatedAt})
		st.Services = append(st.Services, Service{ID: fmt.Sprintf("app-%d", i), Name: id, SiteID: id, OrganizationID: org, URL: "http://127.0.0.1:3000"})
	}
	return st
}
func TestDirectoryBackendParityAndAuthorization(t *testing.T) {
	st := directorySample(t)
	stores := []*Store{mustOpen(t, t.TempDir(), JSON, st), mustOpen(t, t.TempDir(), SQLite, st)}
	for _, kind := range []string{"users", "sites", "services", "groups", "roles", "invitations", "organizations"} {
		for _, filter := range []DirectoryFilter{
			{Global: true}, {OrganizationID: "org-a", All: true}, {OrganizationID: "org-b", All: true},
			{OrganizationID: "org-a", ServiceIDs: []string{"platform", "app-3", "app-0"}, MemberID: "reader", ExcludeAdmins: true},
			{OrganizationID: "org-a", SiteIDs: []string{"site-a", "site-1", "site-0"}},
			{OrganizationID: "org-a", AllServices: true}, {OrganizationID: "org-a", MemberID: "reader' OR 1=1 --"},
		} {
			q := DirectoryQuery{Kind: kind, Filter: filter}
			want, _, _ := memoryDirectory(st, q, -1)
			for _, store := range stores {
				page, err := store.Directory(q)
				if err != nil || !reflect.DeepEqual(page.State, want) {
					t.Fatalf("%s unpaged mismatch: %v\ngot%+v\nwant%+v", kind, err, page.State, want)
				}
				q.Limit = 2
				q.Cursor = ""
				var got State
				for pages := 0; ; pages++ {
					if pages > 15 {
						t.Fatal("pagination did not progress")
					}
					page, err = store.Directory(q)
					if err != nil {
						t.Fatal(err)
					}
					visitDirectory(page.State, kind, func(_ int, v any) bool { appendDirectory(&got, v); return true })
					q.Cursor = page.NextCursor
					if q.Cursor == "" {
						break
					}
				}
				// Compare selected entities; role projections may repeat across user pages.
				var expected State
				visitDirectory(want, kind, func(_ int, v any) bool { appendDirectory(&expected, v); return true })
				if !reflect.DeepEqual(Clone(got), Clone(expected)) {
					t.Fatalf("%s paged mismatch", kind)
				}
				q.Limit = 0
				q.Cursor = ""
			}
		}
	}
}
func TestDirectoryCursorRevisionScopeOwnership(t *testing.T) {
	for _, kind := range []string{JSON, SQLite} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			s := mustOpen(t, dir, kind, directorySample(t))
			q := DirectoryQuery{Kind: "users", Filter: DirectoryFilter{Global: true, ActorID: "admin"}, Limit: 1}
			page, err := s.Directory(q)
			if err != nil || page.NextCursor == "" {
				t.Fatal(err)
			}
			page.State.Users[0].SiteIDs = append(page.State.Users[0].SiteIDs, "mutated")
			full, _ := s.Directory(DirectoryQuery{Kind: "users", Filter: q.Filter})
			if len(full.State.Users[0].SiteIDs) != 0 {
				t.Fatal("directory aliases state")
			}
			q.Cursor = page.NextCursor
			if err = s.Update(func(st *State) error { st.Sites[0].Name = "rejected"; return errors.New("reject") }); err == nil {
				t.Fatal("rejected mutation accepted")
			}
			if _, err = s.Directory(q); err != nil {
				t.Fatal("rejected mutation invalidates cursor", err)
			}
			for _, change := range []func(*DirectoryQuery){func(q *DirectoryQuery) { q.Kind = "sites" }, func(q *DirectoryQuery) { q.Filter.ActorID = "other" }, func(q *DirectoryQuery) { q.Filter.Global = false }} {
				invalid := q
				change(&invalid)
				if _, err = s.Directory(invalid); !errors.Is(err, ErrDirectoryQuery) {
					t.Fatal("cursor crossed scope", err)
				}
			}
			if err = s.Update(func(st *State) error { st.Sites[0].Name = "committed"; return nil }); err != nil {
				t.Fatal(err)
			}
			if _, err = s.Directory(q); !errors.Is(err, ErrDirectoryChanged) {
				t.Fatal("mixed revision allowed", err)
			}
			q.Cursor = ""
			page, err = s.Directory(q)
			if err != nil {
				t.Fatal(err)
			}
			q.Cursor = page.NextCursor
			s.Close()
			s = mustOpen(t, dir, kind, State{})
			if _, err = s.Directory(q); !errors.Is(err, ErrDirectoryChanged) {
				t.Fatal("cursor survived restart", err)
			}
			for _, invalid := range []DirectoryQuery{{Kind: "users", Limit: 201}, {Kind: "users", Limit: -1}, {Kind: "users", Cursor: "bad"}, {Kind: "users", Limit: 1, Cursor: "bad"}, {Kind: "users; DROP TABLE users"}} {
				if _, err = s.Directory(invalid); !errors.Is(err, ErrDirectoryQuery) {
					t.Fatal("invalid query allowed", err)
				}
			}
			s.Close()
			if _, err = s.Directory(DirectoryQuery{Kind: "users"}); err == nil {
				t.Fatal("closed store allowed")
			}
		})
	}
}
func TestDirectorySQLReadAndWriteFailures(t *testing.T) {
	s := mustOpen(t, t.TempDir(), SQLite, directorySample(t))
	b := s.disk.(*sqliteBackend)
	q := DirectoryQuery{Kind: "users", Filter: DirectoryFilter{Global: true}, Limit: 1}
	page, err := s.Directory(q)
	if err != nil {
		t.Fatal(err)
	}
	q.Cursor = page.NextCursor
	if _, err = b.db.Exec(`CREATE TRIGGER reject_members BEFORE DELETE ON group_members BEGIN SELECT RAISE(ABORT,'test'); END`); err != nil {
		t.Fatal(err)
	}
	if err = s.Update(func(st *State) error { st.Groups[0].MemberIDs = nil; st.Users[1].Username = "rejected"; return nil }); err == nil {
		t.Fatal("durable failure accepted")
	}
	if _, err = s.Directory(q); err != nil {
		t.Fatal("rollback invalidated cursor", err)
	}
	user, found, err := s.UserByUsername("reader")
	if err != nil || !found || user.Username != "reader" {
		t.Fatal("partial scalar commit", err)
	}
	if _, err = b.db.Exec(`DROP TABLE roles`); err != nil {
		t.Fatal(err)
	}
	page, err = s.Directory(DirectoryQuery{Kind: "users", Filter: DirectoryFilter{OrganizationID: "org-a"}, Limit: 2})
	if err == nil || len(page.State.Users) != 0 || page.NextCursor != "" || s.QueryReadFailures() != 1 {
		t.Fatal("partial projection published", err)
	}
}

func TestDirectoryConcurrentProjectionRevision(t *testing.T) {
	for _, kind := range []string{JSON, SQLite} {
		t.Run(kind, func(t *testing.T) {
			st := organizationSample(t)
			st.Users[1].Username = "revision-zero"
			st.Roles[0].Name = "revision-zero"
			s := mustOpen(t, t.TempDir(), kind, st)
			var wg sync.WaitGroup
			for i := 0; i < 4; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for j := 0; j < 30; j++ {
						page, err := s.Directory(DirectoryQuery{Kind: "users", Filter: DirectoryFilter{OrganizationID: "org-a"}, Limit: 2})
						if err != nil || len(page.State.Users) != 1 || len(page.State.Roles) != 1 || page.State.Users[0].Username != page.State.Roles[0].Name {
							t.Error("mixed directory revision", err)
							return
						}
					}
				}()
			}
			for i := 0; i < 10; i++ {
				name := fmt.Sprintf("revision-%d", i)
				if err := s.Update(func(st *State) error { st.Users[1].Username = name; st.Roles[0].Name = name; return nil }); err != nil {
					t.Fatal(err)
				}
			}
			wg.Wait()
		})
	}
}
