package control

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
	s, err := New(config.Server{PublicURL: "http://" + h.Listener.Addr().String(), AccessURL: "http://apps.localhost:" + strconv.Itoa(h.Listener.Addr().(*net.TCPAddr).Port), DataDir: dir}, testPassword)
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
	for _, name := range []string{"state.json", "audit.jsonl"} {
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
	_, token := f.site(t, "persistent")
	reopened, err := OpenStore(f.dir, "ignored", "")
	if err != nil {
		t.Fatal(err)
	}
	st := reopened.snapshot()
	if len(st.Sites) != 1 || st.Sites[0].TokenHash != digest(token) {
		t.Fatal("state not restored")
	}
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
}
