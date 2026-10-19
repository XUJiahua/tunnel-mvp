package control

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/XUJiahua/tunnel-mvp/internal/agent"
	"github.com/XUJiahua/tunnel-mvp/internal/config"
)

func TestWatchedACLRejectsInvalidAndRevokesOldPolicy(t *testing.T) {
	f := setup(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "allowed") }))
	defer upstream.Close()
	site, token := f.site(t, "watched")
	svc := f.service(t, site, "app", upstream.URL)
	c := config.Agent{ServerURL: f.h.URL, SiteID: site.ID, Allow: []config.Rule{{CIDR: "127.0.0.0/8", Ports: []int{portOf(upstream.URL)}}}}
	path := filepath.Join(t.TempDir(), "agent.json")
	raw, _ := json.Marshal(c)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	var loads atomic.Int64
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- agent.RunWatched(ctx, c, token, func() (config.Agent, error) { next, err := config.AgentFrom(path, nil); loads.Add(1); return next, err })
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(3 * time.Second):
			t.Error("watcher did not stop")
		}
	})
	waitFor(t, func() bool { return f.s.tunnel.HasSession(site.ID) })
	access, host, _ := f.grant(t, svc, f.cookie)
	_, b := f.request(t, "GET", "/", nil, access, host)
	if string(b) != "allowed" {
		t.Fatal("initial policy failed")
	}
	if err := os.WriteFile(path, []byte(`{"allow":`), 0600); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return loads.Load() > 0 })
	_, b = f.request(t, "GET", "/", nil, access, host)
	if string(b) != "allowed" {
		t.Fatal("invalid config replaced good policy")
	}
	c.Allow = []config.Rule{{CIDR: "127.0.0.0/8", Ports: []int{1}}}
	raw, _ = json.Marshal(c)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		if loads.Load() < 2 {
			return false
		}
		_, b := f.request(t, "POST", "/api/services/"+svc.ID+"/probe", nil, f.cookie, "")
		report := mustJSON[probeReport](t, b)
		if report.Failure != nil && report.Code == "acl_denied" {
			return true
		}
		time.Sleep(100 * time.Millisecond)
		return false
	})
	res, _ := f.request(t, "GET", "/", nil, access, host)
	if res.StatusCode != 502 {
		t.Fatal("old pool bypassed reloaded ACL")
	}
}
