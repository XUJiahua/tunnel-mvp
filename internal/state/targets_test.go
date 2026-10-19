package state

import (
	"path/filepath"
	"reflect"
	"testing"
)

func TestApplicationURLPaths(t *testing.T) {
	for _, raw := range []string{"http://internal/grafana", "https://internal/team/app/", "http://internal", "http://internal/"} {
		if _, err := ApplicationURL(raw); err != nil {
			t.Fatal(err)
		}
	}
	for _, raw := range []string{"http://user:pass@internal/app/", "http://internal/app?q=1", "http://internal/app#x", "http://internal/../app/", "http://internal/app//path/", "http://internal/%2e/app", "http://internal/a%2fb/", "http://internal/a\\b/", "http://internal/_tunnel/entry/", "http://internal/应用/"} {
		if _, err := ApplicationURL(raw); err == nil {
			t.Fatal("ambiguous application URL accepted", raw)
		}
	}
}
func TestMountedTargetsAndDomainsPersistence(t *testing.T) {
	for _, kind := range []string{JSON, SQLite} {
		t.Run(kind, func(t *testing.T) {
			initial := sample(t)
			initial.Services[0].URL += "/grafana/"
			initial.Services[0].CustomDomain = "reports.customer.test"
			dir := t.TempDir()
			r := mustOpen(t, dir, kind, initial)
			r.Close()
			r = mustOpen(t, dir, kind, State{})
			if !reflect.DeepEqual(r.Snapshot(), initial) {
				t.Fatal("mounted target/domain lost")
			}
			r.Close()
			backup := filepath.Join(t.TempDir(), "mounted.json")
			if _, err := Export(dir, kind, backup); err != nil {
				t.Fatal(err)
			}
			dest := t.TempDir()
			if _, err := Restore(dest, SQLite, backup); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(mustOpen(t, dest, SQLite, State{}).Snapshot(), initial) {
				t.Fatal("backup changed mount/domain")
			}
		})
	}
}
func TestCustomDomainUniquenessAndV4Upgrade(t *testing.T) {
	s := sample(t)
	s.Services[0].CustomDomain = "same.customer.test"
	s.Services[1].CustomDomain = s.Services[0].CustomDomain
	if Validate(&s) == nil {
		t.Fatal("duplicate custom domain accepted")
	}
	dir := t.TempDir()
	initial := sample(t)
	r := mustOpen(t, dir, SQLite, initial)
	db := r.disk.(*sqliteBackend).db
	for _, sql := range []string{"DROP TABLE agents", "ALTER TABLE sites DROP COLUMN legacy_disabled", "ALTER TABLE services DROP COLUMN health_path", "ALTER TABLE services DROP COLUMN health_method", "ALTER TABLE services DROP COLUMN health_expected_status", "ALTER TABLE services DROP COLUMN custom_domain", "PRAGMA user_version=4"} {
		if _, err := db.Exec(sql); err != nil {
			t.Fatal(err)
		}
	}
	r.Close()
	got := mustOpen(t, dir, SQLite, State{})
	if !reflect.DeepEqual(got.Snapshot(), initial) {
		t.Fatal("v4 upgrade changed targets")
	}
}
