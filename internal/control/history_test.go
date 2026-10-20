package control

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/XUJiahua/tunnel-mvp/internal/config"
)

func TestCustomHealthAndHistoryPersistence(t *testing.T) {
	f := setup(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/app/ready" && r.Method == "GET" {
			w.WriteHeader(204)
			return
		}
		w.WriteHeader(503)
	}))
	defer upstream.Close()
	site, token := f.site(t, "health")
	f.agent(t, site, token, portOf(upstream.URL))
	svc := f.service(t, site, "health app", upstream.URL+"/app/")
	_, login := f.member(t, nil, []string{svc.ID})
	access, host, _ := f.grant(t, svc, login)
	res, b := f.request(t, "PATCH", "/api/services/"+svc.ID, map[string]any{"name": svc.Name, "url": svc.URL, "healthPath": "ready", "healthMethod": "GET", "healthExpectedStatus": 204}, f.cookie, "")
	if res.StatusCode != 200 {
		t.Fatal(string(b))
	}
	svc = mustJSON[Service](t, b)
	res, _ = f.request(t, "GET", "/app/", nil, access, host)
	if res.StatusCode != 503 {
		t.Fatal("health configuration revoked application cookie")
	}
	res, b = f.request(t, "POST", "/api/services/"+svc.ID+"/probe", nil, login, "")
	report := mustJSON[probeReport](t, b)
	if res.StatusCode != 200 || !report.Healthy || report.Status != 204 {
		t.Fatal("business health path/status failed", string(b))
	}
	res, b = f.request(t, "GET", "/api/services/"+svc.ID+"/history", nil, login, "")
	history := mustJSON[struct{ Samples []healthSample }](t, b)
	if res.StatusCode != 200 || len(history.Samples) != 1 || history.Samples[0].Status != 204 {
		t.Fatal("probe not stored", string(b))
	}
	other := f.service(t, site, "private", upstream.URL)
	res, _ = f.request(t, "GET", "/api/services/"+other.ID+"/history", nil, login, "")
	if res.StatusCode != 404 {
		t.Fatal("history crossed grant boundary")
	}
	// A separate reader after reopening proves data is durable, rather than an
	// in-memory report cache. Close only the monitoring connection, not the server.
	f.s.history.close()
	reopened, err := openHistory(f.s.cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	f.s.history = reopened
	rows, err := reopened.samples(svc, time.Now().Add(-time.Hour), 10)
	if err != nil || len(rows) != 1 {
		t.Fatal("history did not survive reopen", err)
	}
	res, _ = f.request(t, "PATCH", "/api/services/"+svc.ID, map[string]any{"name": svc.Name, "url": svc.URL, "healthMethod": "HEAD"}, f.cookie, "")
	if res.StatusCode != 200 {
		t.Fatal("health edit failed")
	}
	res, b = f.request(t, "GET", "/api/services/"+svc.ID+"/history", nil, login, "")
	history = mustJSON[struct{ Samples []healthSample }](t, b)
	if len(history.Samples) != 0 {
		t.Fatal("new health profile reused obsolete chart")
	}
	res, _ = f.request(t, "PATCH", "/api/services/"+svc.ID, map[string]any{"name": svc.Name, "url": svc.URL, "healthPath": "../private"}, f.cookie, "")
	if res.StatusCode != 400 {
		t.Fatal("health check escaped mount")
	}
}
func TestHistoryChronologyRetentionAndStorageFailure(t *testing.T) {
	dir := t.TempDir()
	h, err := openHistory(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer h.close()
	svc := Service{ID: "app", SiteID: "site", URL: "http://internal"}
	base := time.Now().UTC().Truncate(time.Second)
	for _, at := range []time.Time{base.Add(time.Millisecond), base, base.Add(-8 * 24 * time.Hour)} {
		if err = h.save(svc, probeReport{CheckedAt: at, Reachable: true, Healthy: true, Status: 200}); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := h.samples(svc, base.Add(-time.Hour), 10)
	if err != nil || len(rows) != 2 || !rows[0].CheckedAt.Equal(base) || !rows[1].CheckedAt.Equal(base.Add(time.Millisecond)) {
		t.Fatal("history timestamp ordering/expiry failed", rows, err)
	}
	// Force a real durable-write error. The current report remains usable while
	// the dedicated storage error metric makes lost history observable.
	f := setup(t)
	site, _ := f.site(t, "offline")
	app := f.service(t, site, "app", "http://127.0.0.1:1")
	if _, err = f.s.history.db.Exec("CREATE TRIGGER reject_sample BEFORE INSERT ON samples BEGIN SELECT RAISE(ABORT,'injected'); END"); err != nil {
		t.Fatal(err)
	}
	f.s.probeAll(context.Background())
	if f.s.stats.historyErrors.Load() != 1 {
		t.Fatal("history storage failure not counted")
	}
	f.s.monitorMu.Lock()
	_, ok := f.s.reports[app.ID]
	f.s.monitorMu.Unlock()
	if !ok {
		t.Fatal("storage failure discarded current health report")
	}

}

func TestHistorySampleCaps(t *testing.T) {
	h, err := openHistory(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer h.close()
	svc := Service{ID: "app", SiteID: "site", URL: "http://internal"}
	now := time.Now().UTC()
	tx, err := h.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	statement, err := tx.Prepare("INSERT INTO samples(service_id,profile,checked_at,reachable,healthy,status,latency_ms,request_id,code,stage) VALUES(?,?,?,1,1,200,1,'request','','')")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < historyPerService+5; i++ {
		if _, err = statement.Exec(svc.ID, healthProfile(svc), now.Add(time.Duration(i)*time.Millisecond).Format(historyTimeLayout)); err != nil {
			t.Fatal(err)
		}
	}
	statement.Close()
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = h.save(svc, probeReport{CheckedAt: now.Add(time.Second * 2), Reachable: true, Healthy: true, Status: 200}); err != nil {
		t.Fatal(err)
	}
	var count int
	h.db.QueryRow("SELECT count(*) FROM samples WHERE service_id=?", svc.ID).Scan(&count)
	if count != historyPerService {
		t.Fatal("per-application history cap failed", count)
	}
	if _, err = h.db.Exec("DELETE FROM samples"); err != nil {
		t.Fatal(err)
	}
	_, err = h.db.Exec(`WITH RECURSIVE seq(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM seq WHERE x<=100000) INSERT INTO samples(service_id,profile,checked_at,reachable,healthy,status,latency_ms,request_id,code,stage) SELECT 'bulk-'||(x%100),'profile',?,1,1,200,1,'request','','' FROM seq`, now.Format(historyTimeLayout))
	if err != nil {
		t.Fatal(err)
	}
	if err = h.save(svc, probeReport{CheckedAt: now.Add(time.Second), Reachable: true, Healthy: true, Status: 200}); err != nil {
		t.Fatal(err)
	}
	h.db.QueryRow("SELECT count(*) FROM samples").Scan(&count)
	if count != historyGlobal {
		t.Fatal("global history cap failed", count)
	}
}
func TestPendingOperationalRestoreBlocksServer(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "operations.restore.pending"), []byte("{}"), 0600)
	_, err := New(config.Server{DataDir: dir, PublicURL: "http://localhost:8080", AccessURL: "http://apps.localhost:8080"}, "strong-recovery-password")
	if err == nil {
		t.Fatal("incomplete operational restore allowed Server startup")
	}
	if _, err = os.Stat(filepath.Join(dir, "state.json")); !os.IsNotExist(err) {
		t.Fatalf("startup initialized state during incomplete restore: %v", err)
	}
}
