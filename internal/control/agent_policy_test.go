package control

import (
	"context"
	"github.com/XUJiahua/tunnel-mvp/internal/agent"
	"github.com/XUJiahua/tunnel-mvp/internal/config"
	"github.com/XUJiahua/tunnel-mvp/internal/state"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func waitPolicy(t *testing.T, s *Server, site, version string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		cs := s.connectedAgents(site)
		if len(cs) == 1 && cs[0].policyVersion == version {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("Agent did not apply policy")
}
func TestRemotePolicyRealAgentNarrowingAndRevoke(t *testing.T) {
	f := setup(t)
	site, _ := f.site(t, "policy site")
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/events" {
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "data: ready\n\n")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		io.WriteString(w, "allowed")
	}))
	defer up.Close()
	outside := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "outside local ACL") }))
	defer outside.Close()
	svc := f.service(t, site, "allowed", up.URL)
	forbidden := f.service(t, site, "outside", outside.URL)
	a, token := registerAgent(t, f, site, "constrained")
	path := "/api/sites/" + site.ID + "/agents/" + a.ID + "/policy"
	update := func(rules []state.NetworkRule) state.AgentPolicy {
		res, b := f.request(t, "PUT", path, map[string]any{"allow": rules}, f.cookie, "")
		if res.StatusCode != 200 {
			t.Fatalf("policy update: %s", b)
		}
		return mustJSON[state.AgentPolicy](t, b)
	}
	broad := update([]state.NetworkRule{{CIDR: "0.0.0.0/0", Ports: []int{portOf(up.URL), portOf(outside.URL)}}})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- agent.Run(ctx, config.Agent{ServerURL: f.h.URL, SiteID: site.ID, InstanceID: a.ID, RemotePolicy: true, Allow: []config.Rule{{CIDR: "127.0.0.0/8", Ports: []int{portOf(up.URL)}}}}, token)
	}()
	defer func() { cancel(); <-done }()
	waitPolicy(t, f.s, site.ID, broad.Version)
	cookie, host, _ := f.grant(t, svc, f.cookie)
	res, b := f.request(t, "GET", "/", nil, cookie, host)
	if res.StatusCode != 200 || string(b) != "allowed" {
		t.Fatal("intersection blocked allowed target")
	}
	fc, fh, _ := f.grant(t, forbidden, f.cookie)
	res, _ = f.request(t, "GET", "/", nil, fc, fh)
	if res.StatusCode != 502 {
		t.Fatal("remote policy widened local ACL")
	}
	req, _ := http.NewRequest("GET", f.h.URL+"/events", nil)
	req.Host = host
	req.AddCookie(cookie)
	stream, e := http.DefaultClient.Do(req)
	if e != nil {
		t.Fatal(e)
	}
	defer stream.Body.Close()
	if stream.StatusCode != 200 {
		t.Fatal("SSE failed")
	}
	buf := make([]byte, 13)
	if _, e = io.ReadFull(stream.Body, buf); e != nil {
		t.Fatal(e)
	}
	empty := update([]state.NetworkRule{})
	waitPolicy(t, f.s, site.ID, empty.Version)
	closed := make(chan error, 1)
	go func() { _, e := stream.Body.Read(make([]byte, 1)); closed <- e }()
	select {
	case e := <-closed:
		if e == nil {
			t.Fatal("old stream survived narrowing")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("old stream remained open")
	}
	res, _ = f.request(t, "GET", "/", nil, cookie, host)
	if res.StatusCode != 502 {
		t.Fatal("empty remote policy allowed traffic")
	}
	update([]state.NetworkRule{{CIDR: "127.0.0.0/8", Ports: []int{portOf(up.URL)}}})
	waitPolicy(t, f.s, site.ID, state.PolicyVersion([]state.NetworkRule{{CIDR: "127.0.0.0/8", Ports: []int{portOf(up.URL)}}}))
	res, _ = f.request(t, "GET", "/", nil, cookie, host)
	if res.StatusCode != 200 {
		t.Fatal("valid new policy did not restore traffic")
	}
	// Public policy endpoint binds instance credentials and rejects browser Origin.
	request := func(origin, credential string) int {
		r, _ := http.NewRequest("GET", f.h.URL+"/tunnel/policy", nil)
		r.Header.Set("X-Tunnel-Site", site.ID)
		r.Header.Set("X-Tunnel-Instance", a.ID)
		r.Header.Set("Authorization", "Bearer "+credential)
		r.Header.Set("Origin", origin)
		response, e := http.DefaultClient.Do(r)
		if e != nil {
			t.Fatal(e)
		}
		response.Body.Close()
		return response.StatusCode
	}
	if request("", token) != 200 || request(f.h.URL, token) != 403 || request("", strings.Repeat("x", 48)) != 401 {
		t.Fatal("policy credential/origin check failed")
	}
}
