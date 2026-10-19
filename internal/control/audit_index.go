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

func ensureAuditSchema(db *sql.DB, version int) error {
	if version == 1 {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback()
		for _, statement := range append(append([]string{}, auditSchema...), "PRAGMA user_version=2") {
			if _, err = tx.Exec(statement); err != nil {
				return err
			}
		}
		if err = tx.Commit(); err != nil {
			return err
		}
	}
	rows, err := db.Query("SELECT organization_id,occurred_at,event,username,site_id,resource_id,payload FROM audit_events LIMIT 0")
	if err != nil {
		return err
	}
	rows.Close()
	var epoch string
	return db.QueryRow("SELECT epoch FROM audit_index_state WHERE singleton=1").Scan(&epoch)
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

const insertAuditSQL = "INSERT INTO audit_events(organization_id,occurred_at,event,username,site_id,resource_id,payload) VALUES(?,?,?,?,?,?,?)"

func (h *historyStore) indexAudit(raw []byte) error {
	values, valid := auditValues(raw)
	if !valid {
		return fmt.Errorf("invalid audit record")
	}
	_, err := h.db.Exec(insertAuditSQL, values...)
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
	statement, err := tx.Prepare(insertAuditSQL)
	if err != nil {
		return err
	}
	defer statement.Close()
	files, skipped := 0, 0
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
					if _, e = statement.Exec(values...); e != nil {
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
	}
	if _, err = tx.Exec("UPDATE audit_index_state SET epoch=?,file_count=?,skipped=? WHERE singleton=1", randomID(), files, skipped); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
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
