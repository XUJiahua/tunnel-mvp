package state

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestSQLiteSparseUpdatePreservesUnchangedRows(t *testing.T) {
	s := agentSample(t)
	for i := 0; i < 40; i++ {
		u := s.Users[1]
		u.ID = fmt.Sprintf("reader-%d", i)
		u.Username = u.ID
		s.Users = append(s.Users, u)
	}
	r := mustOpen(t, t.TempDir(), SQLite, s)
	db := r.disk.(*sqliteBackend).db
	if _, e := db.Exec("CREATE TABLE write_log(table_name TEXT,row_id TEXT,operation TEXT)"); e != nil {
		t.Fatal(e)
	}
	for _, table := range []string{"users", "sites", "services", "agents", "user_sites", "user_services"} {
		id := "id"
		if strings.HasPrefix(table, "user_") {
			id = "user_id"
		}
		for _, op := range []string{"INSERT", "DELETE", "UPDATE"} {
			value := "NEW"
			if op == "DELETE" {
				value = "OLD"
			}
			q := fmt.Sprintf("CREATE TRIGGER log_%s_%s AFTER %s ON %s BEGIN INSERT INTO write_log VALUES('%s',%s.%s,'%s'); END", table, op, op, table, table, value, id, op)
			if _, e := db.Exec(q); e != nil {
				t.Fatal(e)
			}
		}
	}
	if e := r.Update(func(next *State) error { next.Sites[0].Name = "renamed only"; return nil }); e != nil {
		t.Fatal(e)
	}
	var changed, foreign int
	if e := db.QueryRow("SELECT count(*),sum(CASE WHEN table_name!='sites' OR row_id!='site-a' THEN 1 ELSE 0 END) FROM write_log").Scan(&changed, &foreign); e != nil {
		t.Fatal(e)
	}
	if changed != 2 || foreign != 0 {
		t.Fatalf("sparse site update rewrote unrelated rows: %d changes, %d unrelated", changed, foreign)
	}
	db.Exec("DELETE FROM write_log")
	if e := r.Update(func(next *State) error { return nil }); e != nil {
		t.Fatal(e)
	}
	db.QueryRow("SELECT count(*) FROM write_log").Scan(&changed)
	if changed != 0 {
		t.Fatal("no-op update wrote rows")
	}
	if e := r.Update(func(next *State) error { next.Users[1].ServiceIDs = []string{"metrics"}; return nil }); e != nil {
		t.Fatal(e)
	}
	db.QueryRow("SELECT count(*) FROM write_log WHERE table_name != 'user_services' OR row_id!='reader'").Scan(&foreign)
	if foreign != 0 {
		t.Fatal("grant update rewrote unrelated scalar rows")
	}
}
func TestSQLiteAtomicSwapsReorderAndReferenceDeletion(t *testing.T) {
	s := agentSample(t)
	r := mustOpen(t, t.TempDir(), SQLite, s)
	if e := r.Update(func(next *State) error {
		next.Users[0].Username, next.Users[1].Username = next.Users[1].Username, next.Users[0].Username
		next.Users[0], next.Users[1] = next.Users[1], next.Users[0]
		next.Services[0], next.Services[1] = next.Services[1], next.Services[0]
		return nil
	}); e != nil {
		t.Fatal(e)
	}
	wanted := r.Snapshot()
	got, e := r.disk.(*sqliteBackend).load()
	if e != nil || !reflect.DeepEqual(wanted, got) {
		t.Fatal("swapped rows did not roundtrip", e)
	}
	if e = r.Update(func(next *State) error {
		next.Users[0].ServiceIDs = []string{}
		next.Services = []Service{}
		next.Sites = []Site{}
		next.Agents = nil
		return nil
	}); e != nil {
		t.Fatal(e)
	}
	got, e = r.disk.(*sqliteBackend).load()
	if e != nil || !reflect.DeepEqual(r.Snapshot(), got) {
		t.Fatal("parent/reference removal failed", e)
	}
}
func TestSQLiteDeltaCacheRetainedAfterFailure(t *testing.T) {
	r := mustOpen(t, t.TempDir(), SQLite, sample(t))
	db := r.disk.(*sqliteBackend).db
	before := r.Snapshot()
	db.Exec("CREATE TRIGGER reject_delta BEFORE INSERT ON sites BEGIN SELECT RAISE(ABORT,'injected failure'); END")
	if e := r.Update(func(next *State) error { next.Sites[0].Name = "rejected"; return nil }); e == nil {
		t.Fatal("injected transaction succeeded")
	}
	wantRows, _ := stateRows(before)
	if !reflect.DeepEqual(r.disk.(*sqliteBackend).rows, wantRows) || !reflect.DeepEqual(r.disk.(*sqliteBackend).grants, grantRows(before.Users)) {
		t.Fatal("failed transaction advanced delta cache")
	}
	db.Exec("DROP TRIGGER reject_delta")
	if e := r.Update(func(next *State) error { next.Sites[0].Name = "committed"; return nil }); e != nil {
		t.Fatal(e)
	}
	got, e := r.disk.(*sqliteBackend).load()
	if e != nil || !reflect.DeepEqual(r.Snapshot(), got) {
		t.Fatal("retry after rollback lost changes", e)
	}
}

func TestSQLiteDeltaCacheDoesNotAliasRetainedCallback(t *testing.T) {
	r := mustOpen(t, t.TempDir(), SQLite, sample(t))
	var retained *State
	if e := r.Update(func(next *State) error { next.Users[1].ServiceIDs = []string{"metrics"}; retained = next; return nil }); e != nil {
		t.Fatal(e)
	}
	retained.Users[1].ServiceIDs[0] = "platform"
	retained.Sites[0].Name = "uncommitted alias"
	if e := r.Update(func(next *State) error { next.Sites[0].Name = "next committed update"; return nil }); e != nil {
		t.Fatal(e)
	}
	got, e := r.disk.(*sqliteBackend).load()
	if e != nil || !reflect.DeepEqual(got, r.Snapshot()) || got.Users[1].ServiceIDs[0] != "metrics" {
		t.Fatal("retained callback corrupted delta cache", e)
	}
}
