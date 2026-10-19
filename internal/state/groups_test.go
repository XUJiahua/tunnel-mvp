package state

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func grouped(t *testing.T) State {
	s := sample(t)
	now := time.Now().UTC()
	s.Groups = []Group{{ID: "ops", Name: "Operators", MemberIDs: []string{"reader"}, SiteIDs: []string{}, ServiceIDs: []string{"platform"}}}
	s.Invites = []Invite{{ID: "invite", Username: "invited", TokenHash: strings.Repeat("c", 64), CreatedAt: now, ExpiresAt: now.Add(time.Hour), SiteIDs: []string{}, ServiceIDs: []string{}, GroupIDs: []string{"ops"}}}
	return s
}
func TestGroupsInvitationsPersistenceAndRestore(t *testing.T) {
	for _, kind := range []string{JSON, SQLite} {
		t.Run(kind, func(t *testing.T) {
			initial := grouped(t)
			dir := t.TempDir()
			s := mustOpen(t, dir, kind, initial)
			_ = s.Close()
			s = mustOpen(t, dir, kind, State{})
			if !reflect.DeepEqual(s.Snapshot(), initial) {
				t.Fatal("group/invite lost on restart")
			}
			_ = s.Close()
			backup := filepath.Join(t.TempDir(), "backup.json")
			if _, err := Export(dir, kind, backup); err != nil {
				t.Fatal(err)
			}
			for _, target := range []string{JSON, SQLite} {
				dest := t.TempDir()
				if _, err := Restore(dest, target, backup); err != nil {
					t.Fatal(err)
				}
				r := mustOpen(t, dest, target, State{})
				restored := r.Snapshot()
				if len(restored.Invites) != 0 {
					t.Fatal("restore resurrected an invitation token")
				}
				restored.Invites = initial.Invites
				if !reflect.DeepEqual(restored, initial) {
					t.Fatal("restore changed groups or members")
				}
			}
		})
	}
}
func TestGroupAndInviteReferenceValidation(t *testing.T) {
	for _, mode := range []string{"member", "site", "group", "username", "token", "expiry"} {
		t.Run(mode, func(t *testing.T) {
			s := grouped(t)
			switch mode {
			case "member":
				s.Groups[0].MemberIDs = []string{"missing"}
			case "site":
				s.Groups[0].SiteIDs = []string{"missing"}
			case "group":
				s.Invites[0].GroupIDs = []string{"missing"}
			case "username":
				s.Invites[0].Username = "admin"
			case "token":
				s.Invites[0].TokenHash = "raw"
			case "expiry":
				s.Invites[0].ExpiresAt = s.Invites[0].CreatedAt.Add(8 * 24 * time.Hour)
			}
			if Validate(&s) == nil {
				t.Fatal("invalid model accepted", mode)
			}
		})
	}
}
func TestSQLiteV2GroupUpgradeAndRollback(t *testing.T) {
	for _, invalid := range []bool{false, true} {
		t.Run(map[bool]string{false: "upgrade", true: "rollback"}[invalid], func(t *testing.T) {
			dir := t.TempDir()
			initial := sample(t)
			s := mustOpen(t, dir, SQLite, initial)
			db := s.disk.(*sqliteBackend).db
			for _, sql := range []string{"DROP TABLE roles", "DROP TABLE organizations", "ALTER TABLE users DROP COLUMN organization_id", "ALTER TABLE users DROP COLUMN role_id", "ALTER TABLE sites DROP COLUMN organization_id", "ALTER TABLE services DROP COLUMN organization_id", "DROP TABLE groups", "DROP TABLE invites", "PRAGMA user_version=2"} {
				if _, err := db.Exec(sql); err != nil {
					t.Fatal(err)
				}
			}
			if invalid {
				if _, err := db.Exec("UPDATE users SET password_hash='bad' WHERE id='reader'"); err != nil {
					t.Fatal(err)
				}
			}
			_ = s.Close()
			reopened, err := Open(dir, SQLite, nil)
			if invalid {
				if err == nil {
					reopened.Close()
					t.Fatal("invalid legacy upgraded")
				}
				b, err := connectSQLite(filepath.Join(dir, "state.db"))
				if err != nil {
					t.Fatal(err)
				}
				defer b.close()
				var version int
				_ = b.db.QueryRow("PRAGMA user_version").Scan(&version)
				if version != 2 {
					t.Fatal("partial upgrade committed")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			if !reflect.DeepEqual(reopened.Snapshot(), initial) {
				t.Fatal("upgrade changed original data")
			}
		})
	}
}
func TestGroupPayloadMismatchRejected(t *testing.T) {
	dir := t.TempDir()
	s := mustOpen(t, dir, SQLite, grouped(t))
	db := s.disk.(*sqliteBackend).db
	if _, err := db.Exec(`UPDATE groups SET payload='{"id":"wrong","name":"wrong","memberIDs":[],"siteIDs":[],"serviceIDs":[]}'`); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	if reopened, err := Open(dir, SQLite, nil); err == nil {
		reopened.Close()
		t.Fatal("SQL identity mismatch accepted")
	}
	// The database still exists and is never reinitialized after a load error.
	if _, err := os.Stat(filepath.Join(dir, "state.db")); err != nil {
		t.Fatal(err)
	}
}
