package control

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/XUJiahua/tunnel-mvp/internal/diagnostics"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/XUJiahua/tunnel-mvp/internal/agent"
	"github.com/XUJiahua/tunnel-mvp/internal/config"
	"github.com/gorilla/websocket"
)

const testPassword = "test-password-strong-123"

type fixture struct {
	s      *Server
	h      *httptest.Server
	cookie *http.Cookie
	dir    string
}

func setup(t *testing.T) *fixture {
	t.Helper()
	h := httptest.NewUnstartedServer(nil)
	dir := t.TempDir()
	s, err := New(config.Server{PublicURL: "http://" + h.Listener.Addr().String(), AccessURL: "http://apps.localhost:" + strconv.Itoa(h.Listener.Addr().(*net.TCPAddr).Port), DataDir: dir, Storage: os.Getenv("TUNNEL_TEST_STORAGE")}, testPassword)
	if err != nil {
		t.Fatal(err)
	}
	h.Config.Handler = s.Handler()
	h.Start()
	f := &fixture{s: s, h: h, dir: dir}
	t.Cleanup(func() { h.Close(); s.Close() })
	res, _ := f.request(t, "POST", "/api/login", map[string]string{"username": "admin", "password": testPassword}, nil, "")
	if res.StatusCode != 200 {
		t.Fatal("login failed")
	}
	f.cookie = res.Cookies()[0]
	return f
}
func (f *fixture) request(t *testing.T, method, path string, body any, cookie *http.Cookie, host string) (*http.Response, []byte) {
	t.Helper()
	var data io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		data = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, f.h.URL+path, data)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", f.h.URL)
	req.Header.Set("Content-Type", "application/json")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	if host != "" {
		req.Host = host
	}
	res, err := (&http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(res.Body)
	res.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	return res, b
}
func mustJSON[T any](t *testing.T, b []byte) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("invalid JSON %q: %v", b, err)
	}
	return out
}
func (f *fixture) site(t *testing.T, name string) (Site, string) {
	t.Helper()
	res, b := f.request(t, "POST", "/api/sites", map[string]string{"name": name}, f.cookie, "")
	if res.StatusCode != 201 {
		t.Fatalf("create site: %s", b)
	}
	v := mustJSON[struct {
		Site  Site   `json:"site"`
		Token string `json:"token"`
	}](t, b)
	return v.Site, v.Token
}
func (f *fixture) service(t *testing.T, site Site, name, target string) Service {
	t.Helper()
	res, b := f.request(t, "POST", "/api/services", map[string]string{"name": name, "siteID": site.ID, "url": target}, f.cookie, "")
	if res.StatusCode != 201 {
		t.Fatalf("create service: %s", b)
	}
	return mustJSON[Service](t, b)
}
func waitFor(t *testing.T, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for tunnel state")
}
func (f *fixture) agent(t *testing.T, site Site, token string, ports ...int) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- agent.Run(ctx, config.Agent{ServerURL: f.h.URL, SiteID: site.ID, Allow: []config.Rule{{CIDR: "127.0.0.0/8", Ports: ports}}}, token)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(3 * time.Second):
			t.Error("agent did not stop")
		}
	})
	waitFor(t, func() bool { return f.s.tunnel.HasSession(site.ID) })
	return cancel
}
func portOf(raw string) int { u, _ := url.Parse(raw); n, _ := strconv.Atoi(u.Port()); return n }
func (f *fixture) grant(t *testing.T, svc Service, login *http.Cookie) (*http.Cookie, string, string) {
	t.Helper()
	res, b := f.request(t, "POST", "/api/services/"+svc.ID+"/launch", nil, login, "")
	if res.StatusCode != 200 {
		t.Fatalf("launch: %d %s", res.StatusCode, b)
	}
	dest := mustJSON[map[string]string](t, b)["url"]
	u, _ := url.Parse(dest)
	res, b = f.request(t, "GET", u.RequestURI(), nil, nil, u.Host)
	if res.StatusCode != 303 {
		t.Fatalf("entry: %d %s", res.StatusCode, b)
	}
	if !res.Cookies()[0].HttpOnly || res.Cookies()[0].Domain != "" {
		t.Fatal("invalid gateway cookie")
	}
	return res.Cookies()[0], u.Host, u.RequestURI()
}
func TestRealTunnelProxyAndRevocation(t *testing.T) {
	f := setup(t)
	var origin string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/redirect":
			w.Header().Set("Location", origin+"/login?next=%2F")
			w.WriteHeader(302)
		case "/cookies":
			w.Header().Add("Set-Cookie", "appsession=value; Domain=internal.example; Path=/; HttpOnly")
			w.Header().Add("Set-Cookie", accessCookie+"=evil; Path=/")
			fmt.Fprint(w, "cookie")
		case "/ws":
			c, err := (&websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return r.Header.Get("Origin") == origin }}).Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer c.Close()
			for {
				kind, msg, err := c.ReadMessage()
				if err != nil {
					return
				}
				if err = c.WriteMessage(kind, msg); err != nil {
					return
				}
			}
		case "/events":
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: ready\n\n")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		default:
			jsonOut(w, 200, map[string]string{"path": r.URL.RequestURI(), "cookie": r.Header.Get("Cookie"), "host": r.Host, "forwardedHost": r.Header.Get("X-Forwarded-Host"), "origin": r.Header.Get("Origin")})
		}
	}))
	defer upstream.Close()
	origin = upstream.URL
	site, token := f.site(t, "Site A")
	cancelAgent := f.agent(t, site, token, portOf(origin))
	svc := f.service(t, site, "Platform", origin)
	access, host, ticketPath := f.grant(t, svc, f.cookie)
	res, b := f.request(t, "GET", ticketPath, nil, nil, host)
	if res.StatusCode != 401 {
		t.Fatalf("ticket replay accepted: %s", b)
	}
	res, _ = f.request(t, "GET", "/", nil, nil, host)
	if res.StatusCode != 401 {
		t.Fatal("unauthenticated proxy accepted")
	}
	req, _ := http.NewRequest("POST", f.h.URL+"/echo?q=hello", strings.NewReader("body"))
	req.Host = host
	req.Header.Set("Origin", f.s.serviceURL(svc.ID).String())
	req.AddCookie(access)
	req.AddCookie(f.cookie)
	req.AddCookie(&http.Cookie{Name: "app", Value: "ok"})
	req.Header.Set("X-Forwarded-Host", "forged")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ = io.ReadAll(res.Body)
	res.Body.Close()
	echo := mustJSON[map[string]string](t, b)
	if echo["path"] != "/echo?q=hello" || echo["cookie"] != "app=ok" || echo["forwardedHost"] != host || echo["origin"] != origin {
		t.Fatalf("incorrect forwarding: %+v", echo)
	}
	res, b = f.request(t, "GET", "/redirect", nil, access, host)
	if res.StatusCode != 302 || res.Header.Get("Location") != f.s.serviceURL(svc.ID).String()+"/login?next=%2F" {
		t.Fatalf("redirect not rewritten: %v %s", res.Header, b)
	}
	res, _ = f.request(t, "GET", "/cookies", nil, access, host)
	if len(res.Cookies()) != 1 || res.Cookies()[0].Domain != "" || res.Cookies()[0].Name != "appsession" {
		t.Fatalf("unsafe cookies: %v", res.Cookies())
	}
	// Real WebSocket upgrade and bidirectional frames over the reverse tunnel.
	headers := http.Header{"Host": []string{host}, "Cookie": []string{access.String()}, "Origin": []string{f.s.serviceURL(svc.ID).String()}}
	ws, _, err := (&websocket.Dialer{}).Dial("ws"+strings.TrimPrefix(f.h.URL, "http")+"/ws", headers)
	if err != nil {
		t.Fatal(err)
	}
	ws.SetReadDeadline(time.Now().Add(3 * time.Second))
	if err = ws.WriteMessage(websocket.TextMessage, []byte("hello tunnel")); err != nil {
		t.Fatal(err)
	}
	_, msg, err := ws.ReadMessage()
	ws.Close()
	if err != nil || string(msg) != "hello tunnel" {
		t.Fatalf("websocket: %q %v", msg, err)
	}
	// SSE must arrive before the upstream response ends.
	ctx, stop := context.WithTimeout(context.Background(), 3*time.Second)
	req, _ = http.NewRequestWithContext(ctx, "GET", f.h.URL+"/events", nil)
	req.Host = host
	req.AddCookie(access)
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		stop()
		t.Fatal(err)
	}
	line, err := bufio.NewReader(res.Body).ReadString('\n')
	res.Body.Close()
	stop()
	if err != nil || line != "data: ready\n" {
		t.Fatalf("SSE did not flush: %q %v", line, err)
	}
	res, b = f.request(t, "POST", "/api/services/"+svc.ID+"/probe", nil, f.cookie, "")
	if !mustJSON[ProbeResult](t, b).Reachable {
		t.Fatalf("probe failed: %s", b)
	}
	// Logout revokes already-issued application cookies for subsequent requests.
	f.request(t, "POST", "/api/logout", nil, f.cookie, "")
	res, _ = f.request(t, "GET", "/", nil, access, host)
	if res.StatusCode != 401 {
		t.Fatal("logout did not revoke gateway access")
	}
	cancelAgent()
	waitFor(t, func() bool { return !f.s.tunnel.HasSession(site.ID) })
	// Tokens and plaintext passwords must never reach persistent state or audit.
	stateName := "state.json"
	if f.s.cfg.Storage == "sqlite" {
		stateName = "state.db"
	}
	for _, name := range []string{stateName, "audit.jsonl"} {
		data, err := os.ReadFile(filepath.Join(f.dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(data, []byte(token)) || bytes.Contains(data, []byte(testPassword)) {
			t.Fatalf("plaintext secret stored in %s", name)
		}
	}
}

type ProbeResult struct {
	Reachable bool `json:"reachable"`
}

func TestPermissionsIsolationACLAndRotation(t *testing.T) {
	f := setup(t)
	a, ta := f.site(t, "A")
	b, tb := f.site(t, "B")
	ua := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "site A") }))
	defer ua.Close()
	ub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "site B") }))
	defer ub.Close()
	f.agent(t, a, ta, portOf(ua.URL))
	f.agent(t, b, tb, portOf(ub.URL))
	sa := f.service(t, a, "A", ua.URL)
	sb := f.service(t, b, "B", ub.URL)
	denied := f.service(t, a, "Forbidden port", ub.URL)
	ca, ha, _ := f.grant(t, sa, f.cookie)
	cb, hb, _ := f.grant(t, sb, f.cookie)
	cd, hd, _ := f.grant(t, denied, f.cookie)
	for _, v := range []struct {
		cookie     *http.Cookie
		host, body string
	}{{ca, ha, "site A"}, {cb, hb, "site B"}} {
		res, body := f.request(t, "GET", "/", nil, v.cookie, v.host)
		if res.StatusCode != 200 || string(body) != v.body {
			t.Fatalf("wrong site route: %d %s", res.StatusCode, body)
		}
	}
	res, _ := f.request(t, "GET", "/", nil, ca, hb)
	if res.StatusCode != 401 {
		t.Fatal("cross-application cookie accepted")
	}
	res, _ = f.request(t, "GET", "/", nil, cd, hd)
	if res.StatusCode != 502 {
		t.Fatal("agent port ACL bypassed")
	}
	res, body := f.request(t, "POST", "/api/users", map[string]any{"username": "viewer", "password": testPassword, "siteIDs": []string{a.ID}}, f.cookie, "")
	if res.StatusCode != 201 {
		t.Fatal(string(body))
	}
	viewer := mustJSON[User](t, body)
	res, _ = f.request(t, "POST", "/api/login", map[string]string{"username": "viewer", "password": testPassword}, nil, "")
	login := res.Cookies()[0]
	res, body = f.request(t, "GET", "/api/sites", nil, login, "")
	sites := mustJSON[[]Site](t, body)
	if len(sites) != 1 || sites[0].ID != a.ID {
		t.Fatalf("site grant leak: %s", body)
	}
	res, _ = f.request(t, "POST", "/api/sites", map[string]string{"name": "forbidden"}, login, "")
	if res.StatusCode != 403 {
		t.Fatal("viewer can create sites")
	}
	res, _ = f.request(t, "POST", "/api/services/"+sb.ID+"/launch", nil, login, "")
	if res.StatusCode != 404 {
		t.Fatal("viewer can access other site")
	}
	cv, hv, _ := f.grant(t, sa, login)
	f.request(t, "DELETE", "/api/users/"+viewer.ID, nil, f.cookie, "")
	res, _ = f.request(t, "GET", "/", nil, cv, hv)
	if res.StatusCode != 401 {
		t.Fatal("deleted user still authorized")
	}
	res, body = f.request(t, "POST", "/api/sites/"+a.ID+"/rotate", nil, f.cookie, "")
	if res.StatusCode != 200 {
		t.Fatal(string(body))
	}
	waitFor(t, func() bool { return !f.s.tunnel.HasSession(a.ID) })
	// Old token is rejected at the handshake, not just removed from the registry.
	ws, resp, err := (&websocket.Dialer{}).Dial("ws"+strings.TrimPrefix(f.h.URL, "http")+"/tunnel/connect", http.Header{"X-Tunnel-Site": []string{a.ID}, "Authorization": []string{"Bearer " + ta}})
	if ws != nil {
		ws.Close()
	}
	if err == nil || resp.StatusCode != 401 {
		t.Fatal("rotated token still accepted")
	}
	resp.Body.Close()
	next := mustJSON[map[string]string](t, body)["token"]
	f.agent(t, a, next, portOf(ua.URL))
	res, body = f.request(t, "POST", "/api/services/"+sa.ID+"/probe", nil, f.cookie, "")
	if !mustJSON[ProbeResult](t, body).Reachable {
		t.Fatalf("new token cannot reconnect: %s", body)
	}
	f.request(t, "DELETE", "/api/sites/"+a.ID, nil, f.cookie, "")
	waitFor(t, func() bool { return !f.s.tunnel.HasSession(a.ID) })
	res, _ = f.request(t, "GET", "/", nil, ca, ha)
	if res.StatusCode != 404 {
		t.Fatal("deleted site service survived")
	}
}
func TestCSRFTrustAndPersistence(t *testing.T) {
	f := setup(t)
	req, _ := http.NewRequest("POST", f.h.URL+"/api/sites", strings.NewReader(`{"name":"evil"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://evil.test")
	req.AddCookie(f.cookie)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatal("cross-origin write accepted")
	}
	persistent, persistentToken := f.site(t, "persistent")
	// TLS remains verified even though the connection is opened through an Agent.
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "secret") }))
	defer upstream.Close()
	site, token := f.site(t, "TLS")
	f.agent(t, site, token, portOf(upstream.URL))
	svc := f.service(t, site, "TLS", upstream.URL)
	access, host, _ := f.grant(t, svc, f.cookie)
	resp, _ = f.request(t, "GET", "/", nil, access, host)
	if resp.StatusCode != 502 {
		t.Fatal("untrusted origin certificate accepted")
	}
	f.h.Close()
	f.s.Close()
	reopened, err := openStore(f.dir, f.s.cfg.Storage, "ignored", "")
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	saved, ok := reopened.site(persistent.ID)
	if !ok || saved.TokenHash != digest(persistentToken) {
		t.Fatal("state not restored")
	}

}

func TestLocalProbeDiagnosticsSurviveTunnel(t *testing.T) {
	var logs lockedBuffer
	original := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(original) })
	f := setup(t)
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	defer app.Close()
	tlsApp := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	defer tlsApp.Close()
	deniedApp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	defer deniedApp.Close()
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedURL := "http://" + l.Addr().String()
	_ = l.Close()
	site, token := f.site(t, "Local diagnostic site")
	f.agent(t, site, token, portOf(app.URL), portOf(tlsApp.URL), portOf(closedURL))
	// A live server, Agent and application on one machine still need the correct ACL.
	for _, tc := range []struct{ name, target, code, stage string }{
		{"healthy", app.URL, "", ""},
		{"ACL", deniedApp.URL, "acl_denied", "acl"},
		{"refused", closedURL, "connection_refused", "tcp"},
		{"TLS", tlsApp.URL, "tls_failed", "tls"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := f.service(t, site, tc.name, tc.target)
			res, body := f.request(t, "POST", "/api/services/"+svc.ID+"/probe", nil, f.cookie, "")
			report := mustJSON[probeReport](t, body)
			if report.RequestID == "" || report.RequestID != res.Header.Get("X-Tunnel-Request-ID") || report.Target != tc.target || report.SiteID != site.ID {
				t.Fatalf("missing correlation: %s", body)
			}
			if tc.code == "" {
				if !report.Reachable || report.Status != 204 {
					t.Fatalf("local app unreachable: %s", body)
				}
				return
			}
			if report.Reachable || report.Failure == nil || report.Code != tc.code || report.Stage != tc.stage || report.Message == "" || report.Hint == "" {
				t.Fatalf("lost network diagnosis: %s", body)
			}
			if tc.stage == "acl" || tc.stage == "tcp" {
				if report.AgentRequestID == "" || len(report.ResolvedIPs) == 0 || !strings.Contains(logs.String(), report.AgentRequestID) {
					t.Fatalf("Agent failure not correlated: %s", body)
				}
			}
			if !strings.Contains(logs.String(), report.RequestID) {
				t.Fatal("Server request ID absent from logs")
			}
			data, _ := os.ReadFile(filepath.Join(f.dir, "audit.jsonl"))
			if !bytes.Contains(data, []byte(report.RequestID)) || !bytes.Contains(data, []byte(tc.code)) {
				t.Fatal("audit missing diagnosis")
			}
		})
	}
	offline, _ := f.site(t, "Offline")
	svc := f.service(t, offline, "offline", app.URL)
	_, body := f.request(t, "POST", "/api/services/"+svc.ID+"/probe", nil, f.cookie, "")
	report := mustJSON[probeReport](t, body)
	if report.Failure == nil || report.Code != "site_offline" {
		t.Fatalf("offline: %s", body)
	}
}

type lockedBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(p)
}
func (b *lockedBuffer) String() string { b.mu.Lock(); defer b.mu.Unlock(); return b.buffer.String() }

func TestDiagnosticReportAuthenticationAndSiteIsolation(t *testing.T) {
	f := setup(t)
	site, token := f.site(t, "A")
	other, _ := f.site(t, "B")
	failure := diagnostics.New("acl_denied", "acl", "reported failure")
	failure.AgentRequestID = diagnostics.ID()
	res, _ := f.request(t, "POST", "/tunnel/diagnostics", failure, f.cookie, "")
	if res.StatusCode != 401 {
		t.Fatal("console cookie accepted as Agent identity")
	}
	body, _ := json.Marshal(failure)
	req, _ := http.NewRequest("POST", f.h.URL+"/tunnel/diagnostics", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Tunnel-Site", site.ID)
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 204 {
		t.Fatalf("authenticated Agent report rejected: %d", res.StatusCode)
	}
	wire := errors.New(failure.Error())
	if f.s.describeFailure(site.ID, wire).Message != "reported failure" {
		t.Fatal("full report not recovered")
	}
	// A different site cannot resolve the same ID to this site's private report.
	f.s.diagnosticMu.Lock()
	record := f.s.diagnosticCache[site.ID+":"+failure.AgentRequestID]
	record.failure.Message = "private diagnostic"
	f.s.diagnosticCache[site.ID+":"+failure.AgentRequestID] = record
	f.s.diagnosticMu.Unlock()
	if f.s.describeFailure(other.ID, wire).Message == "private diagnostic" {
		t.Fatal("cross-site diagnostic leak")
	}
}
