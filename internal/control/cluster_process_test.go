package control

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/XUJiahua/tunnel-mvp/internal/agent"
	"github.com/XUJiahua/tunnel-mvp/internal/config"
	"github.com/XUJiahua/tunnel-mvp/internal/state"
	"github.com/XUJiahua/tunnel-mvp/internal/testpostgres"
)

// Run the production constructor/listeners in an isolated OS process so killing
// the owner cannot execute Server.Close, defer-release a claim or expire a lease.
func TestClusterProcessHelper(t *testing.T) {
	raw := os.Getenv("TUNNEL_CLUSTER_PROCESS_CONFIG")
	if raw == "" {
		return
	}
	var cfg config.Server
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatal(err)
	}
	s, err := New(cfg, testPassword)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(cfg.DataDir, "test-ready"), []byte("http://"+listener.Addr().String()), 0600); err != nil {
		t.Fatal(err)
	}
	if err = http.Serve(listener, s.Handler()); err != nil {
		t.Fatal(err)
	}
}
func clusterProcess(t *testing.T, cfg config.Server) (*exec.Cmd, *httputil.ReverseProxy) {
	t.Helper()
	raw, _ := json.Marshal(cfg)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(t.TempDir(), "node.log")
	log, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(executable, "-test.run=^TestClusterProcessHelper$", "-test.timeout=90s")
	cmd.Env = append(os.Environ(), "TUNNEL_CLUSTER_PROCESS_CONFIG="+string(raw))
	cmd.Stdout = log
	cmd.Stderr = log
	if err = cmd.Start(); err != nil {
		log.Close()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("cluster child did not exit")
		}
		log.Close()
	})
	ready := filepath.Join(cfg.DataDir, "test-ready")
	var address string
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if b, e := os.ReadFile(ready); e == nil {
			address = string(b)
			break
		}
		select {
		case err := <-done:
			done <- err
			b, _ := os.ReadFile(logPath)
			t.Fatalf("cluster child startup: %v\n%s", err, b)
		default:
		}
		time.Sleep(20 * time.Millisecond)
	}
	if address == "" {
		b, _ := os.ReadFile(logPath)
		t.Fatalf("cluster child startup timeout\n%s", b)
	}
	endpoint, err := url.Parse(address)
	if err != nil {
		t.Fatal(err)
	}
	return cmd, httputil.NewSingleHostReverseProxy(endpoint)
}
func TestPostgresClusterProcessCrashTakeover(t *testing.T) {
	dsn := testpostgres.Database(t)
	t.Setenv("TUNNEL_POSTGRES_DSN", dsn)
	ca := newClusterTestCA(t)
	lb := httptest.NewUnstartedServer(nil)
	var selected, agentNode atomic.Int32
	agentNode.Store(1)
	handlers := make([]*httputil.ReverseProxy, 2)
	processes := make([]*exec.Cmd, 2)
	for i, id := range []string{"process-a", "process-b"} {
		cfg := config.Server{PublicURL: "http://" + lb.Listener.Addr().String(), AccessURL: "http://apps.localhost:" + strconv.Itoa(lb.Listener.Addr().(*net.TCPAddr).Port), DataDir: t.TempDir(), Storage: state.Postgres, Cluster: ca.node(t, id)}
		processes[i], handlers[i] = clusterProcess(t, cfg)
	}
	lb.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := selected.Load()
		if strings.HasPrefix(r.URL.Path, "/tunnel") {
			n = agentNode.Load()
		}
		handlers[n].ServeHTTP(w, r)
	})
	lb.Start()
	t.Cleanup(lb.Close)
	f := &fixture{h: lb}
	res, body := f.request(t, "POST", "/api/login", map[string]string{"username": "admin", "password": testPassword}, nil, "")
	if res.StatusCode != 200 {
		t.Fatalf("login %d %s", res.StatusCode, body)
	}
	f.cookie = res.Cookies()[0]
	var posts atomic.Int32
	up := clusterUpstream(t, &posts)
	site, token := f.site(t, "process crash")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- agent.Run(ctx, config.Agent{ServerURL: lb.URL, SiteID: site.ID, Allow: []config.Rule{{CIDR: "127.0.0.0/8", Ports: []int{portOf(up.URL)}}}}, token)
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
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	owner := func() (string, uint64, bool) {
		var id string
		var generation uint64
		var ready bool
		err := db.QueryRow(`SELECT coalesce(owner_id,''),generation,ready FROM tunnel_state.cluster_agents WHERE agent_key=$1`, site.ID).Scan(&id, &generation, &ready)
		return id, generation, err == nil && ready
	}
	waitFor(t, func() bool { id, _, ready := owner(); return id == "process-b" && ready })
	svc := f.service(t, site, "process app", up.URL)
	cookie, host, _ := f.grant(t, svc, f.cookie)
	res, body = f.request(t, "GET", "/", nil, cookie, host)
	if res.StatusCode != 200 || string(body) != "cluster-ok" {
		t.Fatalf("cross-process proxy %d %s", res.StatusCode, body)
	}
	req, _ := http.NewRequest("POST", lb.URL+"/commit", strings.NewReader("one execution"))
	req.Host = host
	req.AddCookie(cookie)
	pending, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer pending.Body.Close()
	if pending.StatusCode != 200 {
		t.Fatal("upstream POST not entered", pending.StatusCode)
	}
	_, generation, _ := owner()
	agentNode.Store(0)
	if err = processes[1].Process.Kill(); err != nil {
		t.Fatal(err)
	}
	readDone := make(chan struct{})
	go func() { _, _ = io.ReadAll(pending.Body); close(readDone) }()
	select {
	case <-readDone:
	case <-time.After(5 * time.Second):
		t.Fatal("owner crash retained old stream")
	}
	deadline := time.Now().Add(30 * time.Second)
	taken := false
	for time.Now().Before(deadline) {
		id, next, ready := owner()
		if id == "process-a" && next > generation && ready {
			taken = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !taken {
		t.Fatal("Agent did not reconnect to surviving process with newer generation")
	}
	res, body = f.request(t, "GET", "/", nil, cookie, host)
	if res.StatusCode != 200 || string(body) != "cluster-ok" {
		t.Fatalf("post-crash traffic %d %s", res.StatusCode, body)
	}
	if posts.Load() != 1 {
		t.Fatal("accepted POST replayed", posts.Load())
	}
	fmt.Println("cluster owner process killed; surviving node acquired newer generation; POST executed once")
}
