package state

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestLoginPointQueriesAndCommittedIdentity(t *testing.T) {
	for _, kind := range []string{JSON, SQLite} {
		t.Run(kind, func(t *testing.T) {
			st := organizationSample(t)
			identity := Identity{Issuer: "https://id.example.com/realm", Subject: "subject"}
			st.Users[1].Identity = &identity
			s := mustOpen(t, t.TempDir(), kind, st)
			got, found, err := s.UserByUsername("reader")
			if err != nil || !found || !reflect.DeepEqual(got, st.Users[1]) {
				t.Fatal("username projection differs", err)
			}
			got.Identity.Subject = "mutated"
			got.ServiceIDs[0] = "metrics"
			got, found, err = s.UserByIdentity(identity)
			if err != nil || !found || got.Identity.Subject != "subject" || got.ServiceIDs[0] != "platform" {
				t.Fatal("identity projection aliases", err)
			}
			for _, username := range []string{"", "Reader", "reader' OR 1=1 --"} {
				if _, found, err = s.UserByUsername(username); err != nil || found {
					t.Fatal("unexpected username match", err)
				}
			}
			for _, id := range []Identity{{}, {Issuer: identity.Issuer, Subject: "SUBJECT"}, {Issuer: "https://other.example.com", Subject: identity.Subject}} {
				if _, found, err = s.UserByIdentity(id); err != nil || found {
					t.Fatal("unexpected identity match", err)
				}
			}
			fresh := Identity{Issuer: identity.Issuer, Subject: "new-subject"}
			if err = s.Update(func(st *State) error {
				st.Users[1].Username = "renamed"
				st.Users[1].Identity = &fresh
				st.Users[1].Disabled = true
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if _, found, err = s.UserByUsername("reader"); err != nil || found {
				t.Fatal("old name persisted", err)
			}
			if _, found, err = s.UserByIdentity(identity); err != nil || found {
				t.Fatal("old identity persisted", err)
			}
			got, found, err = s.UserByIdentity(fresh)
			if err != nil || !found || !got.Disabled || got.Username != "renamed" {
				t.Fatal("committed account missing", err)
			}
			s.Close()
			if _, found, err = s.UserByUsername("renamed"); err == nil || found {
				t.Fatal("closed read allowed")
			}
		})
	}
}
func TestMembershipIndexTransactionAndRebuild(t *testing.T) {
	dir := t.TempDir()
	st := organizationSample(t)
	s := mustOpen(t, dir, SQLite, st)
	b := s.disk.(*sqliteBackend)
	// Unrelated writes must not rewrite the derived membership rows.
	if _, err := b.db.Exec(`CREATE TRIGGER reject_members BEFORE DELETE ON group_members BEGIN SELECT RAISE(ABORT,'test'); END`); err != nil {
		t.Fatal(err)
	}
	if err := s.Update(func(st *State) error { st.Sites[0].Name = "renamed"; return nil }); err != nil {
		t.Fatal("unrelated write rewrote members", err)
	}
	if err := s.Update(func(st *State) error { st.Groups[0].MemberIDs = nil; return nil }); err == nil {
		t.Fatal("membership failure accepted")
	}
	got, found, err := s.UserContext("reader")
	if err != nil || !found || len(got.Groups) != 1 {
		t.Fatal("failed transaction lost membership", err)
	}
	if _, err = b.db.Exec(`DROP TRIGGER reject_members`); err != nil {
		t.Fatal(err)
	}
	if err = s.Update(func(st *State) error { st.Groups[0].MemberIDs = nil; return nil }); err != nil {
		t.Fatal("rollback poisoned delta cache", err)
	}
	got, found, err = s.UserContext("reader")
	if err != nil || !found || len(got.Groups) != 0 {
		t.Fatal("membership removal not committed", err)
	}
	// A forged extra derived row cannot manufacture a canonical grant.
	if _, err = b.db.Exec(`INSERT INTO group_members VALUES('ops','reader',0)`); err != nil {
		t.Fatal(err)
	}
	if _, found, err = s.UserContext("reader"); err == nil || found {
		t.Fatal("forged membership authorized")
	}
	s.Close()
	s = mustOpen(t, dir, SQLite, State{})
	got, found, err = s.UserContext("reader")
	if err != nil || !found || len(got.Groups) != 0 {
		t.Fatal("index not rebuilt", err)
	}
	if err = s.Update(func(st *State) error { st.Groups[0].MemberIDs = []string{"reader"}; return nil }); err != nil {
		t.Fatal(err)
	}
	b = s.disk.(*sqliteBackend)
	if _, err = b.db.Exec(`DROP TABLE group_members`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s = mustOpen(t, dir, SQLite, State{})
	got, found, err = s.UserContext("reader")
	if err != nil || !found || len(got.Groups) != 1 {
		t.Fatal("missing derived index not rebuilt", err)
	}
	s.Close()
	backup := filepath.Join(t.TempDir(), "backup.json")
	if _, err = Export(dir, SQLite, backup); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{JSON, SQLite} {
		dest := t.TempDir()
		if _, err = Restore(dest, kind, backup); err != nil {
			t.Fatal(err)
		}
		restored := mustOpen(t, dest, kind, State{})
		got, found, err = restored.UserContext("reader")
		if err != nil || !found || len(got.Groups) != 1 {
			t.Fatal("restore lost indexed group", err)
		}
	}
}
func TestLoginAndMembershipQueryPlans(t *testing.T) {
	s := mustOpen(t, t.TempDir(), SQLite, organizationSample(t))
	b := s.disk.(*sqliteBackend)
	for _, sql := range []string{`SELECT id FROM users WHERE username='reader'`, `SELECT id FROM users WHERE oidc_issuer='issuer' AND oidc_subject='subject' AND oidc_issuer!=''`, `SELECT g.payload FROM group_members m JOIN groups g ON g.id=m.group_id WHERE m.user_id='reader'`} {
		rows, err := b.db.Query("EXPLAIN QUERY PLAN " + sql)
		if err != nil {
			t.Fatal(err)
		}
		var details []string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err = rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			details = append(details, detail)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			t.Fatal(err)
		}
		plan := strings.Join(details, "\n")
		if strings.Contains(plan, "SCAN ") || !strings.Contains(plan, "SEARCH ") {
			t.Fatal("point read scans tables", plan)
		}
	}
	b.db.Close()
	if _, found, err := s.UserByUsername("reader"); err == nil || found || s.QueryReadFailures() != 1 {
		t.Fatal("failed login read returned user", err)
	}
}
