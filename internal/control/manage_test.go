package control

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func (f *fixture) member(t *testing.T, sites, services []string) (User, *http.Cookie) {
	t.Helper()
	res, b := f.request(t, "POST", "/api/users", map[string]any{"username": "member", "password": testPassword, "siteIDs": sites, "serviceIDs": services}, f.cookie, "")
	if res.StatusCode != 201 {
		t.Fatalf("member: %s", b)
	}
	u := mustJSON[User](t, b)
	res, b = f.request(t, "POST", "/api/login", map[string]string{"username": "member", "password": testPassword}, nil, "")
	if res.StatusCode != 200 {
		t.Fatalf("member login: %s", b)
	}
	return u, res.Cookies()[0]
}
func TestEditResourcesAndApplicationGrants(t *testing.T) {
	f := setup(t)
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "first") }))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "second") }))
	defer second.Close()
	site, token := f.site(t, "old")
	f.agent(t, site, token, portOf(first.URL), portOf(second.URL))
	svc := f.service(t, site, "platform", first.URL)
	sibling := f.service(t, site, "private", first.URL)
	member, login := f.member(t, nil, []string{svc.ID})
	res, b := f.request(t, "GET", "/api/services", nil, login, "")
	got := mustJSON[[]Service](t, b)
	if res.StatusCode != 200 || len(got) != 1 || got[0].ID != svc.ID {
		t.Fatalf("application isolation: %s", b)
	}
	_, b = f.request(t, "GET", "/api/sites", nil, login, "")
	if len(mustJSON[[]Site](t, b)) != 1 {
		t.Fatal("application-only grant hides parent site")
	}
	for _, action := range []string{"launch", "probe"} {
		res, _ = f.request(t, "POST", "/api/services/"+sibling.ID+"/"+action, nil, login, "")
		if res.StatusCode != 404 {
			t.Fatalf("sibling %s allowed", action)
		}
	}
	access, host, _ := f.grant(t, svc, login)
	res, b = f.request(t, "GET", "/", nil, access, host)
	if string(b) != "first" {
		t.Fatalf("first: %s", b)
	}
	res, _ = f.request(t, "PATCH", "/api/sites/"+site.ID, map[string]string{"name": "new"}, login, "")
	if res.StatusCode != 403 {
		t.Fatal("member can edit site")
	}
	res, b = f.request(t, "PATCH", "/api/sites/"+site.ID, map[string]string{"name": "new"}, f.cookie, "")
	if res.StatusCode != 200 || mustJSON[Site](t, b).ID != site.ID {
		t.Fatal("rename changed identity")
	}
	res, _ = f.request(t, "PATCH", "/api/services/"+svc.ID, map[string]string{"name": "new app", "url": second.URL + "/forbidden"}, f.cookie, "")
	if res.StatusCode != 400 {
		t.Fatal("accepted path target")
	}
	res, b = f.request(t, "PATCH", "/api/services/"+svc.ID, map[string]string{"name": "new app", "url": second.URL}, f.cookie, "")
	if res.StatusCode != 200 || mustJSON[Service](t, b).ID != svc.ID {
		t.Fatalf("update: %s", b)
	}
	res, _ = f.request(t, "GET", "/", nil, access, host)
	if res.StatusCode != 401 {
		t.Fatal("target edit retained old grant")
	}
	access, host, _ = f.grant(t, svc, login)
	_, b = f.request(t, "GET", "/", nil, access, host)
	if string(b) != "second" {
		t.Fatalf("stale upstream pool: %s", b)
	}
	res, _ = f.request(t, "PATCH", "/api/users/"+member.ID, map[string]any{"siteIDs": []string{site.ID}, "serviceIDs": []string{}}, f.cookie, "")
	if res.StatusCode != 200 {
		t.Fatal("grant update failed")
	}
	future := f.service(t, site, "future", first.URL)
	res, _ = f.request(t, "POST", "/api/services/"+future.ID+"/probe", nil, login, "")
	if res.StatusCode != 200 {
		t.Fatal("site grant did not cover new service")
	}
	res, _ = f.request(t, "PATCH", "/api/users/"+member.ID, map[string]any{"serviceIDs": []string{"unknown"}}, f.cookie, "")
	if res.StatusCode != 400 {
		t.Fatal("unknown grant accepted")
	}
	f.h.Close()
	f.s.Close()
	reopened, err := openStore(f.dir, f.s.cfg.Storage, "admin", "")
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	persisted, _ := reopened.service(svc.ID)
	if persisted.URL != second.URL {
		t.Fatal("resource edit not persisted")
	}
}

func TestPasswordChangeAndReset(t *testing.T) {
	f := setup(t)
	u, login := f.member(t, nil, nil)
	fresh := "changed-member-password-123"
	res, _ := f.request(t, "POST", "/api/me/password", map[string]string{"currentPassword": "wrong", "password": fresh}, login, "")
	if res.StatusCode != 403 {
		t.Fatal("wrong current password accepted")
	}
	res, _ = f.request(t, "POST", "/api/me/password", map[string]string{"currentPassword": testPassword, "password": "short"}, login, "")
	if res.StatusCode != 400 {
		t.Fatal("weak password accepted")
	}
	res, b := f.request(t, "POST", "/api/me/password", map[string]string{"currentPassword": testPassword, "password": fresh}, login, "")
	if res.StatusCode != 200 {
		t.Fatalf("change: %s", b)
	}
	res, _ = f.request(t, "GET", "/api/me", nil, login, "")
	if res.StatusCode != 401 {
		t.Fatal("password change retained session")
	}
	res, _ = f.request(t, "POST", "/api/login", map[string]string{"username": "member", "password": testPassword}, nil, "")
	if res.StatusCode != 401 {
		t.Fatal("old password accepted")
	}
	res, _ = f.request(t, "POST", "/api/login", map[string]string{"username": "member", "password": fresh}, nil, "")
	if res.StatusCode != 200 {
		t.Fatal("new password rejected")
	}
	newLogin := res.Cookies()[0]
	res, _ = f.request(t, "POST", "/api/users/"+u.ID+"/password", map[string]string{"password": testPassword}, newLogin, "")
	if res.StatusCode != 403 {
		t.Fatal("member may reset password")
	}
	res, _ = f.request(t, "POST", "/api/users/"+u.ID+"/password", map[string]string{"password": testPassword}, f.cookie, "")
	if res.StatusCode != 200 {
		t.Fatal("admin reset failed")
	}
	res, _ = f.request(t, "GET", "/api/me", nil, newLogin, "")
	if res.StatusCode != 401 {
		t.Fatal("reset retained session")
	}
	admin := f.s.store.snapshot().Users[0]
	res, _ = f.request(t, "POST", "/api/users/"+admin.ID+"/password", map[string]string{"password": testPassword}, f.cookie, "")
	if res.StatusCode != 404 {
		t.Fatal("admin reset bypasses current password")
	}
}

func TestLiveWebSocketAndSSERevocation(t *testing.T) {
	for _, action := range []string{"logout", "password", "grants", "delete", "target", "expiry", "disable", "identity", "session", "group"} {
		t.Run(action, func(t *testing.T) {
			f := setup(t)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/ws" {
					c, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
					if err != nil {
						return
					}
					defer c.Close()
					for {
						kind, msg, err := c.ReadMessage()
						if err != nil {
							return
						}
						if c.WriteMessage(kind, msg) != nil {
							return
						}
					}
				}
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "data: ready\n\n")
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			}))
			defer upstream.Close()
			site, token := f.site(t, "site")
			f.agent(t, site, token, portOf(upstream.URL))
			svc := f.service(t, site, "app", upstream.URL)
			u, login := f.member(t, nil, []string{svc.ID})
			var group Group
			if action == "group" {
				res, data := f.request(t, "POST", "/api/groups", map[string]any{"name": "live group", "memberIDs": []string{u.ID}, "serviceIDs": []string{svc.ID}}, f.cookie, "")
				if res.StatusCode != 201 {
					t.Fatal(string(data))
				}
				group = mustJSON[Group](t, data)
				res, _ = f.request(t, "PATCH", "/api/users/"+u.ID, map[string]any{"siteIDs": []string{}, "serviceIDs": []string{}}, f.cookie, "")
				if res.StatusCode != 200 {
					t.Fatal("remove direct grant")
				}
			}
			access, host, _ := f.grant(t, svc, login)
			if action == "expiry" {
				f.s.auth.mu.Lock()
				g := f.s.auth.access[digest(access.Value)]
				g.Expires = time.Now().Add(500 * time.Millisecond)
				f.s.auth.access[digest(access.Value)] = g
				f.s.auth.mu.Unlock()
			}
			ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(f.h.URL, "http")+"/ws", http.Header{"Host": []string{host}, "Cookie": []string{access.String()}})
			if err != nil {
				t.Fatal(err)
			}
			defer ws.Close()
			if err = ws.WriteMessage(websocket.TextMessage, []byte("hello")); err != nil {
				t.Fatal(err)
			}
			if _, _, err = ws.ReadMessage(); err != nil {
				t.Fatal(err)
			}
			req, _ := http.NewRequest("GET", f.h.URL+"/events", nil)
			req.Host = host
			req.AddCookie(access)
			stream, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer stream.Body.Close()
			reader := bufio.NewReader(stream.Body)
			if _, err = reader.ReadString('\n'); err != nil {
				t.Fatal(err)
			}
			reader.ReadString('\n')
			var res *http.Response
			switch action {
			case "group":
				res, _ = f.request(t, "PATCH", "/api/groups/"+group.ID, map[string]any{"name": "live group", "memberIDs": []string{u.ID}, "serviceIDs": []string{}}, f.cookie, "")
			case "session":
				_, data := f.request(t, "GET", "/api/me/sessions", nil, login, "")
				sessions := mustJSON[[]sessionInfo](t, data)
				res, _ = f.request(t, "DELETE", "/api/me/sessions/"+sessions[0].ID, nil, login, "")
			case "disable":
				res, _ = f.request(t, "PATCH", "/api/users/"+u.ID+"/disabled", map[string]bool{"disabled": true}, f.cookie, "")
			case "identity":
				f.s.cfg.OIDC.Issuer = "https://idp.example"
				f.s.oidc = &oidcLogin{}
				res, _ = f.request(t, "PATCH", "/api/users/"+u.ID+"/identity", map[string]string{"subject": "new-binding"}, f.cookie, "")
			case "logout":
				res, _ = f.request(t, "POST", "/api/logout", nil, login, "")
			case "password":
				res, _ = f.request(t, "POST", "/api/users/"+u.ID+"/password", map[string]string{"password": "another-strong-password-123"}, f.cookie, "")
			case "grants":
				res, _ = f.request(t, "PATCH", "/api/users/"+u.ID, map[string]any{"siteIDs": []string{}, "serviceIDs": []string{}}, f.cookie, "")
			case "delete":
				res, _ = f.request(t, "DELETE", "/api/users/"+u.ID, nil, f.cookie, "")
			case "target":
				res, _ = f.request(t, "PATCH", "/api/services/"+svc.ID, map[string]string{"name": "app", "url": "http://127.0.0.1:1"}, f.cookie, "")
			}
			if res != nil && res.StatusCode != 200 {
				t.Fatalf("revoke: %d", res.StatusCode)
			}
			ws.SetReadDeadline(time.Now().Add(3 * time.Second))
			if _, _, err = ws.ReadMessage(); err == nil {
				t.Fatal("revoked WebSocket stayed open")
			} else if n, ok := err.(net.Error); ok && n.Timeout() {
				t.Fatal("WebSocket timed out instead of closing")
			}
			closed := make(chan error, 1)
			go func() { _, err := io.ReadAll(reader); closed <- err }()
			select {
			case <-closed:
			case <-time.After(3 * time.Second):
				t.Fatal("revoked SSE stayed open")
			}
		})
	}
}

func TestPoolReuseMonitoringAndAudit(t *testing.T) {
	f := setup(t)
	var connections atomic.Int64
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "pooled") }))
	upstream.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	upstream.Start()
	defer upstream.Close()
	site, token := f.site(t, "site")
	f.agent(t, site, token, portOf(upstream.URL))
	svc := f.service(t, site, "app", upstream.URL)
	access, host, _ := f.grant(t, svc, f.cookie)
	for range 5 {
		res, b := f.request(t, "GET", "/", nil, access, host)
		if res.StatusCode != 200 || string(b) != "pooled" {
			t.Fatal("proxy failed")
		}
	}
	if connections.Load() != 1 {
		t.Fatalf("did not reuse connection: %d", connections.Load())
	}
	f.s.probeAll(context.Background())
	_, b := f.request(t, "GET", "/api/status", nil, f.cookie, "")
	status := mustJSON[struct {
		Reports []probeReport `json:"reports"`
	}](t, b)
	if len(status.Reports) != 1 || !status.Reports[0].Reachable || !status.Reports[0].Healthy {
		t.Fatalf("status: %s", b)
	}
	res, _ := f.request(t, "GET", "/metrics", nil, nil, "")
	if res.StatusCode != 404 {
		t.Fatal("metrics exposed by default")
	}
	f.s.metricsToken = strings.Repeat("m", 32)
	res, _ = f.request(t, "GET", "/metrics", nil, nil, "")
	if res.StatusCode != 401 {
		t.Fatal("metrics accepted unauthenticated scrape")
	}
	req, _ := http.NewRequest("GET", f.h.URL+"/metrics", nil)
	req.Header.Set("Authorization", "Bearer "+f.s.metricsToken)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ = io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || !strings.Contains(string(b), "tunnel_proxy_requests_total 5") || !strings.Contains(string(b), "tunnel_service_healthy{service_id=\""+svc.ID+"\"} 1") {
		t.Fatalf("metrics: %s", b)
	}
	_, member := f.member(t, nil, nil)
	res, _ = f.request(t, "GET", "/api/audit", nil, member, "")
	if res.StatusCode != 403 {
		t.Fatal("member read audit")
	}
	_, b = f.request(t, "GET", "/api/status", nil, member, "")
	if strings.Contains(string(b), svc.ID) {
		t.Fatal("status leaked another member's resource")
	}
	_, b = f.request(t, "GET", "/api/audit?event=site_created&limit=1", nil, f.cookie, "")
	if !strings.Contains(string(b), site.ID) || strings.Contains(string(b), token) {
		t.Fatalf("audit: %s", b)
	}
	f.s.cfg.AuditMaxMB = 1
	f.s.auditMu.Lock()
	for range 20000 {
		fmt.Fprintln(f.s.auditFile, `{"event":"padding","value":"audit rotation integration test"}`)
	}
	f.s.auditMu.Unlock()
	f.s.audit("rotation_check", "admin", "", "")
	if _, err = os.Stat(f.s.auditFile.Name() + ".1"); err != nil {
		t.Fatal("audit did not rotate", err)
	}
	_, b = f.request(t, "GET", "/api/audit?event=rotation_check", nil, f.cookie, "")
	if !strings.Contains(string(b), "rotation_check") {
		t.Fatal("new audit file unavailable")
	}
}

func TestHealthStatusDistinguishesHTTPReachability(t *testing.T) {
	f := setup(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer upstream.Close()
	site, token := f.site(t, "site")
	f.agent(t, site, token, portOf(upstream.URL))
	svc := f.service(t, site, "app", upstream.URL)
	_, b := f.request(t, "POST", "/api/services/"+svc.ID+"/probe", nil, f.cookie, "")
	p := mustJSON[probeReport](t, b)
	if !p.Reachable || p.Healthy || p.Status != 503 {
		t.Fatalf("HTTP 503 treated as healthy: %s", b)
	}
}

func TestParallelProxyRemainsIsolatedAcrossTargetEdit(t *testing.T) {
	f := setup(t)
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "old") }))
	defer old.Close()
	next := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "new") }))
	defer next.Close()
	site, token := f.site(t, "site")
	f.agent(t, site, token, portOf(old.URL), portOf(next.URL))
	svc := f.service(t, site, "app", old.URL)
	access, host, _ := f.grant(t, svc, f.cookie)
	done := make(chan struct{}, 24)
	for range 24 {
		go func() {
			defer func() { done <- struct{}{} }()
			for range 5 {
				req, err := http.NewRequest("GET", f.h.URL+"/", nil)
				if err != nil {
					t.Error(err)
					return
				}
				req.Host = host
				req.AddCookie(access)
				res, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Error(err)
					return
				}
				b, err := io.ReadAll(res.Body)
				res.Body.Close()
				// In-flight old responses may finish before revocation; rejected requests
				// can be 401/502. An old grant must never obtain the new application's body.
				if err == nil && res.StatusCode == 200 && string(b) != "old" {
					t.Errorf("old grant reached new target: %s", b)
				}
			}
		}()
	}
	res, b := f.request(t, "PATCH", "/api/services/"+svc.ID, map[string]string{"name": "app", "url": next.URL}, f.cookie, "")
	if res.StatusCode != 200 {
		t.Fatalf("target edit: %s", b)
	}
	for range 24 {
		<-done
	}
	newAccess, host, _ := f.grant(t, svc, f.cookie)
	res, b = f.request(t, "GET", "/", nil, newAccess, host)
	if res.StatusCode != 200 || string(b) != "new" {
		t.Fatalf("fresh grant did not reach new target: %d %s", res.StatusCode, b)
	}
}
