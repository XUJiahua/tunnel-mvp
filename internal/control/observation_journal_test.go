package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func journalAudit(t *testing.T, h *historyStore, event string) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"event": event, "time": time.Now().UTC().Format(time.RFC3339Nano), "eventID": digest(randomID()), "nodeID": h.observer.Node, "nodeEpoch": strings.Repeat("a", 32), "sourceID": h.observer.Source})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
func TestObservationJournalCommitFailureRecoveryAndCapturedAcknowledgment(t *testing.T) {
	f := setup(t)
	h := f.s.history
	ctx := context.Background()
	if err := h.enableObservations("node-a", 16<<20); err != nil {
		t.Fatal(err)
	}
	initial, err := h.pendingObservations(ctx)
	if err != nil || len(initial) == 0 {
		t.Fatal(err)
	}
	if err = h.acknowledgeObservations(ctx, initial[len(initial)-1].Sequence); err != nil {
		t.Fatal(err)
	}
	raw := journalAudit(t, h, "queue_failure")
	if _, err = f.s.auditFile.Write(append(raw, '\n')); err != nil {
		t.Fatal(err)
	}
	if err = f.s.auditFile.Sync(); err != nil {
		t.Fatal(err)
	}
	// A real SQLite trigger aborts the queue write after the audit index insert.
	if _, err = h.db.Exec(`CREATE TRIGGER fail_queue BEFORE INSERT ON observation_queue BEGIN SELECT RAISE(ABORT,'injected queue failure');END`); err != nil {
		t.Fatal(err)
	}
	if err = h.indexAudit(raw); err == nil {
		t.Fatal("queue failure reported success")
	}
	var indexed int
	if err = h.db.QueryRow(`SELECT count(*) FROM audit_events WHERE event='queue_failure'`).Scan(&indexed); err != nil || indexed != 0 {
		t.Fatal("index committed without queue", indexed, err)
	}
	if _, err = h.db.Exec(`DROP TRIGGER fail_queue`); err != nil {
		t.Fatal(err)
	}
	f.s.auditMu.Lock()
	err = f.s.rebuildAuditIndex()
	f.s.auditMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if err = h.reconcileObservations(); err != nil {
		t.Fatal(err)
	}
	first, err := h.pendingObservations(ctx)
	if err != nil || len(first) != 1 || first[0].Record.Kind != "audit" {
		t.Fatal("retained JSONL was not repaired", first, err)
	}
	second := journalAudit(t, h, "after_capture")
	if err = h.indexAudit(second); err != nil {
		t.Fatal(err)
	}
	if err = h.acknowledgeObservations(ctx, first[0].Sequence); err != nil {
		t.Fatal(err)
	}
	pending, err := h.pendingObservations(ctx)
	if err != nil || len(pending) != 1 || !strings.Contains(string(pending[0].Record.Payload), "after_capture") {
		t.Fatal("ack deleted concurrent append", pending, err)
	}
	if err = h.reconcileObservations(); err != nil {
		t.Fatal(err)
	}
	pending, err = h.pendingObservations(ctx)
	if err != nil || len(pending) != 1 {
		t.Fatal("repair requeued acknowledged history", pending, err)
	}
	backlog, err := h.observationBacklog(ctx)
	if err != nil || backlog.Count != 1 || backlog.Bytes <= 0 {
		t.Fatal(backlog, err)
	}
	if err = h.acknowledgeObservations(ctx, pending[0].Sequence); err != nil {
		t.Fatal(err)
	}
	backlog, err = h.observationBacklog(ctx)
	if err != nil || backlog.Count != 0 || backlog.Bytes != 0 || backlog.Oldest != nil {
		t.Fatal("byte counter/oldest drift", backlog, err)
	}
}
func TestObservationJournalReopenLegacyDuplicatesAndBufferAtomicity(t *testing.T) {
	f := setup(t)
	h := f.s.history
	ctx := context.Background()
	raw := []byte(`{"time":"2026-10-19T00:00:00Z","event":"equal_legacy"}`)
	for range 2 {
		if _, err := f.s.auditFile.Write(append(raw, '\n')); err != nil {
			t.Fatal(err)
		}
	}
	f.s.auditMu.Lock()
	err := f.s.rebuildAuditIndex()
	f.s.auditMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if err = h.enableObservations("node-a", 16<<20); err != nil {
		t.Fatal(err)
	}
	source := h.observer.Source
	pending, err := h.pendingObservations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	legacy := 0
	for _, v := range pending {
		if v.Record.Kind == "audit" && strings.Contains(string(v.Record.Payload), "equal_legacy") {
			legacy++
			if ids[v.Record.ID] {
				t.Fatal("identical legacy lines shared ID")
			}
			ids[v.Record.ID] = true
		}
	}
	if legacy != 2 {
		t.Fatal("legacy rows omitted", legacy)
	}
	svc := Service{ID: "app", SiteID: "site", URL: "http://127.0.0.1"}
	report := probeReport{CheckedAt: time.Now().UTC(), Reachable: true, Healthy: true, Status: 200, NodeEpoch: strings.Repeat("a", 32)}
	h.observer.MaxBytes = 1
	if err = h.save(svc, report); !errors.Is(err, errObservationBuffer) {
		t.Fatal("buffer cap ignored", err)
	}
	samples, err := h.samples(svc, time.Now().Add(-time.Hour), 10)
	if err != nil || len(samples) != 0 {
		t.Fatal("sample committed without durable queue", samples, err)
	}
	h.observer.MaxBytes = 16 << 20
	if err = h.save(svc, report); err != nil {
		t.Fatal(err)
	}
	h.close()
	reopened, err := openHistory(f.s.cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	f.s.history = reopened
	if err = reopened.enableObservations("node-a", 16<<20); err != nil {
		t.Fatal(err)
	}
	if reopened.observer.Source != source {
		t.Fatal("restart changed source identity")
	}
	var baseVersion int
	if err = reopened.db.QueryRow(`PRAGMA user_version`).Scan(&baseVersion); err != nil || baseVersion != 3 {
		t.Fatal("base monitor compatibility changed", baseVersion, err)
	}
	if err = reopened.enableObservations("node-b", 16<<20); err == nil {
		t.Fatal("copied data silently changed node identity")
	}
	var sampleIDs int
	if err = reopened.db.QueryRow(`SELECT count(DISTINCT event_id) FROM observation_sample_identity`).Scan(&sampleIDs); err != nil || sampleIDs != 1 {
		t.Fatal("restart changed sample identity", sampleIDs, err)
	}
	if _, err = reopened.db.Exec(`UPDATE observation_local SET version=99`); err != nil {
		t.Fatal(err)
	}
	if err = reopened.enableObservations("node-a", 16<<20); err == nil {
		t.Fatal("future extension silently accepted")
	}
}
func TestObservationJournalPagedRepairLargerThanBuffer(t *testing.T) {
	f := setup(t)
	h := f.s.history
	ctx := context.Background()
	if err := h.enableObservations("node-a", 16<<20); err != nil {
		t.Fatal(err)
	}
	pending, err := h.pendingObservations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = h.acknowledgeObservations(ctx, pending[len(pending)-1].Sequence); err != nil {
		t.Fatal(err)
	}
	// Retained history exceeds the deliberately small active buffer. A repair
	// page must make progress across publication cycles instead of rolling back
	// every previously repaired row when a later page fills the queue.
	for i := range 25 {
		raw := journalAudit(t, h, fmt.Sprintf("repair-%d", i))
		if i == 0 {
			record, e := h.observer.auditRecord(raw)
			if e != nil {
				t.Fatal(e)
			}
			encoded, e := json.Marshal(record)
			if e != nil {
				t.Fatal(e)
			}
			h.observer.MaxBytes = int64(len(encoded)) * 11
		}
		values, ok := auditValues(raw)
		if !ok {
			t.Fatal("fixture")
		}
		if _, err = h.db.Exec(insertAuditSQL, append(values, h.auditSource.Load())...); err != nil {
			t.Fatal(err)
		}
	}
	h.auditReconcile.Store(true)
	seen := map[string]bool{}
	for i := 0; i < 5 && h.auditReconcile.Load(); i++ {
		if err = h.reconcileObservations(); err != nil {
			t.Fatal(err)
		}
		pending, err = h.pendingObservations(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range pending {
			if seen[v.Record.ID] {
				t.Fatal("repair repeated acknowledged record")
			}
			seen[v.Record.ID] = true
		}
		if len(pending) > 0 {
			if err = h.acknowledgeObservations(ctx, pending[len(pending)-1].Sequence); err != nil {
				t.Fatal(err)
			}
		}
	}
	if len(seen) != 25 || h.auditReconcile.Load() {
		t.Fatal("repair failed to finish bounded pages", len(seen))
	}
}
