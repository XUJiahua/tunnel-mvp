package state

import (
	"path/filepath"
	"reflect"
	"testing"
)

func TestAgentPolicyPersistenceAndV7Upgrade(t *testing.T) {
	for _, kind := range []string{JSON, SQLite} {
		t.Run(kind, func(t *testing.T) {
			s := agentSample(t)
			p := EmptyAgentPolicy()
			p.Allow = []NetworkRule{{CIDR: "10.0.0.0/8", Host: "app.internal", Ports: []int{80, 443}}}
			p.Version = PolicyVersion(p.Allow)
			s.Agents[0].Policy = &p
			dir := t.TempDir()
			r := mustOpen(t, dir, kind, s)
			r.Close()
			reopened := mustOpen(t, dir, kind, State{})
			if got := reopened.Snapshot(); !reflect.DeepEqual(s, got) {
				t.Fatal("policy lost")
			}
			reopened.Close()
			path := filepath.Join(t.TempDir(), "policy.json")
			if _, e := Export(dir, kind, path); e != nil {
				t.Fatal(e)
			}
			for _, target := range []string{JSON, SQLite} {
				dest := t.TempDir()
				if _, e := Restore(dest, target, path); e != nil {
					t.Fatal(e)
				}
				if !reflect.DeepEqual(s, mustOpen(t, dest, target, State{}).Snapshot()) {
					t.Fatal("remote policy cross-backend backup changed")
				}
			}
		})
	}
	dir := t.TempDir()
	s := agentSample(t)
	r := mustOpen(t, dir, SQLite, s)
	if _, e := r.disk.(*sqliteBackend).db.Exec("ALTER TABLE services DROP COLUMN connection_mode"); e != nil {
		t.Fatal(e)
	}
	if _, e := r.disk.(*sqliteBackend).db.Exec("PRAGMA user_version=7"); e != nil {
		t.Fatal(e)
	}
	r.Close()
	r = mustOpen(t, dir, SQLite, State{})
	if !reflect.DeepEqual(s, r.Snapshot()) {
		t.Fatal("v7 migration changed credential state")
	}
	r.Close()
	path := filepath.Join(t.TempDir(), "policy.json")
	if _, e := Export(dir, SQLite, path); e != nil {
		t.Fatal(e)
	}
	dest := t.TempDir()
	if _, e := Restore(dest, JSON, path); e != nil {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(s, mustOpen(t, dest, JSON, State{}).Snapshot()) {
		t.Fatal("policy backup changed state")
	}
}
