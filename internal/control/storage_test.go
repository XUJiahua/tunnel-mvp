package control

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/XUJiahua/tunnel-mvp/internal/config"
	"github.com/XUJiahua/tunnel-mvp/internal/state"
)

func TestLiveJSONToSQLiteMigrationPreservesAccess(t *testing.T) {
	var handler atomic.Value
	h := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handler.Load().(http.Handler).ServeHTTP(w, r) }))
	c := config.Server{PublicURL: "http://" + h.Listener.Addr().String(), AccessURL: "http://apps.localhost:" + strconv.Itoa(h.Listener.Addr().(*net.TCPAddr).Port), DataDir: t.TempDir(), Storage: state.JSON}
	s, err := New(c, testPassword)
	if err != nil {
		t.Fatal(err)
	}
	handler.Store(s.Handler())
	h.Start()
	f := &fixture{s: s, h: h, dir: c.DataDir}
	t.Cleanup(func() { h.Close(); f.s.Close() })
	res, b := f.request(t, "POST", "/api/login", map[string]string{"username": "admin", "password": testPassword}, nil, "")
	if res.StatusCode != 200 {
		t.Fatalf("login: %s", b)
	}
	f.cookie = res.Cookies()[0]
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "migration works") }))
	defer upstream.Close()
	site, token := f.site(t, "migrated")
	f.agent(t, site, token, portOf(upstream.URL))
	svc := f.service(t, site, "platform", upstream.URL)
	_, login := f.member(t, nil, []string{svc.ID})
	oldAccess, host, _ := f.grant(t, svc, login)
	_, b = f.request(t, "GET", "/", nil, oldAccess, host)
	if string(b) != "migration works" {
		t.Fatal("pre-migration access failed")
	}
	// Retain the HTTP listener but stop the old control plane and release its
	// directory lock. The same running Agent reconnects using its original token.
	s.Close()
	if _, err = state.MigrateJSON(c.DataDir); err != nil {
		t.Fatal(err)
	}
	c.Storage = state.SQLite
	next, err := New(c, "")
	if err != nil {
		t.Fatal(err)
	}
	f.s = next
	handler.Store(next.Handler())
	waitFor(t, func() bool { return next.tunnel.HasSession(site.ID) })
	res, _ = f.request(t, "GET", "/", nil, oldAccess, host)
	if res.StatusCode != 401 {
		t.Fatal("old in-memory grant survived restart")
	}
	res, b = f.request(t, "POST", "/api/login", map[string]string{"username": "member", "password": testPassword}, nil, "")
	if res.StatusCode != 200 {
		t.Fatalf("member password changed during migration: %s", b)
	}
	login = res.Cookies()[0]
	_, b = f.request(t, "GET", "/api/services", nil, login, "")
	services := mustJSON[[]Service](t, b)
	if len(services) != 1 || services[0].ID != svc.ID {
		t.Fatal("application grant or ID changed during migration")
	}
	access, newHost, _ := f.grant(t, svc, login)
	if newHost != host {
		t.Fatal("public application hostname changed")
	}
	res, b = f.request(t, "GET", "/", nil, access, newHost)
	if res.StatusCode != 200 || string(b) != "migration works" {
		t.Fatalf("migrated tunnel access: %d %s", res.StatusCode, b)
	}
}

func TestFailedServerStartupReleasesStateLock(t *testing.T) {
	t.Setenv("TUNNEL_METRICS_TOKEN", "too-short")
	c := config.Server{PublicURL: "http://localhost:8080", AccessURL: "http://apps.localhost:8080", DataDir: t.TempDir(), Storage: state.SQLite}
	if _, err := New(c, testPassword); err == nil {
		t.Fatal("invalid startup unexpectedly succeeded")
	}
	if _, err := state.Inspect(c.DataDir, state.SQLite); err != nil {
		t.Fatal("failed startup leaked directory lock", err)
	}
}
