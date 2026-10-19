package control

import (
	"bufio"
	"context"
	"encoding/json"
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
)

type pooledTransport struct {
	service   Service
	transport *http.Transport
}
type counters struct{ requests, errors, probes atomic.Uint64 }

// Pool keys are immutable application identities; different sites and targets
// never share an HTTP connection. Every request still passes authorization.
func (s *Server) serviceTransport(svc Service) *http.Transport {
	s.poolMu.Lock()
	defer s.poolMu.Unlock()
	if p, ok := s.pools[svc.ID]; ok && p.service == svc {
		return p.transport
	}
	if p, ok := s.pools[svc.ID]; ok {
		p.transport.CloseIdleConnections()
	}
	t := s.transport(svc.SiteID)
	t.MaxIdleConns = 32
	t.MaxIdleConnsPerHost = 16
	t.MaxConnsPerHost = 64
	s.pools[svc.ID] = pooledTransport{svc, t}
	return t
}
func (s *Server) dropTransport(id string) {
	s.poolMu.Lock()
	defer s.poolMu.Unlock()
	if p, ok := s.pools[id]; ok {
		p.transport.CloseIdleConnections()
		delete(s.pools, id)
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
				s.auth.mu.Lock()
				s.auth.clean(time.Now())
				s.auth.mu.Unlock()
			case <-tick:
				s.probeAll(ctx)
			}
		}
	}()
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
	if !exists || old.Reachable != report.Reachable || old.Healthy != report.Healthy {
		s.recordDiagnostic("health_changed", User{Username: "system"}, report)
	}
}
func (s *Server) status(w http.ResponseWriter, r *http.Request, u User) {
	out := []probeReport{}
	s.monitorMu.Lock()
	reports := make(map[string]probeReport, len(s.reports))
	for k, v := range s.reports {
		reports[k] = v
	}
	s.monitorMu.Unlock()
	for _, svc := range s.store.snapshot().Services {
		if canAccessService(u, svc) {
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
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	metric(w, "tunnel_proxy_requests_total", "Authorized proxy requests.", "counter", s.stats.requests.Load())
	metric(w, "tunnel_proxy_errors_total", "Proxy transport errors.", "counter", s.stats.errors.Load())
	metric(w, "tunnel_probes_total", "Completed application probes.", "counter", s.stats.probes.Load())
	s.liveMu.Lock()
	active := len(s.live)
	s.liveMu.Unlock()
	metric(w, "tunnel_active_requests", "Active proxy requests and manual probes.", "gauge", active)
	fmt.Fprint(w, "# HELP tunnel_site_online Agent tunnel connected.\n# TYPE tunnel_site_online gauge\n")
	for _, site := range s.store.snapshot().Sites {
		online := 0
		if s.tunnel.HasSession(site.ID) {
			online = 1
		}
		fmt.Fprintf(w, "tunnel_site_online{site_id=%q} %d\n", site.ID, online)
	}
	s.monitorMu.Lock()
	defer s.monitorMu.Unlock()
	fmt.Fprint(w, "# HELP tunnel_service_reachable Last probe received HTTP headers.\n# TYPE tunnel_service_reachable gauge\n# HELP tunnel_service_healthy Last HEAD returned HTTP 200-399.\n# TYPE tunnel_service_healthy gauge\n# HELP tunnel_service_probe_timestamp_seconds Last probe start timestamp.\n# TYPE tunnel_service_probe_timestamp_seconds gauge\n# HELP tunnel_service_probe_duration_seconds Last probe duration.\n# TYPE tunnel_service_probe_duration_seconds gauge\n")
	for id, p := range s.reports {
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

// The console reads a bounded tail of the current audit file. Rotated files
// remain available for backup and external log collection.
func (s *Server) listAudit(w http.ResponseWriter, r *http.Request, _ User) {
	limit := 100
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 500 {
			fail(w, 400, "limit must be 1–500")
			return
		}
		limit = n
	}
	s.auditMu.Lock()
	f, err := os.Open(s.auditFile.Name())
	s.auditMu.Unlock()
	if err != nil {
		fail(w, 500, "could not read audit")
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		fail(w, 500, "could not read audit")
		return
	}
	const maxTail int64 = 2 << 20
	offset := max(int64(0), info.Size()-maxTail)
	if _, err = f.Seek(offset, io.SeekStart); err != nil {
		fail(w, 500, "could not read audit")
		return
	}
	scanner := bufio.NewScanner(io.LimitReader(f, maxTail))
	scanner.Buffer(make([]byte, 4096), 128<<10)
	if offset > 0 {
		scanner.Scan()
	} // skip an incomplete first line
	rows := []map[string]any{}
	for scanner.Scan() {
		var row map[string]any
		if json.Unmarshal(scanner.Bytes(), &row) != nil {
			continue
		}
		if event := r.URL.Query().Get("event"); event != "" && row["event"] != event {
			continue
		}
		if site := r.URL.Query().Get("siteID"); site != "" && row["siteID"] != site {
			continue
		}
		rows = append(rows, row)
		if len(rows) > limit {
			rows = rows[1:]
		}
	}
	if scanner.Err() != nil {
		fail(w, 500, "could not scan audit")
		return
	}
	for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
		rows[i], rows[j] = rows[j], rows[i]
	}
	jsonOut(w, 200, map[string]any{"entries": rows, "tailBytes": maxTail})
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
