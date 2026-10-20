package control

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/XUJiahua/tunnel-mvp/internal/state"
)

var errObservationBuffer = errors.New("observation buffer is full; retained JSONL remains the recovery source")

type observationJournal struct {
	Source, Node string
	MaxBytes     int64
}
type queuedObservation struct {
	Sequence int64
	Record   state.Observation
}
type observationBacklog struct {
	Count  int64      `json:"pending"`
	Bytes  int64      `json:"bytes"`
	Oldest *time.Time `json:"oldest,omitempty"`
	Errors uint64     `json:"enqueueErrors"`
}

// This is an independent additive extension. Base monitor schema v3 and its
// archive remain readable by HA-3. Queue rows never reference rotating sources.
func (h *historyStore) enableObservations(node string, maxBytes int64) error {
	tx, err := h.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS observation_local(singleton INTEGER PRIMARY KEY CHECK(singleton=1),version INTEGER NOT NULL,source TEXT NOT NULL,node_id TEXT NOT NULL,buffered_bytes INTEGER NOT NULL DEFAULT 0,enqueue_errors INTEGER NOT NULL DEFAULT 0)`,
		`CREATE TABLE IF NOT EXISTS observation_queue(seq INTEGER PRIMARY KEY AUTOINCREMENT,event_id TEXT NOT NULL UNIQUE,record TEXT NOT NULL CHECK(json_valid(record)),queued_at TEXT NOT NULL)`,
		`CREATE TRIGGER IF NOT EXISTS observation_queue_insert AFTER INSERT ON observation_queue BEGIN UPDATE observation_local SET buffered_bytes=buffered_bytes+length(CAST(new.record AS BLOB)) WHERE singleton=1; END`,
		`CREATE TRIGGER IF NOT EXISTS observation_queue_delete AFTER DELETE ON observation_queue BEGIN UPDATE observation_local SET buffered_bytes=buffered_bytes-length(CAST(old.record AS BLOB)) WHERE singleton=1; END`,
		`CREATE INDEX IF NOT EXISTS observation_audit_event ON audit_events(json_extract(payload,'$.eventID'))`,
		`CREATE TABLE IF NOT EXISTS observation_audit_identity(event_id TEXT PRIMARY KEY)`,
		`CREATE TABLE IF NOT EXISTS observation_sample_identity(sample_id INTEGER PRIMARY KEY REFERENCES samples(id) ON DELETE CASCADE,event_id TEXT NOT NULL,epoch TEXT NOT NULL)`,
	} {
		if _, err = tx.Exec(q); err != nil {
			return err
		}
	}
	source := randomID()
	if _, err = tx.Exec(`INSERT OR IGNORE INTO observation_local(singleton,version,source,node_id) VALUES(1,1,?,?)`, source, node); err != nil {
		return err
	}
	var version int
	var bound string
	if err = tx.QueryRow(`SELECT version,source,node_id FROM observation_local WHERE singleton=1`).Scan(&version, &source, &bound); err != nil {
		return err
	}
	sourceBytes, sourceError := hex.DecodeString(source)
	if version != state.ObservationVersion || bound != node || sourceError != nil || len(sourceBytes) != 24 || source != strings.ToLower(source) {
		return fmt.Errorf("observation extension version or node identity mismatch")
	}
	j := &observationJournal{Source: source, Node: node, MaxBytes: maxBytes}
	// Legacy records have no immutable event identity. Each complete import is a
	// named snapshot, including duplicate equal lines. Only its completion marker
	// makes it visible, and a new snapshot invalidates old pagination explicitly.
	snapshot := randomID()
	var after int64
	for {
		rows, err := tx.Query(`SELECT id,payload FROM audit_events WHERE id>? ORDER BY id LIMIT 100`, after)
		if err != nil {
			return err
		}
		type audit struct {
			id  int64
			raw []byte
		}
		page := []audit{}
		for rows.Next() {
			var v audit
			if err = rows.Scan(&v.id, &v.raw); err != nil {
				break
			}
			page = append(page, v)
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return err
		}
		if len(page) == 0 {
			break
		}
		for _, v := range page {
			record, e := j.auditRecord(v.raw)
			if e != nil {
				return e
			}
			if record.ID == "" {
				record.ID = digest(source + snapshot + strconv.FormatInt(v.id, 10))
				record.LegacySnapshot = snapshot
			}
			if err = j.enqueue(tx, record); err != nil {
				return err
			}
			after = v.id
		}
	}
	end := state.Observation{ID: digest(source + snapshot + "complete"), Source: source, NodeID: node, Kind: "legacy_end", LegacySnapshot: snapshot, At: time.Now().UTC(), Payload: json.RawMessage(`{}`)}
	if err = j.enqueue(tx, end); err != nil {
		return err
	}
	rows, err := tx.Query(`SELECT s.id,s.service_id,s.profile,s.checked_at,s.reachable,s.healthy,s.status,s.latency_ms,s.request_id,s.code,s.stage,coalesce(i.event_id,''),coalesce(i.epoch,'') FROM samples s LEFT JOIN observation_sample_identity i ON i.sample_id=s.id ORDER BY s.id`)
	if err != nil {
		return err
	}
	type sample struct {
		id                             int64
		service, profile, event, epoch string
		p                              healthSample
	}
	samples := []sample{}
	for rows.Next() {
		var v sample
		var at string
		if err = rows.Scan(&v.id, &v.service, &v.profile, &at, &v.p.Reachable, &v.p.Healthy, &v.p.Status, &v.p.LatencyMs, &v.p.RequestID, &v.p.Code, &v.p.Stage, &v.event, &v.epoch); err != nil {
			rows.Close()
			return err
		}
		v.p.CheckedAt, err = time.Parse(time.RFC3339Nano, at)
		if err != nil {
			rows.Close()
			return err
		}
		samples = append(samples, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, v := range samples {
		if v.event == "" {
			v.event = digest(source + "sample" + strconv.FormatInt(v.id, 10) + v.p.CheckedAt.UTC().Format(historyTimeLayout) + v.p.RequestID)
			if _, err = tx.Exec(`INSERT INTO observation_sample_identity VALUES(?,?,?)`, v.id, v.event, v.epoch); err != nil {
				return err
			}
		}
		if err = j.enqueueSample(tx, v.event, v.epoch, v.service, v.profile, v.p); err != nil {
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	h.observer = j
	return nil
}
func (j *observationJournal) auditRecord(raw []byte) (state.Observation, error) {
	var entry map[string]any
	if err := json.Unmarshal(raw, &entry); err != nil {
		return state.Observation{}, err
	}
	if entry == nil {
		return state.Observation{}, fmt.Errorf("invalid audit object")
	}
	id, _ := entry["eventID"].(string)
	epoch, _ := entry["nodeEpoch"].(string)
	if id != "" {
		rawID, err := hex.DecodeString(id)
		if err != nil || len(rawID) != 32 || id != strings.ToLower(id) {
			return state.Observation{}, fmt.Errorf("invalid audit event identity")
		}
		if entry["sourceID"] != j.Source || entry["nodeID"] != j.Node {
			return state.Observation{}, fmt.Errorf("audit provenance mismatch")
		}
		rawEpoch, err := hex.DecodeString(epoch)
		if err != nil || len(rawEpoch) != 16 || epoch != strings.ToLower(epoch) {
			return state.Observation{}, fmt.Errorf("invalid audit node epoch")
		}
	} else {
		epoch = ""
	}

	at := time.Unix(0, 0).UTC()
	if text, ok := entry["time"].(string); ok {
		if value, err := time.Parse(time.RFC3339Nano, text); err == nil {
			at = value
		}
	}
	var org *string
	if value, ok := entry["organizationID"].(string); ok {
		org = &value
	}
	return state.Observation{ID: id, Source: j.Source, NodeID: j.Node, Epoch: epoch, Kind: "audit", OrganizationID: org, At: at, Payload: json.RawMessage(raw)}, nil
}
func (j *observationJournal) enqueue(tx *sql.Tx, o state.Observation) error {
	if o.Kind == "audit" && o.LegacySnapshot == "" {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO observation_audit_identity VALUES(?)`, o.ID); err != nil {
			return err
		}
	}

	var exists bool
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM observation_queue WHERE event_id=?)`, o.ID).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return nil
	}
	raw, err := json.Marshal(o)
	if err != nil {
		return err
	}
	if len(raw) > 1<<20 {
		return fmt.Errorf("oversized observation")
	}
	var used int64
	if err = tx.QueryRow(`SELECT buffered_bytes FROM observation_local WHERE singleton=1`).Scan(&used); err != nil {
		return err
	}
	if used+int64(len(raw)) > j.MaxBytes {
		return errObservationBuffer
	}
	_, err = tx.Exec(`INSERT INTO observation_queue(event_id,record,queued_at) VALUES(?,?,?)`, o.ID, string(raw), time.Now().UTC().Format(historyTimeLayout))
	return err
}
func (j *observationJournal) enqueueSample(tx *sql.Tx, id, epoch, service, profile string, p healthSample) error {
	p.EventID, p.NodeID, p.NodeEpoch = id, j.Node, epoch
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return j.enqueue(tx, state.Observation{ID: id, Source: j.Source, NodeID: j.Node, Epoch: epoch, Kind: "sample", ServiceID: service, Profile: profile, At: p.CheckedAt, Payload: raw})
}
func (h *historyStore) pendingObservations(ctx context.Context) ([]queuedObservation, error) {
	rows, err := h.db.QueryContext(ctx, `SELECT seq,record FROM observation_queue ORDER BY seq LIMIT 100`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []queuedObservation{}
	size := 0
	for rows.Next() {
		var q queuedObservation
		var raw []byte
		if err = rows.Scan(&q.Sequence, &raw); err != nil {
			return nil, err
		}
		if len(out) > 0 && size+len(raw) > 256<<10 {
			break
		}
		if err = json.Unmarshal(raw, &q.Record); err != nil {
			return nil, err
		}
		out = append(out, q)
		size += len(raw)
	}
	return out, rows.Err()
}
func (h *historyStore) acknowledgeObservations(ctx context.Context, sequence int64) error {
	_, err := h.db.ExecContext(ctx, `DELETE FROM observation_queue WHERE seq<=?`, sequence)
	return err
}
func (h *historyStore) observationBacklog(ctx context.Context) (observationBacklog, error) {
	var out observationBacklog
	var oldest sql.NullString
	err := h.db.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM observation_queue),buffered_bytes,enqueue_errors,(SELECT min(queued_at) FROM observation_queue) FROM observation_local WHERE singleton=1`).Scan(&out.Count, &out.Bytes, &out.Errors, &oldest)
	if err != nil {
		return out, err
	}
	if oldest.Valid {
		at, e := time.Parse(time.RFC3339Nano, oldest.String)
		if e != nil {
			return out, e
		}
		out.Oldest = &at
	}
	return out, nil
}
func (h *historyStore) observationError() {
	if h.observer != nil {
		_, _ = h.db.Exec(`UPDATE observation_local SET enqueue_errors=enqueue_errors+1 WHERE singleton=1`)
	}
}

// Called after a failed index/queue transaction was repaired from JSONL. New
// events keep their original IDs, so replay after an ambiguous commit is safe.
func (h *historyStore) reconcileObservations() error {
	if h.observer == nil {
		return nil
	}
	tx, err := h.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT payload FROM audit_events WHERE json_type(payload,'$.eventID')='text' AND NOT EXISTS(SELECT 1 FROM observation_audit_identity i WHERE i.event_id=json_extract(audit_events.payload,'$.eventID')) ORDER BY id LIMIT 10`)
	if err != nil {
		return err
	}
	raws := [][]byte{}
	for rows.Next() {
		var raw []byte
		if err = rows.Scan(&raw); err != nil {
			break
		}
		raws = append(raws, raw)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	for _, raw := range raws {
		o, e := h.observer.auditRecord(raw)
		if e != nil {
			return e
		}
		if err = h.observer.enqueue(tx, o); err != nil {
			return err
		}
	}
	if len(raws) < 10 {
		if _, err = tx.Exec(`DELETE FROM observation_audit_identity WHERE NOT EXISTS(SELECT 1 FROM audit_events a WHERE json_extract(a.payload,'$.eventID')=observation_audit_identity.event_id)`); err != nil {
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	h.auditReconcile.Store(len(raws) == 10)
	return nil
}
