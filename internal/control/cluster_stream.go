package control

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/XUJiahua/tunnel-mvp/internal/diagnostics"
	"github.com/XUJiahua/tunnel-mvp/internal/state"
)

const clusterStreamPath = "/_tunnel/internal/v1/stream"

type clusterAuthority struct{ Kind, AccessKey, SessionKey string }
type clusterRelayKey struct{}
type clusterAuthorityKey struct{}
type clusterAgentKey struct{}
type clusterStreamRequest struct {
	Protocol, InstanceID, SourceEpoch string
	OwnerID, OwnerEpoch, AgentKey     string
	Generation                        uint64
	ServiceID, Network, Target        string
	Authority                         clusterAuthority
}
type clusterRemoteConn struct {
	net.Conn
	reader  *bufio.Reader
	once    sync.Once
	onClose func()
}

func (c *clusterRemoteConn) Read(b []byte) (int, error) { return c.reader.Read(b) }
func (c *clusterRemoteConn) Close() error               { err := c.Conn.Close(); c.once.Do(c.onClose); return err }
func clusterTarget(svc Service) string {
	u, err := url.Parse(svc.URL)
	if err != nil {
		return ""
	}
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	return net.JoinHostPort(u.Hostname(), port)
}
func (c *clusterRuntime) dialRemote(ctx context.Context, view state.ClusterSnapshot, route state.ClusterRoute, service, network, address string) (net.Conn, error) {
	authority, ok := ctx.Value(clusterAuthorityKey{}).(clusterAuthority)
	if !ok {
		return nil, fmt.Errorf("cluster stream authority missing")
	}
	endpoint, err := url.Parse(route.Node.URL)
	if err != nil {
		return nil, err
	}
	cfg := c.tls.config.Clone()
	cfg.ServerName = endpoint.Hostname()
	cfg.VerifyConnection = func(cs tls.ConnectionState) error {
		if len(cs.PeerCertificates) == 0 {
			return fmt.Errorf("cluster owner certificate missing")
		}
		id, err := clusterCertificateID(cs.PeerCertificates[0])
		if err != nil || id != route.Node.ID || certificateFingerprint(cs.PeerCertificates[0]) != route.Node.Fingerprint {
			return fmt.Errorf("cluster owner identity mismatch")
		}
		return nil
	}
	port := endpoint.Port()
	if port == "" {
		port = "443"
	}
	conn, err := (&tls.Dialer{NetDialer: &net.Dialer{Timeout: 5 * time.Second}, Config: cfg}).DialContext(ctx, "tcp", net.JoinHostPort(endpoint.Hostname(), port))
	if err != nil {
		return nil, err
	}
	ready := false
	defer func() {
		if !ready {
			conn.Close()
		}
	}()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	deadline := time.Now().Add(5 * time.Second)
	if expires, ok := ctx.Deadline(); ok && expires.Before(deadline) {
		deadline = expires
	}
	conn.SetDeadline(deadline)
	envelope := clusterStreamRequest{Protocol: state.ClusterProtocol, InstanceID: view.InstanceID, SourceEpoch: c.node.Epoch, OwnerID: route.Node.ID, OwnerEpoch: route.Node.Epoch, AgentKey: route.Agent.Key, Generation: route.Agent.Generation, ServiceID: service, Network: network, Target: address, Authority: authority}
	raw, err := json.Marshal(envelope)
	if err != nil {
		return nil, err
	}
	if _, err = fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\nX-Tunnel-Stream: %s\r\n\r\n", clusterStreamPath, endpoint.Host, base64.RawURLEncoding.EncodeToString(raw)); err != nil {
		return nil, err
	}
	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodConnect})
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		var failure diagnostics.Failure
		_ = json.NewDecoder(io.LimitReader(response.Body, 8192)).Decode(&failure)
		response.Body.Close()
		if failure.Code != "" {
			return nil, &failure
		}
		return nil, fmt.Errorf("cluster owner rejected stream (%d)", response.StatusCode)
	}
	if !c.valid() || ctx.Err() != nil {
		return nil, state.ErrClusterLease
	}
	conn.SetDeadline(time.Time{})
	id := randomID()
	nodeCtx, nodeCancel := context.WithCancel(c.ctx)
	remote := &clusterRemoteConn{Conn: conn, reader: reader, onClose: func() { nodeCancel(); c.mu.Lock(); delete(c.outgoing, id); c.mu.Unlock() }}
	c.mu.Lock()
	if c.stopping.Load() {
		c.mu.Unlock()
		nodeCancel()
		return nil, state.ErrClusterLease
	}
	c.outgoing[id] = remote
	c.mu.Unlock()
	context.AfterFunc(nodeCtx, func() { remote.Close() })
	ready = true
	return remote, nil
}
func (c *clusterRuntime) serveStream(w http.ResponseWriter, r *http.Request) {
	if !c.valid() || c.draining.Load() || r.ProtoMajor != 1 || r.Method != http.MethodConnect || r.URL.Path != clusterStreamPath || r.ContentLength > 0 || len(r.TransferEncoding) > 0 || r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		fail(w, 403, "cluster stream rejected")
		return
	}
	sourceID, err := clusterCertificateID(r.TLS.PeerCertificates[0])
	if err != nil {
		fail(w, 403, "cluster peer rejected")
		return
	}
	encoded := r.Header.Values("X-Tunnel-Stream")
	if len(encoded) != 1 || len(encoded[0]) > 8192 {
		fail(w, 400, "invalid cluster envelope")
		return
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded[0])
	if err != nil {
		fail(w, 400, "invalid cluster envelope")
		return
	}
	var request clusterStreamRequest
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&request); err != nil || decoder.Decode(&struct{}{}) != io.EOF || request.Protocol != state.ClusterProtocol || request.InstanceID != c.instanceID || request.OwnerID != c.node.ID || request.OwnerEpoch != c.node.Epoch || request.Generation == 0 || request.Network != "tcp" {
		fail(w, 403, "cluster route rejected")
		return
	}
	proofStarted := time.Now()
	membership, err := c.repo.ClusterMembership(r.Context())
	if err != nil {
		fail(w, 503, "cluster membership unavailable")
		return
	}
	var peer state.ClusterNode
	for _, node := range membership.Nodes {
		if node.ID == sourceID && node.Epoch == request.SourceEpoch && node.Fingerprint == certificateFingerprint(r.TLS.PeerCertificates[0]) {
			peer = node
			break
		}
	}
	if peer.ID == "" || peer.ID == c.node.ID {
		fail(w, 403, "cluster peer is not a live remote member")
		return
	}
	svc, ok := c.server.store.service(request.ServiceID)
	if !ok || clusterTarget(svc) != request.Target {
		fail(w, 403, "cluster target rejected")
		return
	}
	routes, err := c.repo.ClusterRoutes(r.Context(), svc.SiteID)
	if err != nil {
		fail(w, 503, "cluster ownership unavailable")
		return
	}
	owned := false
	for _, route := range routes.Routes {
		if route.Agent.Key == request.AgentKey && route.Agent.Generation == request.Generation && route.Node.ID == c.node.ID && route.Node.Epoch == c.node.Epoch {
			owned = true
			break
		}
	}
	if !owned {
		fail(w, 409, "cluster generation changed")
		return
	}
	peerCtx, releasePeer, err := c.peerLease(r.Context(), peer, proofStarted, membership.Now)
	if err != nil {
		fail(w, 503, "cluster stream capacity reached")
		return
	}
	defer releasePeer()
	flowCtx, releaseAuth, err := c.authorizeStream(context.WithValue(peerCtx, clusterRelayKey{}, true), request, svc, proofStarted)
	if err != nil {
		fail(w, 403, "cluster application authority rejected")
		return
	}
	defer releaseAuth()
	if !c.valid() || flowCtx.Err() != nil {
		fail(w, 503, "cluster proof expired")
		return
	}
	target, err := c.server.dialLocalGeneration(flowCtx, svc.SiteID, svc.ID, request.AgentKey, request.Generation, request.Network, request.Target)
	if err != nil {
		jsonOut(w, 502, diagnostics.Describe(err))
		return
	}
	defer target.Close()
	if !c.valid() || flowCtx.Err() != nil {
		fail(w, 503, "cluster proof expired")
		return
	}
	// The receiver only dials its own exact generation. It never routes a second
	// hop and never accepts an arbitrary address from the sending node.
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		fail(w, 500, "cluster hijacking unavailable")
		return
	}
	c.mu.Lock()
	if c.stopping.Load() {
		c.mu.Unlock()
		fail(w, 503, "cluster stopping")
		return
	}
	c.streamWG.Add(1)
	c.mu.Unlock()
	defer c.streamWG.Done()
	source, buffer, err := hijacker.Hijack()
	if err != nil {
		return
	}
	defer source.Close()
	source.SetDeadline(time.Time{})
	stop := context.AfterFunc(flowCtx, func() { source.Close(); target.Close() })
	defer stop()
	if _, err = buffer.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	if err = buffer.Flush(); err != nil {
		return
	}
	done := make(chan struct{}, 1)
	go func() { _, _ = io.Copy(target, buffer); done <- struct{}{} }()
	_, _ = io.Copy(source, target)
	source.Close()
	target.Close()
	<-done
}
func (c *clusterRuntime) authorizeStream(parent context.Context, r clusterStreamRequest, svc Service, started time.Time) (context.Context, func(), error) {
	a := r.Authority
	if a.Kind == "system" {
		if a.AccessKey != "" || a.SessionKey != "" {
			return nil, nil, state.ErrAuthChanged
		}
		ctx, release := c.server.trackSystemRoute(parent, svc, started)
		return ctx, release, nil
	}
	accesses, sessions := []string{}, []string{}
	switch a.Kind {
	case "access":
		accesses = append(accesses, a.AccessKey)
	case "probe":
		sessions = append(sessions, a.SessionKey)
	default:
		return nil, nil, state.ErrAuthChanged
	}
	snapshot, err := c.server.auth.shared.AuthSnapshotForKeys(parent, accesses, sessions, []string{svc.ID})
	if err != nil {
		return nil, nil, err
	}
	grant := state.AuthGrant{SessionKey: a.SessionKey, ServiceID: svc.ID}
	if a.Kind == "access" {
		var ok bool
		grant, ok = snapshot.Data.Access[a.AccessKey]
		if !ok || grant.SessionKey != a.SessionKey || grant.ServiceID != svc.ID {
			return nil, nil, state.ErrAuthChanged
		}
	}
	sess, ok := snapshot.Data.Sessions[a.SessionKey]
	if !ok || !c.server.authIdentityAllowed(snapshot.Data, sess, snapshot.Now) {
		return nil, nil, state.ErrAuthChanged
	}
	var user User
	found := false
	for _, u := range snapshot.State.Users {
		if u.ID == sess.UserID && !u.Disabled {
			user = effectiveUser(snapshot.State, u)
			found = true
			break
		}
	}
	var current Service
	for _, v := range snapshot.State.Services {
		if v.ID == svc.ID {
			current = v
			break
		}
	}
	if !found || !sameRoute(svc, current) || !canAccessService(user, current) && !(a.Kind == "probe" && canManage(user, current.OrganizationID, "applications:manage")) {
		return nil, nil, state.ErrAuthChanged
	}
	if a.Kind == "probe" {
		grant.Expires = sess.Expires
	}
	resolution := state.AuthResolution{Session: sess, Grant: grant, State: snapshot.State, Now: snapshot.Now, Found: true, Identity: snapshot.Data.Identities[sess.UserID]}
	ctx, release := c.server.trackRequest(parent, user, grant, a.AccessKey, resolution, started)
	return ctx, release, nil
}
func (s *Server) trackSystemRoute(parent context.Context, svc Service, started time.Time) (context.Context, func()) {
	ctx, cancel := context.WithCancel(parent)
	lease := newAuthLease(ctx, cancel, started.Add(authorizationLease))
	id := randomID()
	s.liveMu.Lock()
	s.live[id] = liveRequest{relay: true, system: true, route: svc, serviceID: svc.ID, cancel: func() { lease.stop(); cancel() }, lease: lease}
	s.liveMu.Unlock()
	return ctx, func() { lease.stop(); cancel(); s.liveMu.Lock(); delete(s.live, id); s.liveMu.Unlock() }
}
