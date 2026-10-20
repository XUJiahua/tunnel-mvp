package state

import (
	"path/filepath"
	"reflect"
	"testing"
)

func TestRoleDelegationPermissionAndV9Upgrade(t *testing.T) {
	s := organizationSample(t)
	dir := t.TempDir()
	r := mustOpen(t, dir, SQLite, s)
	if _, e := r.disk.(*sqliteBackend).db.Exec("PRAGMA user_version=9"); e != nil {
		t.Fatal(e)
	}
	r.Close()
	r = mustOpen(t, dir, SQLite, State{})
	if !reflect.DeepEqual(s, r.Snapshot()) {
		t.Fatal("v9 migration changed role authority")
	}
	if e := r.Update(func(st *State) error {
		st.Roles[0].Permissions = append(st.Roles[0].Permissions, "roles:manage")
		return nil
	}); e != nil {
		t.Fatal(e)
	}
	s = r.Snapshot()
	r.Close()
	path := filepath.Join(t.TempDir(), "roles.json")
	if _, e := Export(dir, SQLite, path); e != nil {
		t.Fatal(e)
	}
	for _, kind := range []string{JSON, SQLite} {
		dest := t.TempDir()
		if _, e := Restore(dest, kind, path); e != nil {
			t.Fatal(e)
		}
		got := mustOpen(t, dest, kind, State{}).Snapshot()
		got.Invites = s.Invites
		if !reflect.DeepEqual(s, got) {
			t.Fatal("delegation authority changed on restore")
		}
	}
}
