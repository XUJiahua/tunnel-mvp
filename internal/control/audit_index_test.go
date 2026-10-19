package control

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRetainedAuditIndexFiltersPaginationAndRotation(t *testing.T) {
	f := setup(t)
	f.s.auditMu.Lock()
	legacy := `{"time":"2024-01-01T00:00:00Z","event":"legacy_event","user":"former-operator","siteID":"old-site","resourceID":"old-resource"}` + "\n"
	if err := os.WriteFile(filepath.Join(f.dir, "audit.jsonl.2"), []byte(legacy), 0600); err != nil {
		t.Fatal(err)
	}
	// More than the previous 2 MiB tail, plus one oversized and one malformed
	// line. A bounded reader skips invalid records without losing later events.
	padding := strings.Repeat("{\"event\":\"padding\",\"value\":\""+strings.Repeat("p", 2048)+"\"}\n", 1100) + strings.Repeat("x", 140<<10) + "\nnot-json\n" + string([]byte{0xff, '\n'})
	if _, err := f.s.auditFile.WriteString(padding); err != nil {
		t.Fatal(err)
	}
	f.s.history.auditDirty.Store(true)
	f.s.auditMu.Unlock()
	res, b := f.request(t, "GET", "/api/audit?event=legacy_event&user=former-operator&siteID=old-site&resourceID=old-resource", nil, f.cookie, "")
	page := mustJSON[auditPage](t, b)
	if res.StatusCode != 200 || len(page.Entries) != 1 || page.IndexedFiles != 2 || page.Skipped != 3 {
		t.Fatal("retained file index incomplete", res.StatusCode, len(page.Entries), page.IndexedFiles, page.Skipped)
	}
	for i := 0; i < 5; i++ {
		f.s.audit("paged_event", "operator", "", "resource", map[string]any{"number": i})
	}
	res, b = f.request(t, "GET", "/api/audit?event=paged_event&limit=2", nil, f.cookie, "")
	first := mustJSON[auditPage](t, b)
	if len(first.Entries) != 2 || first.NextCursor == "" || first.Entries[0]["number"] != float64(4) {
		t.Fatal("first audit page incorrect")
	}
	res, b = f.request(t, "GET", "/api/audit?event=paged_event&limit=2&cursor="+first.NextCursor, nil, f.cookie, "")
	second := mustJSON[auditPage](t, b)
	if res.StatusCode != 200 || len(second.Entries) != 2 || second.Entries[0]["number"] != float64(2) {
		t.Fatal("cursor duplicated/skipped records")
	}
	res, b = f.request(t, "GET", "/api/audit?event=paged_event&limit=2&cursor="+second.NextCursor, nil, f.cookie, "")
	last := mustJSON[auditPage](t, b)
	if res.StatusCode != 200 || len(last.Entries) != 1 || last.NextCursor != "" {
		t.Fatal("last page incorrect")
	}
	f.s.cfg.AuditMaxMB = 1
	f.s.audit("forced_rotation", "admin", "", "")
	res, _ = f.request(t, "GET", "/api/audit?event=paged_event&limit=2&cursor="+first.NextCursor, nil, f.cookie, "")
	if res.StatusCode != 409 {
		t.Fatal("old cursor silently survived index rebuild")
	}
	res, b = f.request(t, "GET", "/api/audit?event=paged_event", nil, f.cookie, "")
	if res.StatusCode != 200 || len(mustJSON[auditPage](t, b).Entries) != 5 {
		t.Fatal("rotation lost retained records")
	}
	res, b = f.request(t, "GET", "/api/audit?event=legacy_event&since=2025-01-01T00:00:00Z", nil, f.cookie, "")
	if len(mustJSON[auditPage](t, b).Entries) != 0 {
		t.Fatal("audit timestamp filter failed")
	}
}

func TestAuditPageByteBudget(t *testing.T) {
	f := setup(t)
	for i := 0; i < 50; i++ {
		f.s.audit("large_event", "operator", "", "", map[string]any{"blob": strings.Repeat("x", 64<<10), "number": i})
	}
	res, b := f.request(t, "GET", "/api/audit?event=large_event&limit=500", nil, f.cookie, "")
	page := mustJSON[auditPage](t, b)
	if res.StatusCode != 200 || len(page.Entries) == 0 || len(page.Entries) >= 50 || page.NextCursor == "" || len(b) > (2<<20)+2048 {
		t.Fatal("audit page exceeded bounded response or lost pagination")
	}
	res, b = f.request(t, "GET", "/api/audit?event=large_event&limit=500&cursor="+page.NextCursor, nil, f.cookie, "")
	rest := mustJSON[auditPage](t, b)
	if res.StatusCode != 200 || len(rest.Entries)+len(page.Entries) != 50 {
		t.Fatal("byte budget skipped audit records")
	}
}
func TestAuditIndexDirtyRecoveryAndLegacyTenantPrivacy(t *testing.T) {
	f := setup(t)
	// Existing organization tests cover named tenants. Verify that missing owner
	// metadata is never interpreted as default-organization ownership.
	f.s.auditMu.Lock()
	_, err := f.s.auditFile.WriteString(`{"time":"2024-01-01T00:00:00Z","event":"private_legacy","user":"admin"}` + "\n")
	f.s.history.auditDirty.Store(true)
	f.s.auditMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}

	actor := User{ID: "reader", Role: "viewer", OrganizationID: "", Permissions: []string{"audit:read"}}
	f.s.audit("owned_event", "admin", "", "", map[string]any{"organizationID": ""})
	f.s.auditMu.Lock()
	err = f.s.rebuildAuditIndex()
	f.s.auditMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	page, err := f.s.history.queryAudit(actor, auditFilters{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range page.Entries {
		if entry["event"] == "private_legacy" {
			t.Fatal("legacy audit leaked to default tenant")
		}
	}
	if page.IndexedFiles != 0 || page.Skipped != 0 {
		t.Fatal("global index metadata leaked")
	}
	if _, err = f.s.history.db.Exec("CREATE TRIGGER reject_audit BEFORE INSERT ON audit_events BEGIN SELECT RAISE(ABORT,'injected'); END"); err != nil {
		t.Fatal(err)
	}
	f.s.audit("recoverable_event", "admin", "", "")
	if !f.s.history.auditDirty.Load() || f.s.stats.auditIndexErrors.Load() == 0 {
		t.Fatal("index failure was silent")
	}
	res, _ := f.request(t, "GET", "/api/audit", nil, f.cookie, "")
	if res.StatusCode != 503 {
		t.Fatal("dirty index returned incomplete success")
	}
	f.s.history.db.Exec("DROP TRIGGER reject_audit")
	res, b := f.request(t, "GET", "/api/audit?event=recoverable_event", nil, f.cookie, "")
	if res.StatusCode != 200 || len(mustJSON[auditPage](t, b).Entries) != 1 {
		t.Fatal("index did not recover from source logs")
	}
}
func TestAuditIndexV1Migration(t *testing.T) {
	dir := t.TempDir()
	h, err := openHistory(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{"DROP TABLE audit_events", "DROP TABLE audit_index_state", "PRAGMA user_version=1"} {
		if _, err = h.db.Exec(sql); err != nil {
			t.Fatal(err)
		}
	}
	h.close()
	h, err = openHistory(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer h.close()
	var version int
	h.db.QueryRow("PRAGMA user_version").Scan(&version)
	if version != 2 {
		t.Fatal("monitor schema not upgraded")
	}
	raw, _ := json.Marshal(map[string]any{"time": time.Now().UTC(), "event": "upgrade"})
	if err = h.indexAudit(raw); err != nil {
		t.Fatal(err)
	}
}
