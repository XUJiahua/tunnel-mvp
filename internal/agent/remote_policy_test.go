package agent

import (
	"context"
	"encoding/json"
	"github.com/XUJiahua/tunnel-mvp/internal/config"
	"github.com/XUJiahua/tunnel-mvp/internal/state"
	"github.com/rancher/remotedialer"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRemotePolicyIntersectionAndPinnedIP(t *testing.T) {
	local, e := NewPolicy([]config.Rule{{CIDR: "127.0.0.0/8", Ports: []int{80, 443}, Host: "allowed.test"}})
	if e != nil {
		t.Fatal(e)
	}
	remote, _ := NewPolicy([]config.Rule{{CIDR: "0.0.0.0/0", Ports: []int{80, 8080}}})
	local.remote = remote
	for _, tc := range []struct {
		host, ip, port string
		allowed        bool
	}{{"allowed.test", "127.0.0.1", "80", true}, {"allowed.test", "127.0.0.1", "443", false}, {"allowed.test", "127.0.0.1", "8080", false}, {"allowed.test", "10.0.0.1", "80", false}, {"foreign.test", "127.0.0.1", "80", false}} {
		if got := local.allowed(tc.host, tc.port, netip.MustParseAddr(tc.ip)); got != tc.allowed {
			t.Fatal("intersection widened local authorization", tc)
		}
	}
	local.remote = &Policy{}
	if local.allowed("allowed.test", "80", netip.MustParseAddr("127.0.0.1")) {
		t.Fatal("empty remote policy did not deny all")
	}
}
func TestRemotePolicyValidationExpiryAndNoRedirect(t *testing.T) {
	p := state.EmptyAgentPolicy()
	var mode atomic.Int32
	var handshakes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/tunnel/connect" {
			handshakes.Add(1)
			http.Error(w, "test no websocket", 503)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+strings.Repeat("x", 48) || r.Header.Get("X-Tunnel-Instance") != "instance-a" {
			t.Error("missing bound credentials")
		}
		switch mode.Load() {
		case 1:
			http.Error(w, "unavailable", 503)
		case 2:
			w.Write([]byte(`{"version":"bad","allow":[]}`))
		case 3:
			http.Redirect(w, r, "/redirected", 302)
		default:
			json.NewEncoder(w).Encode(p)
		}
	}))
	defer server.Close()
	c := config.Agent{ServerURL: server.URL, SiteID: "site-a", InstanceID: "instance-a", RemotePolicy: true, Allow: []config.Rule{{CIDR: "127.0.0.0/8", Ports: []int{80}}}}
	token := strings.Repeat("x", 48)
	for _, m := range []int32{1, 2, 3} {
		mode.Store(m)
		if _, e := fetchPolicy(context.Background(), c, token); e == nil {
			t.Fatal("accepted unavailable/invalid/redirected policy")
		}
	}
	mode.Store(1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runRemotePolicy(ctx, c, token, 10*time.Millisecond, 20*time.Millisecond) }()
	time.Sleep(40 * time.Millisecond)
	if handshakes.Load() != 0 {
		t.Fatal("tunnel opened before initial valid policy")
	}
	cancel()
	if e := <-done; e != nil {
		t.Fatal(e)
	}
	c.InstanceID = ""
	if c.Validate() == nil {
		t.Fatal("legacy instance opted into remote policy")
	}
}

func TestRemotePolicyExpiresClosesLiveTunnelAndRecovers(t *testing.T) {
	var unavailable atomic.Bool
	hub := remotedialer.New(func(r *http.Request) (string, bool, error) { return "instance-a", true, nil }, remotedialer.DefaultErrorWriter)
	p := state.EmptyAgentPolicy()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/tunnel/connect" {
			hub.ServeHTTP(w, r)
			return
		}
		if unavailable.Load() {
			http.Error(w, "unavailable", 503)
			return
		}
		json.NewEncoder(w).Encode(p)
	}))
	defer server.Close()
	c := config.Agent{ServerURL: server.URL, SiteID: "site-a", InstanceID: "instance-a", RemotePolicy: true, Allow: []config.Rule{{CIDR: "127.0.0.0/8", Ports: []int{80}}}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- runRemotePolicy(ctx, c, strings.Repeat("x", 48), 10*time.Millisecond, 40*time.Millisecond)
	}()
	defer func() {
		cancel()
		if e := <-done; e != nil {
			t.Error(e)
		}
	}()
	wait := func(want bool) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if hub.HasSession("instance-a") == want {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatal("unexpected live tunnel state", want)
	}
	wait(true)
	unavailable.Store(true)
	wait(false)
	unavailable.Store(false)
	wait(true)
}
