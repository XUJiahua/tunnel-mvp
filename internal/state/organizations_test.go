package state

import (
	"path/filepath"
	"reflect"
	"testing"
)

func organizationSample(t *testing.T) State {
	s := grouped(t)
	s.Organizations = []Organization{{ID: "org-a", Name: "Organization A"}, {ID: "org-b", Name: "Organization B"}}
	s.Roles = []Role{{ID: "operator", Name: "Operator", OrganizationID: "org-a", Permissions: []string{"applications:manage", "applications:access"}}}
	s.Users[1].OrganizationID = "org-a"
	s.Users[1].RoleID = "operator"
	s.Sites[0].OrganizationID = "org-a"
	for i := range s.Services {
		s.Services[i].OrganizationID = "org-a"
	}
	s.Groups[0].OrganizationID = "org-a"
	s.Invites[0].OrganizationID = "org-a"
	return s
}
func TestOrganizationPersistenceAndBackup(t *testing.T) {
	for _, kind := range []string{JSON, SQLite} {
		t.Run(kind, func(t *testing.T) {
			initial := organizationSample(t)
			dir := t.TempDir()
			r := mustOpen(t, dir, kind, initial)
			r.Close()
			r = mustOpen(t, dir, kind, State{})
			if !reflect.DeepEqual(r.Snapshot(), initial) {
				t.Fatal("organization ownership/role lost")
			}
			r.Close()
			backup := filepath.Join(t.TempDir(), "org.json")
			if _, err := Export(dir, kind, backup); err != nil {
				t.Fatal(err)
			}
			for _, target := range []string{JSON, SQLite} {
				dest := t.TempDir()
				if _, err := Restore(dest, target, backup); err != nil {
					t.Fatal(err)
				}
				got := mustOpen(t, dest, target, State{}).Snapshot()
				got.Invites = initial.Invites
				if !reflect.DeepEqual(got, initial) {
					t.Fatal("backup changed ownership/roles")
				}
			}
		})
	}
}
func TestOrganizationReferenceValidation(t *testing.T) {
	for _, mode := range []string{"unknown-owner", "foreign-site", "foreign-app-grant", "foreign-member", "foreign-invite", "foreign-role", "root-owner", "unknown-permission"} {
		t.Run(mode, func(t *testing.T) {
			s := organizationSample(t)
			switch mode {
			case "unknown-owner":
				s.Sites[0].OrganizationID = "missing"
			case "foreign-site":
				s.Services[0].OrganizationID = "org-b"
			case "foreign-app-grant":
				s.Users[1].OrganizationID = "org-b"
				s.Users[1].RoleID = ""
			case "foreign-member":
				s.Users[1].OrganizationID = "org-b"
				s.Users[1].RoleID = ""
				s.Users[1].ServiceIDs = nil
			case "foreign-invite":
				s.Invites[0].OrganizationID = "org-b"
			case "foreign-role":
				s.Roles[0].OrganizationID = "org-b"
			case "root-owner":
				s.Users[0].OrganizationID = "org-a"
			case "unknown-permission":
				s.Roles[0].Permissions = []string{"global:admin"}
			}
			if Validate(&s) == nil {
				t.Fatal("cross-boundary state accepted", mode)
			}
		})
	}
}
func TestSQLiteV3OrganizationUpgradeRollback(t *testing.T) {
	for _, invalid := range []bool{false, true} {
		t.Run(map[bool]string{false: "upgrade", true: "rollback"}[invalid], func(t *testing.T) {
			initial := grouped(t)
			dir := t.TempDir()
			r := mustOpen(t, dir, SQLite, initial)
			db := r.disk.(*sqliteBackend).db
			for _, sql := range []string{"ALTER TABLE services DROP COLUMN custom_domain", "DROP TABLE roles", "DROP TABLE organizations", "ALTER TABLE users DROP COLUMN organization_id", "ALTER TABLE users DROP COLUMN role_id", "ALTER TABLE sites DROP COLUMN organization_id", "ALTER TABLE services DROP COLUMN organization_id", "PRAGMA user_version=3"} {
				if _, err := db.Exec(sql); err != nil {
					t.Fatal(err)
				}
			}
			if invalid {
				if _, err := db.Exec("UPDATE users SET password_hash='bad' WHERE id='reader'"); err != nil {
					t.Fatal(err)
				}
			}
			r.Close()
			got, err := Open(dir, SQLite, nil)
			if invalid {
				if err == nil {
					got.Close()
					t.Fatal("invalid legacy upgraded")
				}
				b, e := connectSQLite(filepath.Join(dir, "state.db"))
				if e != nil {
					t.Fatal(e)
				}
				defer b.close()
				var v int
				b.db.QueryRow("PRAGMA user_version").Scan(&v)
				if v != 3 {
					t.Fatal("schema partially upgraded")
				}
				if rows, e := b.db.Query("SELECT organization_id FROM users"); e == nil {
					rows.Close()
					t.Fatal("added ownership columns survived rollback")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer got.Close()
			if !reflect.DeepEqual(got.Snapshot(), initial) {
				t.Fatal("legacy default ownership changed")
			}
		})
	}
}
