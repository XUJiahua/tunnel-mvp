package control

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/XUJiahua/tunnel-mvp/internal/state"
)

type pooledTransport struct {
	sessionKey string
	lastUsed   time.Time
	service    Service
	transport  *http.Transport
}
type counters struct{ requests, errors, probes, historyErrors, auditIndexErrors, identitySyncChecks, identitySyncErrors, identitySyncRevocations atomic.Uint64 }

// Pool keys are immutable application identities; different sites and targets
// never share an HTTP connection. Every request still passes authorization.
func (s *Server) serviceTransport(svc Service, sessions ...string) *http.Transport {
	session := ""
	key := svc.ID
	if svc.ConnectionMode == "session" || s.cluster != nil {
		session = "probe"
		if len(sessions) > 0 && sessions[0] != "" {
			session = sessions[0]
		}
		key += "~" + session
		if s.cluster != nil && len(sessions) > 1 {
			key += "~" + sessions[1]
		}
	}
	s.poolMu.Lock()
	defer s.poolMu.Unlock()
	if p, ok := s.pools[key]; ok && p.service == svc {
		p.lastUsed = time.Now()
		s.pools[key] = p
		return p.transport
	}
	if p, ok := s.pools[key]; ok {
		p.transport.CloseIdleConnections()
	}
	if session != "" {
		count := 0
		oldestKey := ""
		var oldest time.Time
		for k, p := range s.pools {
			if p.sessionKey != "" {
				count++
				if oldestKey == "" || p.lastUsed.Before(oldest) {
					oldestKey = k
					oldest = p.lastUsed
				}
			}
		}
		if count >= 1024 {
			s.pools[oldestKey].transport.CloseIdleConnections()
			delete(s.pools, oldestKey)
		}
	}
	t := s.transport(svc.SiteID, svc.ID)
	t.MaxIdleConns = 32
	t.MaxIdleConnsPerHost = 16
	t.MaxConnsPerHost = 64
	if session != "" && (s.cluster == nil || svc.ConnectionMode == "session") {
		t.MaxIdleConns = 1
		t.MaxIdleConnsPerHost = 1
		t.MaxConnsPerHost = 1
	}
	s.pools[key] = pooledTransport{service: svc, transport: t, sessionKey: session, lastUsed: time.Now()}
	return t
}
func (s *Server) dropTransport(id string) {
	s.poolMu.Lock()
	defer s.poolMu.Unlock()
	for key, p := range s.pools {
		if p.service.ID == id {
			p.transport.CloseIdleConnections()
			delete(s.pools, key)
		}
	}
}
func (s *Server) snapshotPools() map[string]pooledTransport {
	s.poolMu.Lock()
	defer s.poolMu.Unlock()
	pools := make(map[string]pooledTransport, len(s.pools))
	for key, pool := range s.pools {
		pools[key] = pool
	}
	return pools
}
func (s *Server) pruneSessionPools() {
	pools := s.snapshotPools()
	active := map[string]bool{}
	snapshot, err := s.authSnapshot(context.Background(), false)
	if err == nil {
		for key, sess := range snapshot.Data.Sessions {
			if snapshot.Now.Before(sess.Expires) {
				active[key] = true
			}
		}
	}
	s.poolMu.Lock()
	defer s.poolMu.Unlock()
	for key, p := range pools {
		if current, ok := s.pools[key]; !ok || current.transport != p.transport {
			continue
		}
		if p.sessionKey != "" && p.sessionKey != "probe" && !active[p.sessionKey] {
			p.transport.CloseIdleConnections()
			delete(s.pools, key)
		}
	}
}
func (s *Server) dropSiteTransports(id string) {
	s.poolMu.Lock()
	defer s.poolMu.Unlock()
	for key, p := range s.pools {
		if p.service.SiteID == id {
			p.transport.CloseIdleConnections()
			delete(s.pools, key)
		}
	}
}
func (s *Server) startBackground() {
	ctx, cancel := context.WithCancel(context.Background())
	s.backgroundCancel = cancel
	s.backgroundWG.Add(1)
	go func() {
		defer s.backgroundWG.Done()
		cleanup := time.NewTicker(time.Minute)
		defer cleanup.Stop()
		interval := time.Duration(s.cfg.ProbeIntervalSeconds) * time.Second
		var tick <-chan time.Time
		if interval > 0 {
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			tick = ticker.C
		}
		for {
			select {
			case <-ctx.Done():
				return
			case <-cleanup.C:
				_ = s.authUpdate(ctx, false, func(_ *state.AuthData, _ State, _ time.Time) error { return nil })
				s.pruneSessionPools()
			case <-tick:
				s.probeAll(ctx)
			}
		}
	}()
	s.startIdentitySync(ctx)
	s.startStorageFence(ctx)
	s.startSharedAuth(ctx)
	s.startCluster(ctx)
	s.startObservations(ctx)
}
func (s *Server) probeAll(ctx context.Context) {
	// Four workers bound concurrency and shutdown latency; one batch at a time.
	jobs := make(chan Service)
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for svc := range jobs {
				report := s.checkService(ctx, svc)
				s.saveReport(svc, report)
			}
		}()
	}
	for _, svc := range s.store.snapshot().Services {
		select {
		case jobs <- svc:
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			return
		}
	}
	close(jobs)
	wg.Wait()
}
func (s *Server) saveReport(svc Service, report probeReport) {
	if s.cluster != nil {
		report.NodeID = s.cluster.node.ID
		report.NodeEpoch = s.cluster.node.Epoch
	}
	// Order with target edits/deletes; an obsolete probe must not restore a stale
	// result or keep an obsolete connection pool alive.
	s.accessMu.RLock()
	defer s.accessMu.RUnlock()
	current, ok := s.store.service(svc.ID)
	if !ok || current != svc {
		s.dropTransport(svc.ID)
		return
	}
	s.monitorMu.Lock()
	old, exists := s.reports[svc.ID]
	if exists && old.CheckedAt.After(report.CheckedAt) {
		s.monitorMu.Unlock()
		return
	}
	s.reports[svc.ID] = report
	s.monitorMu.Unlock()
	if err := s.history.save(svc, report); err != nil {
		s.stats.historyErrors.Add(1)
		s.history.observationError()
		slog.Error("monitoring history write failed", "service_id", svc.ID, "error", err)
	}
	if !exists || old.Reachable != report.Reachable || old.Healthy != report.Healthy {
		s.recordDiagnostic("health_changed", User{Username: "system"}, report)
	}
}
func (s *Server) status(w http.ResponseWriter, r *http.Request, u User) {
	if s.observer != nil {
		reports, err := s.latestClusterReports(r, false)
		if err != nil {
			fail(w, 503, "cluster monitoring unavailable or authorization changed")
			return
		}
		jsonOut(w, 200, map[string]any{"probeIntervalSeconds": s.cfg.ProbeIntervalSeconds, "reports": reports, "scope": "cluster"})
		return
	}

	out := []probeReport{}
	s.monitorMu.Lock()
	reports := make(map[string]probeReport, len(s.reports))
	for k, v := range s.reports {
		reports[k] = v
	}
	s.monitorMu.Unlock()
	for _, svc := range s.store.snapshot().Services {
		if canAccessService(u, svc) || canManage(u, svc.OrganizationID, "applications:manage") {
			if p, ok := reports[svc.ID]; ok {
				out = append(out, p)
			}
		}
	}
	jsonOut(w, 200, map[string]any{"probeIntervalSeconds": s.cfg.ProbeIntervalSeconds, "reports": out})
}
func metric(w io.Writer, name, help, kind string, value any) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n%s %v\n", name, help, name, kind, name, value)
}
func (s *Server) metrics(w http.ResponseWriter, r *http.Request, _ User) {
	sharedReports := map[string]probeReport{}
	if s.observer != nil {
		rows, err := s.latestClusterReports(r, r.URL.Path == "/metrics")
		if err != nil {
			fail(w, 503, "cluster health metrics unavailable")
			return
		}
		for _, p := range rows {
			sharedReports[p.ServiceID] = p
		}
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if s.observer != nil {
		if !s.observationMetrics(w, r) {
			return
		}
	}
	syncEnabled := 0
	if s.identitySync != nil {
		syncEnabled = 1
	}
	metric(w, "tunnel_identity_sync_enabled", "Keycloak user status checks enabled.", "gauge", syncEnabled)
	metric(w, "tunnel_identity_sync_checks_total", "Completed Keycloak user status checks.", "counter", s.stats.identitySyncChecks.Load())
	metric(w, "tunnel_identity_sync_errors_total", "Keycloak status/token check failures.", "counter", s.stats.identitySyncErrors.Load())
	metric(w, "tunnel_identity_sync_revocations_total", "OIDC sessions revoked by user status or staleness.", "counter", s.stats.identitySyncRevocations.Load())
	var readErrors uint64
	if statistics, ok := s.store.Repository.(interface{ QueryReadFailures() uint64 }); ok {
		readErrors = statistics.QueryReadFailures()
	}
	metric(w, "tunnel_state_query_errors_total", "Failed persistent state projection reads.", "counter", readErrors)
	metric(w, "tunnel_proxy_requests_total", "Authorized proxy requests.", "counter", s.stats.requests.Load())
	metric(w, "tunnel_proxy_errors_total", "Proxy transport errors.", "counter", s.stats.errors.Load())
	metric(w, "tunnel_audit_index_errors_total", "Audit source/index writes or rebuild failures.", "counter", s.stats.auditIndexErrors.Load())
	metric(w, "tunnel_monitor_storage_errors_total", "Monitoring history storage write failures.", "counter", s.stats.historyErrors.Load())
	metric(w, "tunnel_probes_total", "Completed application probes.", "counter", s.stats.probes.Load())
	s.liveMu.Lock()
	active := len(s.live)
	s.liveMu.Unlock()
	s.poolMu.Lock()
	shared, isolated, probe := 0, 0, 0
	for _, p := range s.pools {
		if p.sessionKey == "" {
			shared++
		} else if p.sessionKey == "probe" {
			probe++
		} else {
			isolated++
		}
	}
	s.poolMu.Unlock()
	fmt.Fprint(w, "# HELP tunnel_http_pools Current HTTP pool entries.\n# TYPE tunnel_http_pools gauge\n")
	fmt.Fprintf(w, "tunnel_http_pools{mode=\"shared\"} %d\ntunnel_http_pools{mode=\"session\"} %d\ntunnel_http_pools{mode=\"probe\"} %d\n", shared, isolated, probe)
	metric(w, "tunnel_active_requests", "Active proxy requests and manual probes.", "gauge", active)
	fmt.Fprint(w, "# HELP tunnel_site_online Agent tunnel connected.\n# TYPE tunnel_site_online gauge\n")
	fmt.Fprint(w, "# HELP tunnel_site_agents Connected Agent instances.\n# TYPE tunnel_site_agents gauge\n")
	for _, site := range s.store.snapshot().Sites {
		online := 0
		if s.siteOnline(site.ID) {
			online = 1
		}
		fmt.Fprintf(w, "tunnel_site_online{site_id=%q} %d\ntunnel_site_agents{site_id=%q} %d\n", site.ID, online, site.ID, s.siteAgentCount(site.ID))
	}
	reports := sharedReports
	if s.observer == nil {
		s.monitorMu.Lock()
		for id, p := range s.reports {
			reports[id] = p
		}
		s.monitorMu.Unlock()
	}
	fmt.Fprint(w, "# HELP tunnel_service_reachable Last probe received HTTP headers.\n# TYPE tunnel_service_reachable gauge\n# HELP tunnel_service_healthy Last probe matched the configured HTTP status policy.\n# TYPE tunnel_service_healthy gauge\n# HELP tunnel_service_probe_timestamp_seconds Last probe start timestamp.\n# TYPE tunnel_service_probe_timestamp_seconds gauge\n# HELP tunnel_service_probe_duration_seconds Last probe duration.\n# TYPE tunnel_service_probe_duration_seconds gauge\n")
	for id, p := range reports {
		reachable, healthy := 0, 0
		if p.Reachable {
			reachable = 1
		}
		if p.Healthy {
			healthy = 1
		}
		fmt.Fprintf(w, "tunnel_service_reachable{service_id=%q} %d\ntunnel_service_healthy{service_id=%q} %d\ntunnel_service_probe_timestamp_seconds{service_id=%q} %d\ntunnel_service_probe_duration_seconds{service_id=%q} %g\n", id, reachable, id, healthy, id, p.CheckedAt.Unix(), id, float64(p.LatencyMs)/1000)
	}
}

// JSONL remains the source of truth; SQLite indexes all retained files.
func (s *Server) listAudit(w http.ResponseWriter, r *http.Request, actor User) {
	limit := 100
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 500 {
			fail(w, 400, "limit must be 1–500")
			return
		}
		limit = n
	}
	epoch, before, err := parseAuditCursor(r.URL.Query().Get("cursor"))
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	filters := auditFilters{Event: r.URL.Query().Get("event"), User: r.URL.Query().Get("user"), Site: r.URL.Query().Get("siteID"), Resource: r.URL.Query().Get("resourceID"), Epoch: epoch, Before: before, Limit: limit}
	for _, value := range []string{filters.Event, filters.User, filters.Site, filters.Resource} {
		if len(value) > 100 {
			fail(w, 400, "audit filters must be at most 100 bytes")
			return
		}
	}
	if raw := r.URL.Query().Get("since"); raw != "" {
		at, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			fail(w, 400, "since must be RFC3339")
			return
		}
		filters.Since = at.UTC().Format(historyTimeLayout)
	}
	if s.observer != nil {
		s.clusterAudit(w, r, filters)
		return
	}
	s.auditMu.Lock()
	if s.history.auditDirty.Load() {
		err = s.rebuildAuditIndex()
	}
	s.auditMu.Unlock()
	if err != nil {
		s.stats.auditIndexErrors.Add(1)
		fail(w, 503, "audit index unavailable; retained JSONL files are unchanged")
		return
	}
	page, err := s.history.queryAudit(actor, filters)
	if err != nil {
		if errors.Is(err, errAuditIndexChanged) {
			fail(w, 409, err.Error())
		} else {
			fail(w, 503, "audit index unavailable")
		}
		return
	}
	jsonOut(w, 200, page)
}
func (s *Server) rotateAudit() (err error) { // caller holds auditMu
	info, err := s.auditFile.Stat()
	if err != nil {
		return err
	}
	if info.Size() < int64(s.cfg.AuditMaxMB)<<20 {
		return nil
	}
	name := s.auditFile.Name()
	// Windows cannot rename an open file. The lock keeps all writers/readers out
	// until the active file is reopened, including when a historical move fails.
	if err = s.auditFile.Close(); err != nil {
		return err
	}
	defer func() {
		next, openErr := os.OpenFile(name, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if openErr != nil {
			err = fmt.Errorf("reopen active audit: %w (rotation: %v)", openErr, err)
			return
		}
		s.auditFile = next
	}()
	if err = os.Remove(name + "." + strconv.Itoa(s.cfg.AuditBackups)); err != nil && !os.IsNotExist(err) {
		return err
	}
	for i := s.cfg.AuditBackups - 1; i >= 1; i-- {
		if err = os.Rename(name+"."+strconv.Itoa(i), name+"."+strconv.Itoa(i+1)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return os.Rename(name, name+".1")
}

func (s *Server) logRotationError(err error) {
	slog.Error("audit rotation failed", "error", strings.TrimSpace(err.Error()))
}
