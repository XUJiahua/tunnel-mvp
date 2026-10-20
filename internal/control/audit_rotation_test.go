package control

import (
	"database/sql"
	"encoding/json"
	"github.com/XUJiahua/tunnel-mvp/internal/state"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIncrementalAuditRotationPreservesRetainedRows(t *testing.T) {
	f := setup(t)
	f.s.cfg.AuditMaxMB = 1
	f.s.cfg.AuditBackups = 2
	f.s.audit("retained_marker", "admin", "", "", map[string]any{"number": 1})
	var originalID int64
	f.s.history.db.QueryRow("SELECT id FROM audit_events WHERE event='retained_marker'").Scan(&originalID)
	// If rotation rebuilds, either trigger would reject rewriting the retained marker.
	for _, q := range []string{`CREATE TRIGGER retained_no_delete BEFORE DELETE ON audit_events WHEN OLD.event='retained_marker' BEGIN SELECT RAISE(ABORT,'retained event rewritten'); END`, `CREATE TRIGGER retained_no_insert BEFORE INSERT ON audit_events WHEN NEW.event='retained_marker' BEGIN SELECT RAISE(ABORT,'retained event reparsed'); END`} {
		if _, e := f.s.history.db.Exec(q); e != nil {
			t.Fatal(e)
		}
	}
	for i := 0; i < 17; i++ {
		f.s.audit("padding", "admin", "", "", map[string]any{"blob": strings.Repeat("x", 64<<10)})
	}
	f.s.audit("after_rotation", "admin", "", "")
	if f.s.history.auditDirty.Load() {
		t.Fatal("healthy rotation attempted a rebuild or failed incrementally")
	}
	var keptID int64
	f.s.history.db.QueryRow("SELECT id FROM audit_events WHERE event='retained_marker'").Scan(&keptID)
	if keptID != originalID {
		t.Fatal("rotation reassigned retained event ID")
	}
	var files int
	f.s.history.db.QueryRow("SELECT file_count FROM audit_index_state").Scan(&files)
	if files != 2 {
		t.Fatal("rotation did not add active source", files)
	}
	f.s.history.db.Exec("DROP TRIGGER retained_no_delete")
	f.s.history.db.Exec("DROP TRIGGER retained_no_insert")
	for i := 0; i < 36; i++ {
		f.s.audit("padding", "admin", "", "", map[string]any{"blob": strings.Repeat("x", 64<<10)})
	}
	var count int
	f.s.history.db.QueryRow("SELECT count(*) FROM audit_events WHERE event='retained_marker'").Scan(&count)
	if count != 0 {
		t.Fatal("expired source remained indexed")
	}
	f.s.history.db.QueryRow("SELECT file_count FROM audit_index_state").Scan(&files)
	if files != 3 {
		t.Fatal("retention file count incorrect", files)
	}
}
func TestAuditSchemaV2UpgradeWithExistingEvents(t *testing.T) {
	dir := t.TempDir()
	h, e := openHistory(dir)
	if e != nil {
		t.Fatal(e)
	}
	for _, q := range []string{"DROP TABLE audit_events", "DROP TABLE audit_sources", auditSchema[0], "PRAGMA user_version=2"} {
		if _, e = h.db.Exec(q); e != nil {
			t.Fatal(e)
		}
	}
	raw, _ := json.Marshal(map[string]any{"event": "v2-retained"})
	values, _ := auditValues(raw)
	if _, e = h.db.Exec("INSERT INTO audit_events(organization_id,occurred_at,event,username,site_id,resource_id,payload) VALUES(?,?,?,?,?,?,?)", values...); e != nil {
		t.Fatal(e)
	}
	h.close()
	h, e = openHistory(dir)
	if e != nil {
		t.Fatal(e)
	}
	defer h.close()
	var count int
	if e = h.db.QueryRow("SELECT count(*) FROM audit_events WHERE event='v2-retained' AND source_id=1").Scan(&count); e != nil || count != 1 {
		t.Fatal("v2 events lost during schema upgrade", e, count)
	}
}

func TestMonitorV3ArchiveRejectsOrphanSource(t *testing.T) {
	f := setup(t)
	f.s.Close()
	valid := filepath.Join(t.TempDir(), "valid.tar.gz")
	if _, e := state.ExportOperations(f.dir, valid); e != nil {
		t.Fatal("valid v3 archive rejected", e)
	}
	db, e := sql.Open("sqlite", filepath.Join(f.dir, "monitor.db"))
	if e != nil {
		t.Fatal(e)
	}
	if _, e = db.Exec("UPDATE audit_events SET source_id=99999"); e != nil {
		t.Fatal(e)
	}
	db.Close()
	invalid := filepath.Join(t.TempDir(), "invalid.tar.gz")
	if _, e = state.ExportOperations(f.dir, invalid); e == nil {
		t.Fatal("archive accepted orphan source")
	}
	if _, e = os.Stat(invalid); !os.IsNotExist(e) {
		t.Fatal("invalid archive published")
	}
}
