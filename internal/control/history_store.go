package control

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"
)

const historyTimeLayout = "2006-01-02T15:04:05.000000000Z"
const historyRetention = 7 * 24 * time.Hour
const historyPerService = 1440
const historyGlobal = 100000
const monitorApplicationID = 0x544d4f4e

type historyStore struct {
	db         *sql.DB
	auditDirty atomic.Bool
}
type healthSample struct {
	CheckedAt time.Time `json:"checkedAt"`
	Reachable bool      `json:"reachable"`
	Healthy   bool      `json:"healthy"`
	Status    int       `json:"status"`
	LatencyMs int64     `json:"latencyMs"`
	RequestID string    `json:"requestID"`
	Code      string    `json:"code,omitempty"`
	Stage     string    `json:"stage,omitempty"`
}

func healthProfile(s Service) string {
	raw, _ := json.Marshal(struct {
		SiteID, URL, Path, Method string
		Status                    int
	}{s.SiteID, s.URL, s.HealthPath, probeMethod(s), s.HealthExpectedStatus})
	return digest(string(raw))
}
func openHistory(dir string) (*historyStore, error) {
	name := filepath.Join(dir, "monitor.db")
	created := false
	if _, err := os.Stat(name); os.IsNotExist(err) {
		f, err := os.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
		if err != nil {
			return nil, err
		}
		f.Close()
		created = true
	} else if err != nil {
		return nil, err
	}
	absolute, err := filepath.Abs(name)
	if err != nil {
		return nil, err
	}
	normalized := filepath.ToSlash(absolute)
	if !strings.HasPrefix(normalized, "/") {
		normalized = "/" + normalized
	}
	query := url.Values{"mode": {"rw"}, "_pragma": {"busy_timeout(1000)", "synchronous(FULL)"}}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: normalized, RawQuery: query.Encode()}).String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	ok := false
	defer func() {
		if !ok {
			db.Close()
			if created {
				os.Remove(name)
			}
		}
	}()
	if created {
		tx, e := db.Begin()
		if e != nil {
			return nil, e
		}
		defer tx.Rollback()
		for _, statement := range []string{`CREATE TABLE samples (id INTEGER PRIMARY KEY,service_id TEXT NOT NULL,profile TEXT NOT NULL,checked_at TEXT NOT NULL,reachable INTEGER NOT NULL CHECK(reachable IN (0,1)),healthy INTEGER NOT NULL CHECK(healthy IN (0,1)),status INTEGER NOT NULL CHECK(status=0 OR status BETWEEN 100 AND 599),latency_ms INTEGER NOT NULL CHECK(latency_ms>=0),request_id TEXT NOT NULL,code TEXT NOT NULL,stage TEXT NOT NULL)`, `CREATE INDEX samples_service_time ON samples(service_id,checked_at DESC,id DESC)`, `CREATE INDEX samples_time ON samples(checked_at DESC,id DESC)`, fmt.Sprintf("PRAGMA application_id=%d", monitorApplicationID), "PRAGMA user_version=1"} {
			if _, err = tx.Exec(statement); err != nil {
				return nil, err
			}
		}
		if err = tx.Commit(); err != nil {
			return nil, err
		}
	}
	var app, version int
	var integrity string
	if err = db.QueryRow("PRAGMA application_id").Scan(&app); err != nil {
		return nil, err
	}
	if err = db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return nil, err
	}
	if app != monitorApplicationID || (version < 1 || version > 2) {
		return nil, fmt.Errorf("unsupported monitor database application/schema")
	}
	if err = db.QueryRow("PRAGMA quick_check").Scan(&integrity); err != nil || integrity != "ok" {
		return nil, fmt.Errorf("monitor database integrity check failed: %v", err)
	}
	rows, e := db.Query("SELECT service_id,profile,checked_at,reachable,healthy,status,latency_ms,request_id,code,stage FROM samples LIMIT 0")
	if e != nil {
		return nil, e
	}
	rows.Close()
	if err = ensureAuditSchema(db, version); err != nil {
		return nil, err
	}
	ok = true
	result := &historyStore{db: db}
	result.auditDirty.Store(true)
	return result, nil
}
func (h *historyStore) close() error { return h.db.Close() }
func (h *historyStore) save(svc Service, p probeReport) error {
	tx, err := h.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	code, stage := "", ""
	if p.Failure != nil {
		code, stage = p.Code, p.Stage
	}
	if _, err = tx.Exec("INSERT INTO samples(service_id,profile,checked_at,reachable,healthy,status,latency_ms,request_id,code,stage) VALUES(?,?,?,?,?,?,?,?,?,?)", svc.ID, healthProfile(svc), p.CheckedAt.UTC().Format(historyTimeLayout), p.Reachable, p.Healthy, p.Status, p.LatencyMs, p.RequestID, code, stage); err != nil {
		return err
	}
	for _, command := range []struct {
		sql  string
		args []any
	}{
		{"DELETE FROM samples WHERE checked_at < ?", []any{time.Now().Add(-historyRetention).UTC().Format(historyTimeLayout)}},
		{"DELETE FROM samples WHERE id IN (SELECT id FROM samples WHERE service_id=? ORDER BY checked_at DESC,id DESC LIMIT -1 OFFSET ?)", []any{svc.ID, historyPerService}},
		{"DELETE FROM samples WHERE id IN (SELECT id FROM samples ORDER BY checked_at DESC,id DESC LIMIT -1 OFFSET ?)", []any{historyGlobal}},
	} {
		if _, err = tx.Exec(command.sql, command.args...); err != nil {
			return err
		}
	}
	return tx.Commit()
}
func (h *historyStore) samples(svc Service, since time.Time, limit int) ([]healthSample, error) {
	rows, err := h.db.Query("SELECT checked_at,reachable,healthy,status,latency_ms,request_id,code,stage FROM samples WHERE service_id=? AND profile=? AND checked_at>=? ORDER BY checked_at DESC,id DESC LIMIT ?", svc.ID, healthProfile(svc), since.UTC().Format(historyTimeLayout), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []healthSample{}
	for rows.Next() {
		var p healthSample
		var raw string
		if err = rows.Scan(&raw, &p.Reachable, &p.Healthy, &p.Status, &p.LatencyMs, &p.RequestID, &p.Code, &p.Stage); err != nil {
			return nil, err
		}
		p.CheckedAt, err = time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			return nil, fmt.Errorf("invalid monitor timestamp")
		}
		out = append(out, p)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}
