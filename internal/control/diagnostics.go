package control

import (
	"encoding/hex"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/XUJiahua/tunnel-mvp/internal/diagnostics"
)

type probeReport struct {
	*diagnostics.Failure
	Healthy   bool      `json:"healthy"`
	Reachable bool      `json:"reachable"`
	RequestID string    `json:"requestID"`
	SiteID    string    `json:"siteID"`
	ServiceID string    `json:"serviceID"`
	Target    string    `json:"target"`
	Status    int       `json:"status,omitempty"`
	LatencyMs int64     `json:"latencyMs"`
	CheckedAt time.Time `json:"checkedAt"`
}

func newReport(svc Service) probeReport {
	return probeReport{RequestID: diagnostics.ID(), SiteID: svc.SiteID, ServiceID: svc.ID, Target: svc.URL, CheckedAt: time.Now().UTC()}
}
func (s *Server) recordDiagnostic(event string, u User, report probeReport) {
	args := []any{"request_id", report.RequestID, "site_id", report.SiteID, "service_id", report.ServiceID, "target", report.Target, "latency_ms", report.LatencyMs}
	if report.Failure != nil {
		args = append(args, "code", report.Code, "stage", report.Stage, "agent_request_id", report.AgentRequestID, "resolved_ips", report.ResolvedIPs, "attempts", report.Attempts, "error", report.Message, "hint", report.Hint)
		slog.Warn(event, args...)
	} else {
		args = append(args, "http_status", report.Status)
		slog.Info(event, args...)
	}
	s.audit(event, u.Username, report.SiteID, report.ServiceID, map[string]any{"diagnostic": report})
}
func (s *Server) proxyFailure(w http.ResponseWriter, u User, svc Service, err error) {
	report := newReport(svc)
	report.Failure = s.describeFailure(svc.SiteID, err)
	s.recordDiagnostic("proxy_error", u, report)
	w.Header().Set("X-Tunnel-Request-ID", report.RequestID)
	jsonOut(w, 502, map[string]any{"error": report.Message, "hint": report.Hint, "requestID": report.RequestID, "diagnostic": report})
}

type diagnosticRecord struct {
	failure diagnostics.Failure
	expires time.Time
}

func (s *Server) authenticatedAgent(r *http.Request) (string, bool) {
	id := r.Header.Get("X-Tunnel-Site")
	site, ok := s.store.site(id)
	authorization := r.Header.Get("Authorization")
	if !ok || !strings.HasPrefix(authorization, "Bearer ") {
		return id, false
	}
	hash := digest(strings.TrimPrefix(authorization, "Bearer "))
	instance := r.Header.Get("X-Tunnel-Instance")
	if instance == "" {
		return id, !site.LegacyDisabled && constantEqual(site.TokenHash, hash)
	}
	a, found := s.store.agent(instance)
	return id, found && a.SiteID == id && !a.Disabled && constantEqual(a.TokenHash, hash)
}

func (s *Server) receiveDiagnostic(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fail(w, 405, "POST required")
		return
	}
	id, ok := s.authenticatedAgent(r)
	if !ok {
		fail(w, 401, "invalid site credentials")
		return
	}
	var failure diagnostics.Failure
	if !decode(w, r, &failure) {
		return
	}
	rawID, err := hex.DecodeString(failure.AgentRequestID)
	if err != nil || len(rawID) != 8 || failure.Code == "" || len(failure.Code) > 40 || len(failure.Stage) > 40 || len(failure.Message) > 4096 || len(failure.Target) > 1024 || len(failure.ResolvedIPs) > 64 || len(failure.Attempts) > 64 {
		fail(w, 400, "invalid diagnostic report")
		return
	}
	failure.AgentInstanceID = r.Header.Get("X-Tunnel-Instance")
	if failure.AgentInstanceID == "" {
		failure.AgentInstanceID = "legacy"
	}
	s.diagnosticMu.Lock()
	defer s.diagnosticMu.Unlock()
	now := time.Now()
	for key, record := range s.diagnosticCache {
		if now.After(record.expires) {
			delete(s.diagnosticCache, key)
		}
	}
	if len(s.diagnosticCache) >= 1024 {
		var oldest string
		var expires time.Time
		for key, record := range s.diagnosticCache {
			if oldest == "" || record.expires.Before(expires) {
				oldest = key
				expires = record.expires
			}
		}
		delete(s.diagnosticCache, oldest)
	}
	s.diagnosticCache[id+":"+failure.AgentRequestID] = diagnosticRecord{failure: failure, expires: now.Add(5 * time.Minute)}
	w.WriteHeader(http.StatusNoContent)
}
func (s *Server) describeFailure(siteID string, err error) *diagnostics.Failure {
	failure := diagnostics.Describe(err)
	if failure.AgentRequestID == "" {
		return failure
	}
	s.diagnosticMu.Lock()
	record, ok := s.diagnosticCache[siteID+":"+failure.AgentRequestID]
	s.diagnosticMu.Unlock()
	if ok && time.Now().Before(record.expires) {
		full := record.failure
		return &full
	}
	return failure
}
