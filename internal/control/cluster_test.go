package control

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/XUJiahua/tunnel-mvp/internal/agent"
	"github.com/XUJiahua/tunnel-mvp/internal/config"
	"github.com/XUJiahua/tunnel-mvp/internal/state"
	"github.com/XUJiahua/tunnel-mvp/internal/testpostgres"
	"github.com/gorilla/websocket"
)

type partitionedClusterRepository struct {
	*state.PostgresRepository
	blocked               atomic.Bool
	lostObservationCommit atomic.Bool
}

func (p *partitionedClusterRepository) ClusterHeartbeat(ctx context.Context, reports []state.ClusterAgent) (state.ClusterSnapshot, error) {
	if p.blocked.Load() {
		<-ctx.Done()
		return state.ClusterSnapshot{}, ctx.Err()
	}
	return p.PostgresRepository.ClusterHeartbeat(ctx, reports)
}

func (p *partitionedClusterRepository) ClusterReleaseAgent(ctx context.Context, key string, generation uint64) error {
	if p.blocked.Load() {
		return context.DeadlineExceeded
	}
	return p.PostgresRepository.ClusterReleaseAgent(ctx, key, generation)
}

// Simulate a lost commit response, after a real database transaction committed.
func (p *partitionedClusterRepository) ObservationPublish(ctx context.Context, records []state.Observation) error {
	err := p.PostgresRepository.ObservationPublish(ctx, records)
	if err != nil {
		return err
	}
	if p.lostObservationCommit.Swap(false) {
		return context.DeadlineExceeded
	}
	return nil
}

type clusterFixture struct {
	ca clusterTestCA
	*fixture
	nodes               [2]*Server
	selected, agentNode atomic.Int32
	replica             *partitionedClusterRepository
}

func newClusterFixture(t *testing.T) *clusterFixture {
	t.Helper()
	dsn := testpostgres.Database(t)
	t.Setenv("TUNNEL_POSTGRES_DSN", dsn)
	ca := newClusterTestCA(t)
	lb := httptest.NewUnstartedServer(nil)
	f := &clusterFixture{ca: ca}
	f.agentNode.Store(1)
	handlers := make([]*httputil.ReverseProxy, 2)
	for i, id := range []string{"node-a", "node-b"} {
		dir := t.TempDir()
		cfg := config.Server{PublicURL: "http://" + lb.Listener.Addr().String(), AccessURL: "http://apps.localhost:" + strconv.Itoa(lb.Listener.Addr().(*net.TCPAddr).Port), Storage: state.Postgres, DataDir: dir, Cluster: ca.node(t, id)}
		var s *Server
		var err error
		if i == 1 {
			st, e := openServingStore(dir, state.Postgres, "admin", testPassword, true)
			if e != nil {
				t.Fatal(e)
			}
			f.replica = &partitionedClusterRepository{PostgresRepository: st.Repository.(*state.PostgresRepository)}
			s, err = newServer(cfg, "", f.replica)
		} else {
			s, err = New(cfg, testPassword)
		}
		if err != nil {
			t.Fatal(err)
		}
		f.nodes[i] = s
		t.Cleanup(s.Close)
		h := httptest.NewServer(s.Handler())
		t.Cleanup(h.Close)
		u, _ := url.Parse(h.URL)
		handlers[i] = httputil.NewSingleHostReverseProxy(u)
	}
	lb.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := f.selected.Load()
		if strings.HasPrefix(r.URL.Path, "/tunnel") {
			n = f.agentNode.Load()
		}
		handlers[n].ServeHTTP(w, r)
	})
	lb.Start()
	t.Cleanup(lb.Close)
	f.fixture = &fixture{s: f.nodes[0], h: lb}
	response, body := f.request(t, "POST", "/api/login", map[string]string{"username": "admin", "password": testPassword}, nil, "")
	if response.StatusCode != 200 {
		t.Fatalf("login %d %s", response.StatusCode, body)
	}
	f.cookie = response.Cookies()[0]
	return f
}
func (f *clusterFixture) connectAgent(t *testing.T, site Site, token string, ports ...int) context.CancelFunc {
	return f.connectAgentInstance(t, site, "", token, ports...)
}
func (f *clusterFixture) connectAgentInstance(t *testing.T, site Site, instance, token string, ports ...int) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- agent.Run(ctx, config.Agent{ServerURL: f.h.URL, SiteID: site.ID, InstanceID: instance, RuntimeVersion: "cluster-test", Allow: []config.Rule{{CIDR: "127.0.0.0/8", Ports: ports}}}, token)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(3 * time.Second):
			t.Error("Agent did not stop")
		}
	})
	waitFor(t, func() bool {
		view, err := f.nodes[0].cluster.repo.ClusterRoutes(context.Background(), site.ID)
		if err != nil {
			return false
		}
		for _, route := range view.Routes {
			if route.Agent.InstanceID == instance {
				return true
			}
		}
		return false
	})
	return cancel
}
func clusterUpstream(t *testing.T, posts *atomic.Int32) *httptest.Server {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/events":
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: ready\n\n")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		case "/ws":
			c, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer c.Close()
			for {
				kind, b, err := c.ReadMessage()
				if err != nil {
					return
				}
				if c.WriteMessage(kind, b) != nil {
					return
				}
			}
		case "/commit":
			posts.Add(1)
			w.WriteHeader(200)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		default:
			io.Copy(w, r.Body)
			fmt.Fprint(w, "cluster-ok")
		}
	}))
	t.Cleanup(up.Close)
	return up
}
func clusterStreams(t *testing.T, f *clusterFixture, svc Service, cookie *http.Cookie, host string) (*http.Response, *websocket.Conn) {
	t.Helper()
	req, _ := http.NewRequest("GET", f.h.URL+"/events", nil)
	req.Host = host
	req.AddCookie(cookie)
	stream, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stream.Body.Close() })
	if stream.StatusCode != 200 {
		body, _ := io.ReadAll(stream.Body)
		t.Fatalf("SSE %d %s", stream.StatusCode, body)
	}
	if _, err = io.ReadFull(stream.Body, make([]byte, 13)); err != nil {
		t.Fatal(err)
	}
	headers := http.Header{"Host": {host}, "Cookie": {cookie.String()}, "Origin": {f.s.serviceURL(svc.ID).String()}}
	ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(f.h.URL, "http")+"/ws", headers)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.Close() })
	if err = ws.WriteMessage(websocket.TextMessage, []byte("cross-node")); err != nil {
		t.Fatal(err)
	}
	if _, b, err := ws.ReadMessage(); err != nil || string(b) != "cross-node" {
		t.Fatal("cross-node WebSocket", err)
	}
	return stream, ws
}
func assertClusterStreamsClosed(t *testing.T, stream *http.Response, ws *websocket.Conn) {
	t.Helper()
	done := make(chan error, 1)
	go func() { _, err := io.ReadAll(stream.Body); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("SSE ended without revocation error")
		}
	case <-time.After(7 * time.Second):
		t.Fatal("SSE proof did not fence stream")
	}
	ws.SetReadDeadline(time.Now().Add(time.Second))
	if _, _, err := ws.ReadMessage(); err == nil {
		t.Fatal("WebSocket remained open")
	} else {
		var timeout net.Error
		if errors.As(err, &timeout) && timeout.Timeout() {
			t.Fatal("WebSocket timed out instead of being closed")
		}
	}
}
func TestPostgresClusterRealRoutingAndRevocation(t *testing.T) {
	f := newClusterFixture(t)
	var posts atomic.Int32
	up := clusterUpstream(t, &posts)
	site, _ := f.site(t, "HA site")
	registered, token := registerAgent(t, f.fixture, site, "registered remote Agent")
	f.connectAgentInstance(t, site, registered.ID, token, portOf(up.URL))
	svc := f.service(t, site, "HA app", up.URL)
	cookie, host, _ := f.grant(t, svc, f.cookie)
	if len(f.nodes[0].connectedAgents(site.ID)) != 0 {
		t.Fatal("test requires Agent exclusively on B")
	}
	for _, node := range []int32{0, 1, 0} {
		f.selected.Store(node)
		res, body := f.request(t, "POST", "/upload", map[string]string{"data": "payload"}, cookie, host)
		if res.StatusCode != 200 || !strings.Contains(string(body), "payload") || !strings.HasSuffix(string(body), "cluster-ok") {
			t.Fatalf("node %d proxy %d %s", node, res.StatusCode, body)
		}
	}
	f.selected.Store(0)
	stream, ws := clusterStreams(t, f, svc, cookie, host)
	f.selected.Store(1)
	res, body := f.request(t, "POST", "/api/logout", nil, f.cookie, "")
	if res.StatusCode != 200 {
		t.Fatalf("logout %d %s", res.StatusCode, body)
	}
	assertClusterStreamsClosed(t, stream, ws)
	f.selected.Store(0)
	res, _ = f.request(t, "GET", "/", nil, cookie, host)
	if res.StatusCode == 200 {
		t.Fatal("revoked grant accepted on A")
	}
	res, body = f.request(t, "POST", "/api/login", map[string]string{"username": "admin", "password": testPassword}, nil, "")
	if res.StatusCode != 200 {
		t.Fatalf("relogin %d %s", res.StatusCode, body)
	}
	f.cookie = res.Cookies()[0]
	res, body = f.request(t, "GET", "/api/agents", nil, f.cookie, "")
	if res.StatusCode != 200 || !strings.Contains(string(body), registered.ID) || !strings.Contains(string(body), "cluster-test") || strings.Contains(string(body), digest(token)) {
		t.Fatalf("remote registered inventory %d %s", res.StatusCode, body)
	}
	res, body = f.request(t, "PATCH", "/api/sites/"+site.ID+"/agents/"+registered.ID, map[string]bool{"disabled": true}, f.cookie, "")
	if res.StatusCode != 200 {
		t.Fatalf("disable remote Agent %d %s", res.StatusCode, body)
	}
	view, err := f.nodes[0].cluster.repo.ClusterRoutes(context.Background(), site.ID)
	if err != nil || len(view.Routes) != 0 {
		t.Fatal("disabled registration retained route", err)
	}
	waitFor(t, func() bool { return len(f.nodes[1].connectedAgents(site.ID)) == 0 })
}
func TestPostgresClusterPartitionFencesBeforeTakeoverWithoutPostReplay(t *testing.T) {
	f := newClusterFixture(t)
	var posts atomic.Int32
	up := clusterUpstream(t, &posts)
	site, token := f.site(t, "HA failover")
	stopOld := f.connectAgent(t, site, token, portOf(up.URL))
	svc := f.service(t, site, "HA app", up.URL)
	cookie, host, _ := f.grant(t, svc, f.cookie)
	stream, ws := clusterStreams(t, f, svc, cookie, host)
	req, _ := http.NewRequest("POST", f.h.URL+"/commit", strings.NewReader("irreversible"))
	req.Host = host
	req.AddCookie(cookie)
	commitStarted := time.Now()
	commit, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer commit.Body.Close()
	if time.Since(commitStarted) > 3*time.Second {
		t.Fatal("SSE serialized another request in shared mode")
	}
	if commit.StatusCode != 200 {
		t.Fatal("POST never reached upstream", commit.StatusCode)
	}
	view, err := f.nodes[0].cluster.repo.ClusterRoutes(context.Background(), site.ID)
	if err != nil || len(view.Routes) != 1 {
		t.Fatal(err)
	}
	oldGeneration := view.Routes[0].Agent.Generation
	began := time.Now()
	f.replica.blocked.Store(true)
	assertClusterStreamsClosed(t, stream, ws)
	waitFor(t, func() bool { return !f.nodes[1].cluster.valid() })
	if time.Since(began) > 7*time.Second {
		t.Fatal("local node deadline missed")
	}
	_, _ = io.ReadAll(commit.Body)
	if posts.Load() != 1 {
		t.Fatal("in-flight POST replayed", posts.Load())
	}
	stopOld()
	deadline := time.Now().Add(12 * time.Second)
	for f.nodes[0].siteOnline(site.ID) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if f.nodes[0].siteOnline(site.ID) {
		t.Fatal("database owner lease did not expire")
	}
	f.agentNode.Store(0)
	f.connectAgent(t, site, token, portOf(up.URL))
	view, err = f.nodes[0].cluster.repo.ClusterRoutes(context.Background(), site.ID)
	if err != nil || len(view.Routes) != 1 || view.Routes[0].Node.ID != "node-a" || view.Routes[0].Agent.Generation <= oldGeneration {
		t.Fatal("takeover did not get newer generation", err)
	}
	res, body := f.request(t, "GET", "/", nil, cookie, host)
	if res.StatusCode != 200 || string(body) != "cluster-ok" {
		t.Fatalf("takeover traffic %d %s", res.StatusCode, body)
	}
	f.replica.blocked.Store(false)
	if _, err = f.replica.ClusterHeartbeat(context.Background(), nil); err == nil {
		t.Fatal("stale owner revived")
	}
	if posts.Load() != 1 {
		t.Fatal("POST replayed after takeover")
	}
}
func TestPostgresClusterRejectsForgedStreams(t *testing.T) {
	f := newClusterFixture(t)
	var posts atomic.Int32
	up := clusterUpstream(t, &posts)
	site, token := f.site(t, "HA identities")
	f.connectAgent(t, site, token, portOf(up.URL))
	svc := f.service(t, site, "HA app", up.URL)
	source, owner := f.nodes[0].cluster, f.nodes[1].cluster
	view, err := source.repo.ClusterRoutes(context.Background(), site.ID)
	if err != nil || len(view.Routes) != 1 {
		t.Fatal(err)
	}
	route := view.Routes[0]
	envelope := clusterStreamRequest{Protocol: state.ClusterProtocol, InstanceID: view.InstanceID, SourceEpoch: source.node.Epoch, OwnerID: owner.node.ID, OwnerEpoch: owner.node.Epoch, AgentKey: route.Agent.Key, Generation: route.Agent.Generation, ServiceID: svc.ID, Network: "tcp", Target: clusterTarget(svc), Authority: clusterAuthority{Kind: "system"}}
	for _, test := range []struct {
		name   string
		mutate func(*clusterStreamRequest)
	}{
		{"old generation", func(r *clusterStreamRequest) { r.Generation++ }}, {"arbitrary target", func(r *clusterStreamRequest) { r.Target = "127.0.0.1:1" }}, {"old peer epoch", func(r *clusterStreamRequest) { r.SourceEpoch = "old" }}, {"other database", func(r *clusterStreamRequest) { r.InstanceID = "other" }}, {"missing grant", func(r *clusterStreamRequest) {
			r.Authority = clusterAuthority{Kind: "access", AccessKey: strings.Repeat("a", 64), SessionKey: strings.Repeat("b", 64)}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := envelope
			test.mutate(&r)
			raw, _ := json.Marshal(r)
			cfg := source.tls.config.Clone()
			cfg.ServerName = "127.0.0.1"
			conn, err := tls.Dial("tcp", owner.listener.Addr().String(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(4 * time.Second))
			fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: internal\r\nX-Tunnel-Stream: %s\r\n\r\n", clusterStreamPath, base64.RawURLEncoding.EncodeToString(raw))
			res, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: "CONNECT"})
			if err != nil {
				t.Fatal(err)
			}
			res.Body.Close()
			if res.StatusCode == 200 {
				t.Fatal("forged stream accepted")
			}
		})
	}
	// A valid CA signature alone is insufficient: the exact leaf fingerprint must
	// belong to a currently registered incarnation.

	forgedConfig := f.ca.node(t, "node-a")
	forgedTLS, err := loadClusterTLS(forgedConfig)
	if err != nil {
		t.Fatal(err)
	}
	forgedTLS.config.ServerName = "127.0.0.1"
	forged, err := tls.Dial("tcp", owner.listener.Addr().String(), forgedTLS.config)
	if err != nil {
		t.Fatal(err)
	}
	forged.SetDeadline(time.Now().Add(3 * time.Second))
	rawEnvelope, _ := json.Marshal(envelope)
	fmt.Fprintf(forged, "CONNECT %s HTTP/1.1\r\nHost: internal\r\nX-Tunnel-Stream: %s\r\n\r\n", clusterStreamPath, base64.RawURLEncoding.EncodeToString(rawEnvelope))
	rejected, err := http.ReadResponse(bufio.NewReader(forged), &http.Request{Method: "CONNECT"})
	if err != nil {
		t.Fatal(err)
	}
	rejected.Body.Close()
	forged.Close()
	if rejected.StatusCode != 403 {
		t.Fatal("unregistered leaf fingerprint accepted", rejected.StatusCode)
	}
	cfg := newClusterTestCA(t).node(t, "intruder")
	untrusted, err := loadClusterTLS(cfg)
	if err != nil {
		t.Fatal(err)
	}
	untrusted.config.RootCAs = source.tls.config.RootCAs
	untrusted.config.ServerName = "127.0.0.1"
	conn, err := tls.Dial("tcp", owner.listener.Addr().String(), untrusted.config)
	if err == nil {
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(time.Second))
		fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: internal\r\n\r\n")
		if _, err = http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: "GET"}); err == nil {
			t.Fatal("untrusted client obtained HTTP response")
		}
	}
	req, _ := http.NewRequest("CONNECT", f.h.URL+clusterStreamPath, nil)
	raw, _ := json.Marshal(envelope)
	req.Header.Set("X-Tunnel-Stream", base64.RawURLEncoding.EncodeToString(raw))
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode == 200 {
		t.Fatal("public header granted private stream")
	}
}

func TestPostgresClusterKeepsUpstreamTLSVerification(t *testing.T) {
	f := newClusterFixture(t)
	up := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "verified-upstream") }))
	t.Cleanup(up.Close)
	site, token := f.site(t, "TLS site")
	f.connectAgent(t, site, token, portOf(up.URL))
	svc := f.service(t, site, "TLS app", up.URL)
	ctx := context.WithValue(context.Background(), clusterAuthorityKey{}, clusterAuthority{Kind: "system"})
	for _, trusted := range []bool{false, true} {
		conn, err := f.s.dialSite(ctx, site.ID, svc.ID, "tcp", clusterTarget(svc))
		if err != nil {
			t.Fatal(err)
		}
		roots := x509.NewCertPool()
		if trusted {
			roots.AddCert(up.Certificate())
		}
		inner := tls.Client(conn, &tls.Config{RootCAs: roots, ServerName: "127.0.0.1", MinVersion: tls.VersionTLS12})
		inner.SetDeadline(time.Now().Add(3 * time.Second))
		err = inner.Handshake()
		if !trusted {
			inner.Close()
			if err == nil {
				t.Fatal("untrusted upstream accepted")
			}
			continue
		}
		if err != nil {
			inner.Close()
			t.Fatal(err)
		}
		fmt.Fprint(inner, "GET / HTTP/1.1\r\nHost: upstream\r\nConnection: close\r\n\r\n")
		res, err := http.ReadResponse(bufio.NewReader(inner), &http.Request{Method: "GET"})
		if err != nil {
			inner.Close()
			t.Fatal(err)
		}
		body, err := io.ReadAll(res.Body)
		res.Body.Close()
		inner.Close()
		if err != nil || string(body) != "verified-upstream" {
			t.Fatal("upstream TLS stream", err, string(body))
		}
	}
}
