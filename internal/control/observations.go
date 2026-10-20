package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/XUJiahua/tunnel-mvp/internal/diagnostics"
	"github.com/XUJiahua/tunnel-mvp/internal/state"
)

type observationRuntime struct {
	server      *Server
	repo        state.ObservationRepository
	mu          sync.Mutex
	statusMu    sync.Mutex
	lastSuccess time.Time
	failed      bool
}
type observationNodeReport struct {
	Version       string             `json:"version"`
	Protocol      string             `json:"protocol"`
	Extension     int                `json:"extension"`
	Draining      bool               `json:"draining"`
	Requests      uint64             `json:"requests"`
	Errors        uint64             `json:"errors"`
	Probes        uint64             `json:"probes"`
	ActiveIngress int                `json:"activeIngress"`
	ActiveRelays  int                `json:"activeRelays"`
	Agents        int                `json:"agents"`
	AgentChannels int64              `json:"agentChannels"`
	Backlog       observationBacklog `json:"backlog"`
	LastSuccess   time.Time          `json:"lastSuccess"`
	Failed        bool               `json:"failed"`
}

func (s *Server) prepareObservations() error {
	if s.cluster == nil {
		return nil
	}
	repository, ok := s.store.Repository.(state.ObservationRepository)
	if !ok {
		return fmt.Errorf("cluster observation repository missing")
	}
	limit := s.cfg.Cluster.ObservationBufferMB
	if limit == 0 {
		limit = 256
	}
	if err := s.history.enableObservations(s.cfg.Cluster.NodeID, int64(limit)<<20); err != nil {
		return fmt.Errorf("prepare durable observation queue: %w", err)
	}
	s.observer = &observationRuntime{server: s, repo: repository}
	return nil
}
func (s *Server) startObservations(ctx context.Context) {
	if s.observer == nil {
		return
	}
	s.backgroundWG.Add(1)
	go func() {
		defer s.backgroundWG.Done()
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-s.cluster.ctx.Done():
				return
			case <-ticker.C:
				s.observer.sync(ctx)
			}
		}
	}()
}
func (o *observationRuntime) sync(parent context.Context) {
	if !o.mu.TryLock() {
		return
	}
	defer o.mu.Unlock()
	s := o.server
	if !s.cluster.valid() {
		return
	}
	failed := false
	// Database failure cannot lose acknowledged local facts: commit PG first,
	// then delete only the captured AUTOINCREMENT sequence range in SQLite.
	for range 2 {
		ctx, cancel := context.WithTimeout(parent, 750*time.Millisecond)
		pending, err := s.history.pendingObservations(ctx)
		cancel()
		if err != nil {
			failed = true
			break
		}
		if len(pending) == 0 {
			break
		}
		records := make([]state.Observation, len(pending))
		for i, v := range pending {
			records[i] = v.Record
		}
		ctx, cancel = context.WithTimeout(parent, 1500*time.Millisecond)
		err = o.repo.ObservationPublish(ctx, records)
		cancel()
		if err != nil {
			failed = true
			break
		}
		ctx, cancel = context.WithTimeout(parent, 750*time.Millisecond)
		err = s.history.acknowledgeObservations(ctx, pending[len(pending)-1].Sequence)
		cancel()
		if err != nil {
			failed = true
			break
		}
	}
	// Repair the index once, then durably enqueue one bounded page per tick.
	// Publication continues between repair pages, even when retained JSONL is
	// larger than the configured buffer.
	if s.history.auditDirty.Load() || s.history.auditReconcile.Load() {
		s.auditMu.Lock()
		var err error
		if s.history.auditDirty.Load() {
			err = s.rebuildAuditIndex()
		}
		if err == nil {
			err = s.history.reconcileObservations()
		}
		s.auditMu.Unlock()
		if err != nil {
			failed = true
		}
	}
	o.statusMu.Lock()
	o.failed = failed
	if !failed {
		o.lastSuccess = time.Now().UTC()
	}
	o.statusMu.Unlock()
	ctx, cancel := context.WithTimeout(parent, 750*time.Millisecond)
	report, err := o.report(ctx)
	cancel()
	if err != nil {
		return
	}
	raw, err := json.Marshal(report)
	if err != nil {
		return
	}
	ctx, cancel = context.WithTimeout(parent, 750*time.Millisecond)
	err = o.repo.ObservationReport(ctx, raw)
	cancel()
	if err != nil {
		o.statusMu.Lock()
		o.failed = true
		o.statusMu.Unlock()
	}
}
func (o *observationRuntime) report(ctx context.Context) (observationNodeReport, error) {
	s := o.server
	r := observationNodeReport{Version: s.cfg.RuntimeVersion, Protocol: state.ClusterProtocol, Extension: state.ObservationVersion, Draining: s.cluster.draining.Load(), Requests: s.stats.requests.Load(), Errors: s.stats.errors.Load(), Probes: s.stats.probes.Load()}
	if r.Version == "" {
		r.Version = "dev"
	}
	s.liveMu.Lock()
	for _, request := range s.live {
		if request.relay {
			r.ActiveRelays++
		} else {
			r.ActiveIngress++
		}
	}
	s.liveMu.Unlock()
	s.tunnelMu.Lock()
	for _, agent := range s.connections {
		if !agent.closing.Load() {
			r.Agents++
			r.AgentChannels += agent.channels.Load()
		}
	}
	s.tunnelMu.Unlock()
	var err error
	r.Backlog, err = s.history.observationBacklog(ctx)
	o.statusMu.Lock()
	r.LastSuccess, r.Failed = o.lastSuccess, o.failed
	o.statusMu.Unlock()
	return r, err
}
func observationSession(r *http.Request) string {
	cookie, err := r.Cookie(consoleCookie)
	if err != nil {
		return ""
	}
	return digest(cookie.Value)
}
func (s *Server) observationUser(a state.AuthResolution) (User, error) {
	u, ok := s.resolvedUser(a)
	if !ok {
		return User{}, state.ErrAuthChanged
	}
	return u, nil
}
func (s *Server) clusterAudit(w http.ResponseWriter, r *http.Request, f auditFilters) {
	query := state.ObservationAuditFilter{Epoch: f.Epoch, Event: f.Event, User: f.User, Site: f.Site, Resource: f.Resource, Node: r.URL.Query().Get("nodeID"), Before: f.Before, Limit: f.Limit}
	if f.Since != "" {
		query.Since, _ = time.Parse(time.RFC3339Nano, f.Since)
	}
	page, err := s.observer.repo.ObservationAudit(r.Context(), observationSession(r), query, func(a state.AuthResolution) (state.ObservationScope, error) {
		u, err := s.observationUser(a)
		if err != nil {
			return state.ObservationScope{}, err
		}
		if !hasPermission(u, "audit:read") {
			return state.ObservationScope{}, state.ErrAuthChanged
		}
		return state.ObservationScope{All: u.Role == "admin", OrganizationID: u.OrganizationID}, nil
	})
	if err != nil {
		if errors.Is(err, state.ErrObservationCursor) {
			fail(w, 409, err.Error())
		} else {
			fail(w, 503, "cluster audit unavailable or authorization changed")
		}
		return
	}
	cursor := ""
	if page.Next > 0 {
		cursor = page.Epoch + ":" + strconv.FormatInt(page.Next, 10)
	}
	jsonOut(w, 200, map[string]any{"entries": page.Entries, "nextCursor": cursor, "scope": "cluster"})
}
func (s *Server) clusterSamples(r *http.Request, id string, since time.Time, limit int) ([]healthSample, error) {
	raws, err := s.observer.repo.ObservationSamples(r.Context(), observationSession(r), id, since, limit, func(a state.AuthResolution, svc Service) (string, error) {
		u, err := s.observationUser(a)
		if err != nil {
			return "", err
		}
		if !canAccessService(u, svc) && !canManage(u, svc.OrganizationID, "applications:manage") {
			return "", state.ErrAuthChanged
		}
		return healthProfile(svc), nil
	})
	if err != nil {
		return nil, err
	}
	out := []healthSample{}
	for _, raw := range raws {
		var sample healthSample
		if err = json.Unmarshal(raw, &sample); err != nil {
			return nil, err
		}
		out = append(out, sample)
	}
	return out, nil
}
func (s *Server) clusterStatus(w http.ResponseWriter, r *http.Request, _ User) {
	if s.observer == nil {
		jsonOut(w, 200, map[string]any{"enabled": false})
		return
	}
	nodes, err := s.observer.repo.ObservationNodesAuthorized(r.Context(), observationSession(r), func(a state.AuthResolution) error {
		u, e := s.observationUser(a)
		if e != nil {
			return e
		}
		if u.Role != "admin" {
			return state.ErrAuthChanged
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, state.ErrAuthChanged) {
			fail(w, 403, "global administrator required")
		} else {
			fail(w, 503, "cluster status unavailable")
		}
		return
	}
	jsonOut(w, 200, map[string]any{"enabled": true, "localNodeID": s.cluster.node.ID, "protocol": state.ClusterProtocol, "observationVersion": state.ObservationVersion, "nodes": nodes})
}
func (s *Server) drainCluster(w http.ResponseWriter, r *http.Request, _ User) {
	if s.observer == nil {
		fail(w, 400, "cluster mode required")
		return
	}
	// This endpoint is local by design: the response identifies the actual node.
	// Operators address its console listener directly while retaining public Host.
	var body struct {
		Draining bool   `json:"draining"`
		NodeID   string `json:"nodeID"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.NodeID != s.cluster.node.ID {
		fail(w, 409, "nodeID does not match this listener")
		return
	}
	actor := ""
	err := s.observer.repo.ClusterDrain(r.Context(), observationSession(r), body.Draining, func(a state.AuthResolution) error {
		u, e := s.observationUser(a)
		if e != nil {
			return e
		}
		if u.Role != "admin" {
			return state.ErrAuthChanged
		}
		actor = u.Username
		return nil
	})
	if err != nil {
		if errors.Is(err, state.ErrAuthChanged) {
			fail(w, 403, "global administrator required")
		} else {
			fail(w, 503, "drain state could not be committed")
		}
		return
	}

	s.cluster.draining.Store(body.Draining)
	s.audit("cluster_drain_changed", actor, "", "", map[string]any{"draining": body.Draining})
	jsonOut(w, 200, map[string]any{"nodeID": s.cluster.node.ID, "draining": body.Draining})
}
func (s *Server) observationMetrics(w http.ResponseWriter, r *http.Request) bool {
	var nodes []state.ObservationNode
	var err error
	if r.URL.Path == "/metrics" {
		nodes, err = s.observer.repo.ObservationNodes(r.Context())
	} else {
		nodes, err = s.observer.repo.ObservationNodesAuthorized(r.Context(), observationSession(r), s.observationAdmin)
	}
	if err != nil {
		fail(w, 503, "cluster observation metrics unavailable")
		return false
	}
	fmt.Fprint(w, "# HELP tunnel_cluster_node_up Node database lease is live.\n# TYPE tunnel_cluster_node_up gauge\n# HELP tunnel_cluster_node_reporting Node has a fresh observation report.\n# TYPE tunnel_cluster_node_reporting gauge\n# HELP tunnel_cluster_node_draining Node is draining new connections.\n# TYPE tunnel_cluster_node_draining gauge\n# HELP tunnel_cluster_node_requests_total Logical ingress requests by node incarnation.\n# TYPE tunnel_cluster_node_requests_total counter\n# HELP tunnel_cluster_node_pending Durable local observations awaiting publication.\n# TYPE tunnel_cluster_node_pending gauge\n")
	for _, node := range nodes {
		up, reporting, draining := 0, 0, 0
		if node.Alive {
			up = 1
		}
		if node.Fresh {
			reporting = 1
		}
		if node.Draining {
			draining = 1
		}
		fmt.Fprintf(w, "tunnel_cluster_node_up{node_id=%q,node_epoch=%q} %d\ntunnel_cluster_node_reporting{node_id=%q,node_epoch=%q} %d\ntunnel_cluster_node_draining{node_id=%q,node_epoch=%q} %d\n", node.NodeID, node.Epoch, up, node.NodeID, node.Epoch, reporting, node.NodeID, node.Epoch, draining)
		if node.Fresh {
			var report observationNodeReport
			if json.Unmarshal(node.Report, &report) == nil {
				fmt.Fprintf(w, "tunnel_cluster_node_requests_total{node_id=%q,node_epoch=%q} %d\ntunnel_cluster_node_pending{node_id=%q,node_epoch=%q} %d\n", node.NodeID, node.Epoch, report.Requests, node.NodeID, node.Epoch, report.Backlog.Count)
			}
		}
	}

	return true
}

func clusterReport(svc Service, p healthSample) probeReport {
	r := probeReport{NodeID: p.NodeID, NodeEpoch: p.NodeEpoch, Reachable: p.Reachable, Healthy: p.Healthy, RequestID: p.RequestID, SiteID: svc.SiteID, ServiceID: svc.ID, Target: svc.URL, Status: p.Status, LatencyMs: p.LatencyMs, CheckedAt: p.CheckedAt}
	if p.Code != "" {
		r.Failure = diagnostics.New(p.Code, p.Stage, "Cluster sample; run a new probe for current details")
	}
	return r
}

func (s *Server) latestClusterReports(r *http.Request, internal bool) ([]probeReport, error) {
	var rows []state.ObservationHealth
	var err error
	if internal {
		rows, err = s.observer.repo.ObservationLatestInternal(r.Context(), healthProfile)
	} else {
		rows, err = s.observer.repo.ObservationLatest(r.Context(), observationSession(r), func(a state.AuthResolution, svc Service) (string, bool, error) {
			u, e := s.observationUser(a)
			if e != nil {
				return "", false, e
			}
			if r.URL.Path == "/api/metrics" && u.Role != "admin" {
				return "", false, state.ErrAuthChanged
			}
			return healthProfile(svc), canAccessService(u, svc) || canManage(u, svc.OrganizationID, "applications:manage"), nil
		})
	}
	if err != nil {
		return nil, err
	}
	out := []probeReport{}
	for _, row := range rows {
		var sample healthSample
		if err = json.Unmarshal(row.Payload, &sample); err != nil {
			return nil, err
		}
		out = append(out, clusterReport(row.Service, sample))
	}
	return out, nil
}

func (s *Server) observationAdmin(a state.AuthResolution) error {
	u, err := s.observationUser(a)
	if err != nil {
		return err
	}
	if u.Role != "admin" {
		return state.ErrAuthChanged
	}
	return nil
}
