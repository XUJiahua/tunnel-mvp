package control

import (
	"bufio"
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

var errAuditIndexChanged = errors.New("audit index changed after rotation; query again from newest")
var auditSchema = []string{
	`CREATE TABLE audit_events (id INTEGER PRIMARY KEY,organization_id TEXT,occurred_at TEXT NOT NULL,event TEXT NOT NULL,username TEXT NOT NULL,site_id TEXT NOT NULL,resource_id TEXT NOT NULL,payload TEXT NOT NULL CHECK(json_valid(payload)))`,
	`CREATE INDEX audit_org_id ON audit_events(organization_id,id DESC)`,
	`CREATE INDEX audit_event_id ON audit_events(event,id DESC)`,
	`CREATE INDEX audit_user_id ON audit_events(username,id DESC)`,
	`CREATE INDEX audit_site_id ON audit_events(site_id,id DESC)`,
	`CREATE INDEX audit_resource_id ON audit_events(resource_id,id DESC)`,
	`CREATE INDEX audit_time_id ON audit_events(occurred_at,id DESC)`,
	`CREATE TABLE audit_index_state (singleton INTEGER PRIMARY KEY CHECK(singleton=1),epoch TEXT NOT NULL,file_count INTEGER NOT NULL,skipped INTEGER NOT NULL)`,
	`INSERT INTO audit_index_state VALUES(1,'',0,0)`,
}

var auditSchemaV3 = []string{
	`CREATE TABLE audit_sources(id INTEGER PRIMARY KEY,slot INTEGER NOT NULL UNIQUE,skipped INTEGER NOT NULL DEFAULT 0)`,
	`INSERT INTO audit_sources(id,slot,skipped) VALUES(1,0,0)`,
	`CREATE TABLE audit_events_v3 (id INTEGER PRIMARY KEY,organization_id TEXT,occurred_at TEXT NOT NULL,event TEXT NOT NULL,username TEXT NOT NULL,site_id TEXT NOT NULL,resource_id TEXT NOT NULL,payload TEXT NOT NULL CHECK(json_valid(payload)),source_id INTEGER NOT NULL REFERENCES audit_sources(id))`,
	`INSERT INTO audit_events_v3 SELECT id,organization_id,occurred_at,event,username,site_id,resource_id,payload,1 FROM audit_events`,
	`DROP TABLE audit_events`,
	`ALTER TABLE audit_events_v3 RENAME TO audit_events`,
	`CREATE INDEX audit_org_id ON audit_events(organization_id,id DESC)`,
	`CREATE INDEX audit_event_id ON audit_events(event,id DESC)`,
	`CREATE INDEX audit_user_id ON audit_events(username,id DESC)`,
	`CREATE INDEX audit_site_id ON audit_events(site_id,id DESC)`,
	`CREATE INDEX audit_resource_id ON audit_events(resource_id,id DESC)`,
	`CREATE INDEX audit_time_id ON audit_events(occurred_at,id DESC)`,
	`CREATE INDEX audit_source_id ON audit_events(source_id)`,
}

func ensureAuditSchema(db *sql.DB, version int) error {
	if version < 3 {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback()
		statements := []string{}
		if version == 1 {
			statements = append(statements, auditSchema...)
		}
		statements = append(statements, auditSchemaV3...)
		statements = append(statements, "PRAGMA user_version=3")
		for _, q := range statements {
			if _, err = tx.Exec(q); err != nil {
				return err
			}
		}
		if err = tx.Commit(); err != nil {
			return err
		}
	}
	rows, err := db.Query("SELECT organization_id,occurred_at,event,username,site_id,resource_id,payload,source_id FROM audit_events LIMIT 0")
	if err != nil {
		return err
	}
	rows.Close()
	var epoch string
	if err = db.QueryRow("SELECT epoch FROM audit_index_state WHERE singleton=1").Scan(&epoch); err != nil {
		return err
	}
	rows, err = db.Query("SELECT id,slot,skipped FROM audit_sources LIMIT 0")
	if rows != nil {
		rows.Close()
	}
	return err
}
func auditValues(raw []byte) ([]any, bool) {
	var entry map[string]any
	if !utf8.Valid(raw) || json.Unmarshal(raw, &entry) != nil || entry == nil {
		return nil, false
	}
	get := func(key string) string { v, _ := entry[key].(string); return v }
	var org any
	if value, ok := entry["organizationID"].(string); ok {
		org = value
	}
	at := ""
	if timestamp, err := time.Parse(time.RFC3339Nano, get("time")); err == nil {
		at = timestamp.UTC().Format(historyTimeLayout)
	}
	for _, key := range []string{"event", "user", "siteID", "resourceID"} {
		if v, ok := entry[key]; ok && v != nil {
			if _, valid := v.(string); !valid {
				return nil, false
			}
		}
	}
	return []any{org, at, get("event"), get("user"), get("siteID"), get("resourceID"), string(raw)}, true
}

const insertAuditSQL = "INSERT INTO audit_events(organization_id,occurred_at,event,username,site_id,resource_id,payload,source_id) VALUES(?,?,?,?,?,?,?,?)"

func (h *historyStore) indexAudit(raw []byte) error {
	values, valid := auditValues(raw)
	if !valid || len(raw) >= 128<<10 {
		return fmt.Errorf("invalid or oversized audit record")
	}
	_, err := h.db.Exec(insertAuditSQL, append(values, h.auditSource.Load())...)
	if err != nil {
		h.auditDirty.Store(true)
	}
	return err
}

// Caller holds auditMu; rebuild from immutable retained JSONL sources in one
// transaction. Rotate/restart changes epoch so old pagination cannot silently
// duplicate or skip events after row IDs are reassigned.
func (s *Server) rebuildAuditIndex() error {
	s.history.auditDirty.Store(true)
	tx, err := s.history.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec("DELETE FROM audit_events"); err != nil {
		return err
	}
	if _, err = tx.Exec("DELETE FROM audit_sources"); err != nil {
		return err
	}
	statement, err := tx.Prepare(insertAuditSQL)
	if err != nil {
		return err
	}
	defer statement.Close()
	files, skipped := 0, 0
	var activeSource, activeSize int64
	names := []string{}
	for i := s.cfg.AuditBackups; i >= 1; i-- {
		names = append(names, fmt.Sprintf("audit.jsonl.%d", i))
	}
	names = append(names, "audit.jsonl")
	for _, name := range names {
		path := filepath.Join(s.cfg.DataDir, name)
		info, e := os.Lstat(path)
		if os.IsNotExist(e) {
			continue
		}
		if e != nil {
			return e
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("audit source must be a regular file")
		}
		f, e := os.Open(path)
		if e != nil {
			return e
		}
		files++
		slot := 0
		if name != "audit.jsonl" {
			slot, _ = strconv.Atoi(strings.TrimPrefix(name, "audit.jsonl."))
		}
		result, e := tx.Exec("INSERT INTO audit_sources(slot,skipped) VALUES(?,0)", slot)
		if e != nil {
			f.Close()
			return e
		}
		sourceID, e := result.LastInsertId()
		if e != nil {
			f.Close()
			return e
		}
		if slot == 0 {
			activeSource = sourceID
			activeSize = info.Size()
		}
		beforeSkipped := skipped
		reader := bufio.NewReaderSize(f, 128<<10)
		for {
			line, e := reader.ReadSlice('\n')
			if e == bufio.ErrBufferFull {
				skipped++
				for e == bufio.ErrBufferFull {
					_, e = reader.ReadSlice('\n')
				}
				if e != nil && e != io.EOF {
					f.Close()
					return e
				}
				if e == io.EOF {
					break
				}
				continue
			}
			if e != nil && e != io.EOF {
				f.Close()
				return e
			}
			if len(bytes.TrimSpace(line)) > 0 {
				if values, valid := auditValues(bytes.TrimSpace(line)); valid {
					if _, e = statement.Exec(append(values, sourceID)...); e != nil {
						f.Close()
						return e
					}
				} else {
					skipped++
				}
			}
			if e == io.EOF {
				break
			}
		}
		f.Close()
		if _, err = tx.Exec("UPDATE audit_sources SET skipped=? WHERE id=?", skipped-beforeSkipped, sourceID); err != nil {
			return err
		}
	}
	if activeSource == 0 {
		return fmt.Errorf("active audit source missing")
	}
	if _, err = tx.Exec("UPDATE audit_index_state SET epoch=?,file_count=?,skipped=? WHERE singleton=1", randomID(), files, skipped); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	s.history.auditSource.Store(activeSource)
	s.history.auditSize.Store(activeSize)
	s.history.auditDirty.Store(false)
	return nil
}

type auditFilters struct {
	Event, User, Site, Resource, Since, Epoch string
	Before                                    int64
	Limit                                     int
}
type auditPage struct {
	Entries      []map[string]any `json:"entries"`
	NextCursor   string           `json:"nextCursor,omitempty"`
	IndexedFiles int              `json:"indexedFiles,omitempty"`
	Skipped      int              `json:"skipped,omitempty"`
}

func (h *historyStore) queryAudit(actor User, f auditFilters) (auditPage, error) {
	out := auditPage{Entries: []map[string]any{}}
	tx, err := h.db.Begin()
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	var epoch string
	var files, skipped int
	if err = tx.QueryRow("SELECT epoch,file_count,skipped FROM audit_index_state WHERE singleton=1").Scan(&epoch, &files, &skipped); err != nil {
		return out, err
	}
	if f.Epoch != "" && f.Epoch != epoch {
		return out, errAuditIndexChanged
	}
	query := "SELECT id,payload FROM audit_events WHERE 1=1"
	args := []any{}
	if actor.Role != "admin" {
		query += " AND organization_id=?"
		args = append(args, actor.OrganizationID)
	}
	for _, filter := range []struct{ column, value string }{{"event", f.Event}, {"username", f.User}, {"site_id", f.Site}, {"resource_id", f.Resource}} {
		if filter.value != "" {
			query += " AND " + filter.column + "=?"
			args = append(args, filter.value)
		}
	}
	if f.Since != "" {
		query += " AND occurred_at>=?"
		args = append(args, f.Since)
	}
	if f.Before > 0 {
		query += " AND id<?"
		args = append(args, f.Before)
	}
	query += " ORDER BY id DESC LIMIT ?"
	args = append(args, f.Limit+1)
	rows, err := tx.Query(query, args...)
	if err != nil {
		return out, err
	}
	last := int64(0)
	pageBytes := 0
	for rows.Next() {
		var id int64
		var raw string
		if err = rows.Scan(&id, &raw); err != nil {
			rows.Close()
			return out, err
		}
		if len(out.Entries) == f.Limit || len(out.Entries) > 0 && pageBytes+len(raw) > 2<<20 {
			out.NextCursor = epoch + ":" + strconv.FormatInt(last, 10)
			break
		}
		var entry map[string]any
		if err = json.Unmarshal([]byte(raw), &entry); err != nil {
			rows.Close()
			return out, err
		}
		out.Entries = append(out.Entries, entry)
		pageBytes += len(raw)
		last = id
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	if err = tx.Commit(); err != nil {
		return out, err
	}
	if actor.Role == "admin" {
		out.IndexedFiles, out.Skipped = files, skipped
	}
	return out, nil
}
func parseAuditCursor(raw string) (string, int64, error) {
	if raw == "" {
		return "", 0, nil
	}
	if len(raw) > 80 {
		return "", 0, fmt.Errorf("invalid audit cursor")
	}
	parts := strings.Split(raw, ":")
	if len(parts) != 2 || len(parts[0]) != 48 {
		return "", 0, fmt.Errorf("invalid audit cursor")
	}
	id, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || id < 1 {
		return "", 0, fmt.Errorf("invalid audit cursor")
	}
	return parts[0], id, nil
}

// Filesystem rotation has completed. Retained source IDs and event IDs stay
// stable; only the expired source's rows are deleted. Epoch still changes so
// callers explicitly restart a page sequence across retention boundaries.
func (s *Server) rotateAuditIndex() error {
	tx, err := s.history.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec("DELETE FROM audit_events WHERE source_id IN (SELECT id FROM audit_sources WHERE slot>=?)", s.cfg.AuditBackups); err != nil {
		return err
	}
	if _, err = tx.Exec("DELETE FROM audit_sources WHERE slot>=?", s.cfg.AuditBackups); err != nil {
		return err
	}
	if _, err = tx.Exec("UPDATE audit_sources SET slot=-slot-1"); err != nil {
		return err
	}
	if _, err = tx.Exec("UPDATE audit_sources SET slot=-slot"); err != nil {
		return err
	}
	result, err := tx.Exec("INSERT INTO audit_sources(slot,skipped) VALUES(0,0)")
	if err != nil {
		return err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return err
	}
	if _, err = tx.Exec("UPDATE audit_index_state SET epoch=?,file_count=(SELECT count(*) FROM audit_sources),skipped=(SELECT coalesce(sum(skipped),0) FROM audit_sources) WHERE singleton=1", randomID()); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	s.history.auditSource.Store(id)
	s.history.auditSize.Store(0)
	return nil
}
