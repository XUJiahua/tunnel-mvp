package state

import (
	"path/filepath"
	"reflect"
	"testing"
)

func TestConnectionModeStorageBackupAndV8Upgrade(t *testing.T) {
	for _, kind := range []string{JSON, SQLite} {
		t.Run(kind, func(t *testing.T) {
			s := sample(t)
			s.Services[0].ConnectionMode = "session"
			dir := t.TempDir()
			r := mustOpen(t, dir, kind, s)
			r.Close()
			r = mustOpen(t, dir, kind, State{})
			if !reflect.DeepEqual(s, r.Snapshot()) {
				t.Fatal("connection mode lost")
			}
			r.Close()
			backup := filepath.Join(t.TempDir(), "backup.json")
			if _, e := Export(dir, kind, backup); e != nil {
				t.Fatal(e)
			}
			for _, target := range []string{JSON, SQLite} {
				dest := t.TempDir()
				if _, e := Restore(dest, target, backup); e != nil {
					t.Fatal(e)
				}
				if !reflect.DeepEqual(s, mustOpen(t, dest, target, State{}).Snapshot()) {
					t.Fatal("mode backup lost")
				}
			}
		})
	}
	dir := t.TempDir()
	s := sample(t)
	r := mustOpen(t, dir, SQLite, s)
	for _, q := range []string{"ALTER TABLE services DROP COLUMN connection_mode", "PRAGMA user_version=8"} {
		if _, e := r.disk.(*sqliteBackend).db.Exec(q); e != nil {
			t.Fatal(e)
		}
	}
	r.Close()
	if !reflect.DeepEqual(s, mustOpen(t, dir, SQLite, State{}).Snapshot()) {
		t.Fatal("v8 upgrade changed legacy state")
	}
	s.Services[0].ConnectionMode = "unknown"
	if Validate(&s) == nil {
		t.Fatal("invalid mode accepted")
	}
}
