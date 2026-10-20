package control

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/XUJiahua/tunnel-mvp/internal/state"
)

type clusterPeerFlow struct {
	peer   state.ClusterNode
	lease  *authLease
	cancel context.CancelFunc
}
type clusterRuntime struct {
	server     *Server
	repo       state.ClusterRepository
	tls        *clusterTLS
	node       state.ClusterNode
	instanceID string
	lease      *authLease
	ctx        context.Context
	cancel     context.CancelFunc
	stopping   atomic.Bool
	listener   net.Listener
	http       *http.Server
	wg         sync.WaitGroup
	mu         sync.Mutex
	flows      map[string]clusterPeerFlow
	outgoing   map[string]net.Conn
	streamWG   sync.WaitGroup
}

func (s *Server) clusterPolicyHash() string {
	if s.cluster == nil {
		return ""
	}
	return state.ClusterProtocol + ":" + s.cluster.tls.caHash
}
func (s *Server) configureCluster() error {
	if s.cluster == nil {
		return nil
	}
	c := s.cluster
	repository, ok := s.store.Repository.(state.ClusterRepository)
	if !ok {
		return fmt.Errorf("cluster requires its PostgreSQL repository")
	}
	c.repo = repository
	c.server = s
	listener, err := net.Listen("tcp", s.cfg.Cluster.Listen)
	if err != nil {
		return fmt.Errorf("listen for cluster: %w", err)
	}
	ready := false
	defer func() {
		if !ready {
			listener.Close()
		}
	}()
	advertised, _ := url.Parse(s.cfg.Cluster.AdvertiseURL)
	if advertised.Port() == "0" {
		_, port, _ := net.SplitHostPort(listener.Addr().String())
		advertised.Host = net.JoinHostPort(advertised.Hostname(), port)
	}
	c.node = state.ClusterNode{ID: s.cfg.Cluster.NodeID, Epoch: randomID()[:32], URL: advertised.String(), Fingerprint: c.tls.fingerprint}
	started := time.Now()
	registerCtx, registerCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer registerCancel()
	snapshot, err := c.repo.ClusterRegister(registerCtx, c.node)
	if err != nil {
		return err
	}
	c.node, c.instanceID = snapshot.Self, snapshot.InstanceID
	c.ctx, c.cancel = context.WithCancel(context.Background())
	c.flows = map[string]clusterPeerFlow{}
	c.outgoing = map[string]net.Conn{}
	c.lease = newAuthLease(c.ctx, func() { s.fenceRuntime() }, clusterProofUntil(started, snapshot.Now, c.node.Expires))
	if !c.valid() {
		return state.ErrClusterLease
	}
	c.http = &http.Server{Handler: http.HandlerFunc(c.serveStream), ReadHeaderTimeout: 3 * time.Second, IdleTimeout: 10 * time.Second, MaxHeaderBytes: 16 << 10}
	c.listener = tls.NewListener(listener, c.tls.config)
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		if err := c.http.Serve(c.listener); err != nil && err != http.ErrServerClosed && c.ctx.Err() == nil {
			slog.Error("cluster listener stopped")
			s.fenceRuntime()
		}
	}()
	ready = true
	return nil
}
func clusterProofUntil(started, now, expires time.Time) time.Time {
	remaining := expires.Sub(now)
	if remaining > authorizationLease {
		remaining = authorizationLease
	}
	return started.Add(remaining)
}
func (c *clusterRuntime) valid() bool {
	return c != nil && !c.stopping.Load() && c.ctx != nil && c.ctx.Err() == nil && c.lease != nil && c.lease.valid()
}
func (s *Server) startCluster(ctx context.Context) {
	c := s.cluster
	if c == nil {
		return
	}
	s.backgroundWG.Add(1)
	go func() {
		defer s.backgroundWG.Done()
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-c.ctx.Done():
				return
			case <-tick.C:
				c.heartbeat(ctx)
			}
		}
	}()
}
func (c *clusterRuntime) heartbeat(ctx context.Context) {
	if !c.valid() {
		return
	}
	c.server.tunnelMu.Lock()
	reports := make([]state.ClusterAgent, 0, len(c.server.connections))
	captured := make(map[string]*agentConnection, len(c.server.connections))
	for key, agent := range c.server.connections {
		if !agent.closing.Load() {
			reports = append(reports, state.ClusterAgent{Key: key, Generation: agent.generation, Channels: agent.channels.Load()})
			captured[key] = agent
		}
	}
	c.server.tunnelMu.Unlock()
	for i := range reports {
		reports[i].Ready = c.server.tunnel.HasSession(captured[reports[i].Key].key)
	}
	started := time.Now()
	readCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	snapshot, err := c.repo.ClusterHeartbeat(readCtx, reports)
	cancel()
	if err != nil {
		return
	} // Independent local timers fence all flows at their deadline.
	c.lease.renew(clusterProofUntil(started, snapshot.Now, snapshot.Self.Expires))
	if !c.valid() {
		return
	}
	valid := map[string]uint64{}
	for _, route := range snapshot.Routes {
		valid[route.Agent.Key] = route.Agent.Generation
	}
	c.server.tunnelMu.Lock()
	for key, agent := range captured {
		if c.server.connections[key] == agent && valid[key] != agent.generation {
			agent.closing.Store(true)
			agent.cancel()
		}
	}
	c.server.tunnelMu.Unlock()
	peers := map[string]state.ClusterNode{}
	for _, node := range snapshot.Nodes {
		peers[node.ID] = node
	}
	c.mu.Lock()
	flows := make([]clusterPeerFlow, 0, len(c.flows))
	for _, flow := range c.flows {
		flows = append(flows, flow)
	}
	c.mu.Unlock()
	for _, flow := range flows {
		peer, ok := peers[flow.peer.ID]
		if !ok || peer.Epoch != flow.peer.Epoch || peer.Fingerprint != flow.peer.Fingerprint {
			flow.cancel()
			continue
		}
		flow.lease.renew(clusterProofUntil(started, snapshot.Now, peer.Expires))
	}
}
func (c *clusterRuntime) peerLease(parent context.Context, peer state.ClusterNode, started, now time.Time) (context.Context, func(), error) {
	ctx, cancel := context.WithCancel(parent)
	lease := newAuthLease(ctx, cancel, clusterProofUntil(started, now, peer.Expires))
	id := randomID()
	c.mu.Lock()
	if len(c.flows) >= 4096 {
		c.mu.Unlock()
		lease.stop()
		cancel()
		return nil, nil, state.ErrAuthCapacity
	}
	c.flows[id] = clusterPeerFlow{peer: peer, lease: lease, cancel: cancel}
	c.mu.Unlock()
	stopNode := context.AfterFunc(c.ctx, cancel)
	return ctx, func() {
		stopNode()
		lease.stop()
		cancel()
		c.mu.Lock()
		delete(c.flows, id)
		c.mu.Unlock()
	}, nil
}
func (c *clusterRuntime) stopAdmission() {
	c.stopping.Store(true)
	if c.lease != nil {
		c.lease.stop()
	}
	if c.cancel != nil {
		c.cancel()
	}
	c.mu.Lock()
	outgoing := make([]net.Conn, 0, len(c.outgoing))
	for _, conn := range c.outgoing {
		outgoing = append(outgoing, conn)
	}
	c.mu.Unlock()
	for _, conn := range outgoing {
		conn.Close()
	}
}
func (c *clusterRuntime) close() {
	c.stopAdmission()
	if c.http != nil {
		_ = c.http.Close()
	}
	c.wg.Wait()
	c.streamWG.Wait()
	if c.repo != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := c.repo.ClusterCloseNode(ctx); err != nil {
			slog.Warn("cluster node lease left to expire")
		}
	}
}
