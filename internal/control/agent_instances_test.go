package control

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/XUJiahua/tunnel-mvp/internal/agent"
	"github.com/XUJiahua/tunnel-mvp/internal/config"
	"github.com/gorilla/websocket"
)

func registerAgent(t *testing.T, f *fixture, site Site, name string) (agentView, string) {
	t.Helper()
	res, b := f.request(t, "POST", "/api/sites/"+site.ID+"/agents", map[string]string{"name": name}, f.cookie, "")
	if res.StatusCode != 201 {
		t.Fatalf("register: %s", b)
	}
	out := mustJSON[struct {
		Agent agentView `json:"agent"`
		Token string    `json:"token"`
	}](t, b)
	return out.Agent, out.Token
}
func namedAgent(t *testing.T, f *fixture, site Site, id, token string, port int) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- agent.Run(ctx, config.Agent{ServerURL: f.h.URL, SiteID: site.ID, InstanceID: id, RuntimeVersion: "instance-test", Allow: []config.Rule{{CIDR: "127.0.0.0/8", Ports: []int{port}}}}, token)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(4 * time.Second):
			t.Error("Agent failed to stop")
		}
	})
	waitFor(t, func() bool { return f.s.tunnel.HasSession(agentKey(site.ID, id)) })
	return cancel
}
func TestAgentInstancesRoutingAndRevocation(t *testing.T) {
	f := setup(t)
	site, legacy := f.site(t, "redundant")
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "application") }))
	defer up.Close()
	svc := f.service(t, site, "app", up.URL)
	a, ta := registerAgent(t, f, site, "A")
	b, tb := registerAgent(t, f, site, "B")
	namedAgent(t, f, site, a.ID, ta, portOf(up.URL))
	namedAgent(t, f, site, b.ID, tb, portOf(up.URL))
	for i := 0; i < 4; i++ {
		c, err := f.s.dialSite(context.Background(), site.ID, svc.ID, "tcp", strings.TrimPrefix(up.URL, "http://"))
		if err != nil {
			t.Fatal(err)
		}
		c.Close()
	}
	f.s.tunnelMu.Lock()
	ra, rb := f.s.connections[agentKey(site.ID, a.ID)], f.s.connections[agentKey(site.ID, b.ID)]
	f.s.tunnelMu.Unlock()
	if ra == nil || rb == nil {
		t.Fatal("missing simultaneous connections")
	}
	res, data := f.request(t, "GET", "/api/agents", nil, f.cookie, "")
	if res.StatusCode != 200 || strings.Contains(string(data), digest(ta)) || !strings.Contains(string(data), "instance-test") {
		t.Fatal("inventory or secret exposure")
	}
	// Duplicate instance, credential/site binding and legacy disable are independent.
	for _, tc := range []struct {
		id, token, site string
		status          int
	}{{a.ID, ta, site.ID, 409}, {a.ID, tb, site.ID, 401}, {"missing", ta, site.ID, 401}} {
		h := http.Header{"X-Tunnel-Site": []string{tc.site}, "X-Tunnel-Instance": []string{tc.id}, "Authorization": []string{"Bearer " + tc.token}}
		c, r, e := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(f.h.URL, "http")+"/tunnel/connect", h)
		if c != nil {
			c.Close()
		}
		if e == nil || r == nil || r.StatusCode != tc.status {
			t.Fatalf("handshake expected %d: %v", tc.status, e)
		}
		r.Body.Close()
	}
	res, _ = f.request(t, "PATCH", "/api/sites/"+site.ID, map[string]bool{"legacyDisabled": true}, f.cookie, "")
	if res.StatusCode != 200 || !f.s.siteOnline(site.ID) {
		t.Fatal("legacy disable stopped named Agents")
	}
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Tunnel-Site", site.ID)
	req.Header.Set("Authorization", "Bearer "+legacy)
	if _, ok := f.s.authenticatedAgent(req); ok {
		t.Fatal("legacy credential remained valid")
	}
	cookie, host, _ := f.grant(t, svc, f.cookie)
	held, err := f.s.dialSite(context.Background(), site.ID, svc.ID, "tcp", strings.TrimPrefix(up.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	owner := held.(*agentChannel).agent.instanceID
	other := a.ID
	if owner == a.ID {
		other = b.ID
	}
	res, _ = f.request(t, "PATCH", "/api/sites/"+site.ID+"/agents/"+owner, map[string]bool{"disabled": true}, f.cookie, "")
	if res.StatusCode != 200 {
		t.Fatal("disable failed")
	}
	held.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err = held.Read(make([]byte, 1)); err == nil {
		t.Fatal("revoked channel remained open")
	}
	held.Close()
	waitFor(t, func() bool { return len(f.s.connectedAgents(site.ID)) == 1 })
	if f.s.connectedAgents(site.ID)[0].instanceID != other {
		t.Fatal("wrong instance stopped")
	}
	res, data = f.request(t, "GET", "/", nil, cookie, host)
	if res.StatusCode != 200 || string(data) != "application" {
		t.Fatalf("failover: %d %s", res.StatusCode, data)
	}
	res, _ = f.request(t, "POST", "/api/sites/"+site.ID+"/rotate", nil, f.cookie, "")
	if res.StatusCode != 200 {
		t.Fatal("site rotate failed")
	}
	waitFor(t, func() bool { return !f.s.siteOnline(site.ID) })
	res, _ = f.request(t, "PATCH", "/api/sites/"+site.ID+"/agents/"+other, map[string]bool{"disabled": false}, f.cookie, "")
	if res.StatusCode == 200 {
		t.Fatal("re-enabled globally invalidated credentials")
	}
	res, data = f.request(t, "POST", "/api/sites/"+site.ID+"/agents/"+other+"/rotate", nil, f.cookie, "")
	if res.StatusCode != 200 {
		t.Fatalf("rotate %s", data)
	}
	r := mustJSON[struct {
		Agent agentView `json:"agent"`
		Token string    `json:"token"`
	}](t, data)
	if r.Agent.RequiresRotation || r.Agent.Disabled {
		t.Fatal("rotation did not clear blocked state")
	}
	namedAgent(t, f, site, other, r.Token, portOf(up.URL))
	res, data = f.request(t, "GET", "/", nil, cookie, host)
	if res.StatusCode != 200 {
		t.Fatalf("reconnect: %s", data)
	}
}

func TestAgentLossDoesNotReplaySubmittedPost(t *testing.T) {
	f := setup(t)
	site, _ := f.site(t, "no replay")
	var submitted atomic.Int32
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			io.Copy(io.Discard, r.Body)
			submitted.Add(1)
			entered <- struct{}{}
			select {
			case <-r.Context().Done():
			case <-release:
			}
			return
		}
		io.WriteString(w, "healthy")
	}))
	defer upstream.Close()
	defer close(release)
	svc := f.service(t, site, "write", upstream.URL)
	a, ta := registerAgent(t, f, site, "A")
	b, tb := registerAgent(t, f, site, "B")
	ca := namedAgent(t, f, site, a.ID, ta, portOf(upstream.URL))
	cb := namedAgent(t, f, site, b.ID, tb, portOf(upstream.URL))
	cookie, host, _ := f.grant(t, svc, f.cookie)
	req, _ := http.NewRequest("POST", f.h.URL+"/", strings.NewReader("non-idempotent mutation"))
	req.Host = host
	req.AddCookie(cookie)
	req.Header.Set("Origin", "http://"+host)
	done := make(chan error, 1)
	go func() {
		r, e := (&http.Client{Timeout: 5 * time.Second}).Do(req)
		if r != nil {
			io.Copy(io.Discard, r.Body)
			r.Body.Close()
			if r.StatusCode == 200 {
				e = fmt.Errorf("lost POST unexpectedly succeeded")
			}
		}
		done <- e
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("POST not submitted")
	}
	owner := ""
	for _, c := range f.s.connectedAgents(site.ID) {
		if c.channels.Load() > 0 {
			owner = c.instanceID
			break
		}
	}
	if owner == a.ID {
		ca()
	} else if owner == b.ID {
		cb()
	} else {
		t.Fatal("missing request owner")
	}
	select {
	case <-done:
	case <-time.After(6 * time.Second):
		t.Fatal("lost POST did not terminate")
	}
	if submitted.Load() != 1 {
		t.Fatal("submitted POST replayed", submitted.Load())
	}
	waitFor(t, func() bool { return len(f.s.connectedAgents(site.ID)) == 1 })
	res, data := f.request(t, "GET", "/", nil, cookie, host)
	if res.StatusCode != 200 || string(data) != "healthy" {
		t.Fatal("surviving Agent failed")
	}
}
