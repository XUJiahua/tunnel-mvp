package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

func sample(t *testing.T) State {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte("storage-test-password"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	return State{
		Users:    []User{{ID: "admin", Username: "admin", PasswordHash: string(hash), Role: "admin", SiteIDs: []string{}, ServiceIDs: []string{}}, {ID: "reader", Username: "reader", PasswordHash: string(hash), Role: "viewer", SiteIDs: []string{}, ServiceIDs: []string{"platform"}}},
		Sites:    []Site{{ID: "site-a", Name: "上海站点", TokenHash: strings.Repeat("a", 64), CreatedAt: time.Date(2026, 10, 19, 0, 0, 0, 123, time.UTC)}},
		Services: []Service{{ID: "platform", Name: "平台", SiteID: "site-a", URL: "http://127.0.0.1:3000"}, {ID: "metrics", Name: "Grafana", SiteID: "site-a", URL: "https://grafana.internal"}},
	}
}
func mustOpen(t *testing.T, dir, kind string, s State) *Store {
	t.Helper()
	store, err := Open(dir, kind, func() (State, error) { return s, nil })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}
func TestRepositoryContract(t *testing.T) {
	for _, kind := range []string{JSON, SQLite} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			initial := sample(t)
			s := mustOpen(t, dir, kind, initial)
			snap := s.Snapshot()
			snap.Users[1].ServiceIDs[0] = "metrics"
			if s.Snapshot().Users[1].ServiceIDs[0] != "platform" {
				t.Fatal("snapshot aliases committed state")
			}
			rejected := errors.New("reject")
			if err := s.Update(func(next *State) error { next.Sites[0].Name = "bad"; return rejected }); !errors.Is(err, rejected) {
				t.Fatal(err)
			}
			if err := s.Update(func(next *State) error { next.Services[0].SiteID = "unknown"; return nil }); err == nil {
				t.Fatal("orphan accepted")
			}
			if !reflect.DeepEqual(s.Snapshot(), initial) {
				t.Fatal("failed update changed snapshot")
			}
			var retained *State
			if err := s.Update(func(next *State) error {
				next.Sites[0].Name = "new site"
				next.Users[1].ServiceIDs = []string{"metrics", "metrics"}
				retained = next
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			want := s.Snapshot()
			retained.Users[1].ServiceIDs[0] = "platform"
			if !reflect.DeepEqual(s.Snapshot(), want) {
				t.Fatal("callback retained a mutable reference")
			}
			if len(want.Users[1].ServiceIDs) != 1 {
				t.Fatal("duplicate grants not normalized")
			}
			name, _ := Filename(kind)
			info, err := os.Stat(filepath.Join(dir, name))
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0600 {
				t.Fatalf("private mode: %v", info.Mode())
			}
			if err = s.Close(); err != nil {
				t.Fatal(err)
			}
			if err = s.Close(); err != nil {
				t.Fatal("Close not idempotent")
			}
			if err = s.Update(func(*State) error { return nil }); err == nil {
				t.Fatal("closed store accepted update")
			}
			reopened, err := Open(dir, kind, func() (State, error) { t.Fatal("existing state reinitialized"); return State{}, nil })
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			if !reflect.DeepEqual(reopened.Snapshot(), want) {
				t.Fatalf("state changed across reopen: %#v", reopened.Snapshot())
			}
		})
	}
}
func TestSQLiteCommitFailureRollsBackDiskAndMemory(t *testing.T) {
	dir := t.TempDir()
	original := sample(t)
	s := mustOpen(t, dir, SQLite, original)
	db := s.disk.(*sqliteBackend).db
	if _, err := db.Exec(`CREATE TRIGGER reject_write BEFORE INSERT ON sites BEGIN SELECT RAISE(ABORT,'injected failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := s.Update(func(next *State) error { next.Sites[0].Name = "must not commit"; return nil }); err == nil {
		t.Fatal("injected failure succeeded")
	}
	if !reflect.DeepEqual(s.Snapshot(), original) {
		t.Fatal("failed commit changed read model")
	}
	onDisk, err := s.disk.(*sqliteBackend).load()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(onDisk, original) {
		t.Fatal("transaction did not roll back deleted rows")
	}
}
func TestJSONWriteFailureKeepsCommittedState(t *testing.T) {
	dir := t.TempDir()
	original := sample(t)
	s := mustOpen(t, dir, JSON, original)
	disk := s.disk.(*jsonBackend)
	path := disk.path
	disk.path = filepath.Join(dir, "missing", "state.json")
	if err := s.Update(func(next *State) error { next.Sites[0].Name = "bad"; return nil }); err == nil {
		t.Fatal("invalid path wrote successfully")
	}
	disk.path = path
	if !reflect.DeepEqual(s.Snapshot(), original) {
		t.Fatal("write failure changed memory")
	}
	onDisk, err := readLegacy(path)
	if err != nil || !reflect.DeepEqual(onDisk, original) {
		t.Fatal("write failure changed original file", err)
	}
}
func TestConcurrentUpdatesAreSerialized(t *testing.T) {
	for _, kind := range []string{JSON, SQLite} {
		t.Run(kind, func(t *testing.T) {
			s := mustOpen(t, t.TempDir(), kind, sample(t))
			var wg sync.WaitGroup
			for i := range 12 {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					err := s.Update(func(next *State) error {
						next.Services = append(next.Services, Service{ID: fmt.Sprintf("extra-%d", i), Name: "extra", SiteID: "site-a", URL: "http://127.0.0.1:3000"})
						return nil
					})
					if err != nil {
						t.Error(err)
					}
				}(i)
			}
			wg.Wait()
			if len(s.Snapshot().Services) != 14 {
				t.Fatal("concurrent update lost state")
			}
		})
	}
}
func TestMigrationPreservesSourceAndAllGrants(t *testing.T) {
	dir := t.TempDir()
	s := mustOpen(t, dir, JSON, sample(t))
	want := s.Snapshot()
	if _, err := MigrateJSON(dir); err == nil {
		t.Fatal("migration allowed while Server owns directory")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	summary, err := MigrateJSON(dir)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Storage != SQLite || summary.Users != 2 {
		t.Fatal(summary)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "state.json"))
	if string(raw) != string(original) {
		t.Fatal("migration changed JSON source")
	}
	if _, err = MigrateJSON(dir); err == nil {
		t.Fatal("migration overwrote SQLite")
	}
	if _, err = Open(dir, JSON, nil); err == nil {
		t.Fatal("default JSON silently used stale source after migration")
	}
	migrated, err := Open(dir, SQLite, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()
	if !reflect.DeepEqual(migrated.Snapshot(), want) {
		t.Fatal("migration changed IDs, credential hashes or grants")
	}
}
func TestBackupRestoreAcrossBackends(t *testing.T) {
	for _, from := range []string{JSON, SQLite} {
		for _, to := range []string{JSON, SQLite} {
			t.Run(from+"-to-"+to, func(t *testing.T) {
				dir := t.TempDir()
				original := sample(t)
				s := mustOpen(t, dir, from, original)
				_ = s.Close()
				path := filepath.Join(t.TempDir(), "backup.json")
				if _, err := Export(dir, from, path); err != nil {
					t.Fatal(err)
				}
				before, _ := os.ReadFile(path)
				if _, err := Export(dir, from, path); err == nil {
					t.Fatal("backup overwrite allowed")
				}
				after, _ := os.ReadFile(path)
				if string(before) != string(after) {
					t.Fatal("failed export changed backup")
				}
				info, _ := os.Stat(path)
				if info.Mode().Perm() != 0600 {
					t.Fatal("backup is not private")
				}
				if _, err := ValidateBackup(path); err != nil {
					t.Fatal(err)
				}
				restoredDir := filepath.Join(t.TempDir(), "restored")
				if _, err := Restore(restoredDir, to, path); err != nil {
					t.Fatal(err)
				}
				restored, err := Open(restoredDir, to, nil)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(restored.Snapshot(), original) {
					t.Fatal("restored snapshot differs")
				}
				_ = restored.Close()
				if _, err = Restore(restoredDir, to, path); err == nil {
					t.Fatal("restore overwrote existing state")
				}
			})
		}
	}
}
func TestCorruptAndUnknownFormatsNeverInitializeOrRestore(t *testing.T) {
	initial := sample(t)
	for _, kind := range []string{JSON, SQLite} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			name, _ := Filename(kind)
			path := filepath.Join(dir, name)
			if err := os.WriteFile(path, []byte("corrupt"), 0600); err != nil {
				t.Fatal(err)
			}
			called := false
			if _, err := Open(dir, kind, func() (State, error) { called = true; return initial, nil }); err == nil {
				t.Fatal("corruption accepted")
			}
			if called {
				t.Fatal("corrupt state reinitialized")
			}
			raw, _ := os.ReadFile(path)
			if string(raw) != "corrupt" {
				t.Fatal("corrupt source changed")
			}
		})
	}
	dir := t.TempDir()
	s := mustOpen(t, dir, SQLite, initial)
	if _, err := s.disk.(*sqliteBackend).db.Exec("PRAGMA user_version=999"); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	if _, err := Open(dir, SQLite, nil); err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatal("future schema accepted", err)
	}
	source := t.TempDir()
	j := mustOpen(t, source, JSON, initial)
	_ = j.Close()
	path := filepath.Join(t.TempDir(), "backup.json")
	if _, err := Export(source, JSON, path); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	var b Backup
	if err := json.Unmarshal(raw, &b); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"checksum", "version", "orphan"} {
		t.Run(mode, func(t *testing.T) {
			next := b
			next.State = Clone(b.State)
			switch mode {
			case "checksum":
				next.State.Sites[0].Name = "tampered"
			case "version":
				next.Version = 999
			case "orphan":
				next.State.Users[1].ServiceIDs = []string{"absent"}
				next.SHA256 = checksum(next.State)
			}
			invalid := filepath.Join(t.TempDir(), "invalid.json")
			data, _ := json.Marshal(next)
			_ = os.WriteFile(invalid, data, 0600)
			target := filepath.Join(t.TempDir(), "must-not-exist")
			if _, err := Restore(target, SQLite, invalid); err == nil {
				t.Fatal("invalid backup restored")
			}
			if _, err := os.Stat(target); !os.IsNotExist(err) {
				t.Fatal("invalid restore touched destination")
			}
		})
	}
}
func TestDirectoryLockHelper(t *testing.T) {
	dir := os.Getenv("TUNNEL_LOCK_TEST_DIR")
	if dir == "" {
		return
	}
	_, err := Inspect(dir, os.Getenv("TUNNEL_LOCK_TEST_KIND"))
	if os.Getenv("TUNNEL_LOCK_TEST_EXPECT") == "locked" {
		if err == nil || !strings.Contains(err.Error(), "in use") {
			t.Fatalf("expected lock rejection, got %v", err)
		}
	} else if err != nil {
		t.Fatal(err)
	}
}
func TestDirectoryLockAcrossProcesses(t *testing.T) {
	for _, kind := range []string{JSON, SQLite} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			s := mustOpen(t, dir, kind, sample(t))
			if _, err := Open(dir, kind, nil); err == nil {
				t.Fatal("second writer allowed")
			}
			for _, expect := range []string{"locked", "free"} {
				if expect == "free" {
					_ = s.Close()
				}
				cmd := exec.Command(os.Args[0], "-test.run=^TestDirectoryLockHelper$")
				cmd.Env = append(os.Environ(), "TUNNEL_LOCK_TEST_DIR="+dir, "TUNNEL_LOCK_TEST_KIND="+kind, "TUNNEL_LOCK_TEST_EXPECT="+expect)
				if raw, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("helper %s: %v %s", expect, err, raw)
				}
			}
		})
	}
}

func TestDatabasePathsAndOrphanedSidecars(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "含空格 #? ' database")
	s := mustOpen(t, dir, SQLite, sample(t))
	_ = s.Close()
	if _, err := Inspect(dir, SQLite); err != nil {
		t.Fatal("SQLite URI path escaped incorrectly", err)
	}
	orphanDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(orphanDir, "state.db-journal"), []byte("orphan"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(orphanDir, SQLite, func() (State, error) { return sample(t), nil }); err == nil {
		t.Fatal("orphaned journal silently ignored")
	}
	source := t.TempDir()
	sourceStore := mustOpen(t, source, JSON, sample(t))
	_ = sourceStore.Close()
	backup := filepath.Join(t.TempDir(), "backup.json")
	if _, err := Export(source, JSON, backup); err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(orphanDir, SQLite, backup); err == nil {
		t.Fatal("restore accepted orphaned journal")
	}
	if _, err := os.Stat(filepath.Join(orphanDir, "state.db")); !os.IsNotExist(err) {
		t.Fatal("restore published a database beside orphaned journal")
	}
	if _, err := Open(source, SQLite, nil); err == nil {
		t.Fatal("SQLite initialized beside unmigrated JSON")
	}
}

func TestLegacyJSONWithoutApplicationGrantField(t *testing.T) {
	dir := t.TempDir()
	old := sample(t)
	old.Users[1].SiteIDs = []string{"site-a"}
	old.Users[1].ServiceIDs = nil
	raw, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	var legacy map[string]any
	if err = json.Unmarshal(raw, &legacy); err != nil {
		t.Fatal(err)
	}
	for _, user := range legacy["users"].([]any) {
		delete(user.(map[string]any), "serviceIDs")
	}
	raw, err = json.MarshalIndent(legacy, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "state.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = MigrateJSON(dir); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir, SQLite, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	reader := s.Snapshot().Users[1]
	if len(reader.SiteIDs) != 1 || reader.SiteIDs[0] != "site-a" || reader.ServiceIDs == nil || len(reader.ServiceIDs) != 0 {
		t.Fatal("legacy grant semantics changed")
	}
	original, _ := os.ReadFile(filepath.Join(dir, "state.json"))
	if string(original) != string(raw) {
		t.Fatal("legacy JSON was rewritten")
	}
}
