package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestIdentityPersistenceAndBackup(t *testing.T) {
	for _, kind := range []string{JSON, SQLite} {
		t.Run(kind, func(t *testing.T) {
			initial := sample(t)
			initial.Users[1].PasswordHash = ""
			initial.Users[1].Identity = &Identity{Issuer: "https://idp.example/realms/tunnel", Subject: "opaque-ID"}
			initial.Users[1].Disabled = true
			dir := t.TempDir()
			s := mustOpen(t, dir, kind, initial)
			_ = s.Close()
			reopened := mustOpen(t, dir, kind, State{})
			if !reflect.DeepEqual(reopened.Snapshot(), initial) {
				t.Fatal("lost identity or disabled flag")
			}
			_ = reopened.Close()
			backup := filepath.Join(t.TempDir(), "identity.json")
			if _, err := Export(dir, kind, backup); err != nil {
				t.Fatal(err)
			}
			raw, _ := os.ReadFile(backup)
			var b Backup
			_ = json.Unmarshal(raw, &b)
			if b.Version != 4 {
				t.Fatal("identity backup must be versioned against old readers")
			}
			for _, target := range []string{JSON, SQLite} {
				dest := t.TempDir()
				if _, err := Restore(dest, target, backup); err != nil {
					t.Fatal(err)
				}
				restored := mustOpen(t, dest, target, State{})
				if !reflect.DeepEqual(restored.Snapshot(), initial) {
					t.Fatal("restore changed identity")
				}
			}
		})
	}
}
func TestIdentityValidationAndRecoveryAdministrator(t *testing.T) {
	for _, mode := range []string{"no-method", "duplicate", "empty-subject", "http", "disabled-admin", "oidc-only-admin"} {
		t.Run(mode, func(t *testing.T) {
			s := sample(t)
			switch mode {
			case "no-method":
				s.Users[1].PasswordHash = ""
			case "duplicate":
				identity := &Identity{"https://idp.example", "same"}
				s.Users[0].Identity = identity
				s.Users[1].Identity = identity
			case "empty-subject":
				s.Users[1].Identity = &Identity{"https://idp.example", ""}
			case "http":
				s.Users[1].Identity = &Identity{"http://idp.example", "sub"}
			case "disabled-admin":
				s.Users[0].Disabled = true
			case "oidc-only-admin":
				s.Users[0].PasswordHash = ""
				s.Users[0].Identity = &Identity{"https://idp.example", "sub"}
			}
			if err := Validate(&s); err == nil {
				t.Fatalf("accepted %s", mode)
			}
		})
	}
}
func legacySQLite(t *testing.T, dir string, invalid bool) State {
	t.Helper()
	initial := sample(t)
	s := mustOpen(t, dir, SQLite, initial)
	db := s.disk.(*sqliteBackend).db
	// Produce the actual v1 table layout and version, not just a changed pragma.
	for _, statement := range []string{"DROP TABLE roles", "DROP TABLE organizations", "ALTER TABLE users DROP COLUMN organization_id", "ALTER TABLE users DROP COLUMN role_id", "ALTER TABLE sites DROP COLUMN organization_id", "ALTER TABLE services DROP COLUMN organization_id", "DROP TABLE groups", "DROP TABLE invites", "DROP INDEX users_identity", "ALTER TABLE users DROP COLUMN disabled", "ALTER TABLE users DROP COLUMN oidc_issuer", "ALTER TABLE users DROP COLUMN oidc_subject", "PRAGMA user_version=1"} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if invalid {
		if _, err := db.Exec("UPDATE users SET password_hash='invalid' WHERE id='reader'"); err != nil {
			t.Fatal(err)
		}
	}
	_ = s.Close()
	return initial
}
func TestSQLiteV1UpgradeAndValidationRollback(t *testing.T) {
	t.Run("upgrade", func(t *testing.T) {
		dir := t.TempDir()
		initial := legacySQLite(t, dir, false)
		s := mustOpen(t, dir, SQLite, State{})
		if !reflect.DeepEqual(s.Snapshot(), initial) {
			t.Fatal("upgrade changed existing data")
		}
		var version int
		if err := s.disk.(*sqliteBackend).db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != SchemaVersion {
			t.Fatal("not upgraded", version, err)
		}
		if err := s.Update(func(next *State) error {
			next.Users[1].Identity = &Identity{"https://idp.example", "reader"}
			next.Users[1].Disabled = true
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("rollback", func(t *testing.T) {
		dir := t.TempDir()
		legacySQLite(t, dir, true)
		if s, err := Open(dir, SQLite, nil); err == nil {
			s.Close()
			t.Fatal("invalid legacy state upgraded")
		}
		b, err := connectSQLite(filepath.Join(dir, "state.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer b.close()
		var version int
		if err = b.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 1 {
			t.Fatal("failed upgrade changed schema", version, err)
		}
		if _, err = b.db.Query("SELECT disabled FROM users"); err == nil {
			t.Fatal("failed upgrade retained added columns")
		}
	})
}
func TestReadOriginalV1Backup(t *testing.T) {
	initial := sample(t)
	// New optional fields are omitted, preserving canonical bytes/checksum of v1.
	raw, err := json.Marshal(Backup{"tunnel-state", 1, time.Now().UTC(), checksum(initial), initial})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "v1.json")
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	restored, err := loadBackup(path)
	if err != nil || !reflect.DeepEqual(initial, restored) {
		t.Fatal("legacy backup rejected", err)
	}
}
