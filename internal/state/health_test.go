package state

import (
	"reflect"
	"testing"
)

func TestHealthValidationAndV5Upgrade(t *testing.T) {
	for _, svc := range []Service{{HealthPath: "../private"}, {HealthPath: "/ready"}, {HealthPath: "ready?q=1"}, {HealthPath: "ready%2fprivate"}, {HealthMethod: "POST"}, {HealthExpectedStatus: 199}, {HealthExpectedStatus: 600}} {
		if ValidateHealth(svc) == nil {
			t.Fatal("invalid health profile accepted", svc)
		}
	}
	dir := t.TempDir()
	initial := sample(t)
	r := mustOpen(t, dir, SQLite, initial)
	db := r.disk.(*sqliteBackend).db
	for _, sql := range []string{"ALTER TABLE services DROP COLUMN health_path", "ALTER TABLE services DROP COLUMN health_method", "ALTER TABLE services DROP COLUMN health_expected_status", "PRAGMA user_version=5"} {
		if _, err := db.Exec(sql); err != nil {
			t.Fatal(err)
		}
	}
	r.Close()
	if got := mustOpen(t, dir, SQLite, State{}).Snapshot(); !reflect.DeepEqual(got, initial) {
		t.Fatal("v5 upgrade changed application data")
	}
}
