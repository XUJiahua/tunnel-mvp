package control

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/XUJiahua/tunnel-mvp/internal/diagnostics"
)

func probeURL(svc Service) string {
	u, _ := url.Parse(svc.URL)
	if svc.HealthPath != "" {
		if u.Path == "" {
			u.Path = "/"
		}
		u.Path += svc.HealthPath
	}
	return u.String()
}
func (s *Server) loadHealthReports() error {
	for _, svc := range s.store.snapshot().Services {
		samples, err := s.history.samples(svc, time.Now().Add(-historyRetention), 1)
		if err != nil {
			return err
		}
		if len(samples) == 0 {
			continue
		}
		p := samples[0]
		report := probeReport{Reachable: p.Reachable, Healthy: p.Healthy, RequestID: p.RequestID, SiteID: svc.SiteID, ServiceID: svc.ID, Target: svc.URL, Status: p.Status, LatencyMs: p.LatencyMs, CheckedAt: p.CheckedAt}
		if p.Code != "" {
			report.Failure = diagnostics.New(p.Code, p.Stage, "Restored historical result; run a new probe for current details")
		}
		s.reports[svc.ID] = report
	}
	return nil
}
func (s *Server) serviceHistory(w http.ResponseWriter, r *http.Request, u User) {
	svc, ok := s.store.service(r.PathValue("id"))
	if !ok || (!canAccessService(u, svc) && !canManage(u, svc.OrganizationID, "applications:manage")) {
		fail(w, 404, "service not found")
		return
	}
	limit := historyPerService
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > historyPerService {
			fail(w, 400, fmt.Sprintf("limit must be 1–%d", historyPerService))
			return
		}
		limit = n
	}
	since := time.Now().Add(-historyRetention)
	if raw := r.URL.Query().Get("since"); raw != "" {
		value, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil || value.After(time.Now()) {
			fail(w, 400, "since must be a past RFC3339 timestamp")
			return
		}
		if value.After(since) {
			since = value
		}
	}
	rows, err := s.history.samples(svc, since, limit)
	if err != nil {
		fail(w, 500, "could not read monitoring history")
		return
	}
	jsonOut(w, 200, map[string]any{"serviceID": svc.ID, "samples": rows, "retentionHours": int(historyRetention.Hours()), "maxSamples": historyPerService, "probeIntervalSeconds": s.cfg.ProbeIntervalSeconds})
}

func probeMethod(svc Service) string {
	if svc.HealthMethod == "" {
		return "HEAD"
	}
	return svc.HealthMethod
}
