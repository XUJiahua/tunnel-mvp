package control

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestPostgresClusterSharedHealthAuditAndLocalDrain(t *testing.T) {
	f := newClusterFixture(t)
	var posts atomic.Int32
	up := clusterUpstream(t, &posts)
	site, token := f.site(t, "observation site")
	f.connectAgent(t, site, token, portOf(up.URL))
	svc := f.service(t, site, "observation app", up.URL)
	cookie, host, _ := f.grant(t, svc, f.cookie)
	f.selected.Store(0)
	stream, ws := clusterStreams(t, f, svc, cookie, host)
	// B emits a real probe and audit; the console request moves to A.
	f.selected.Store(1)
	res, body := f.request(t, "POST", "/api/services/"+svc.ID+"/probe", nil, f.cookie, "")
	if res.StatusCode != 200 {
		t.Fatalf("probe %d %s", res.StatusCode, body)
	}
	f.selected.Store(0)
	waitFor(t, func() bool {
		f.nodes[1].observer.sync(context.Background())
		res, body = f.request(t, "GET", "/api/services/"+svc.ID+"/history", nil, f.cookie, "")
		var result struct{ Samples []healthSample }
		return res.StatusCode == 200 && json.Unmarshal(body, &result) == nil && len(result.Samples) > 0 && result.Samples[len(result.Samples)-1].NodeID == "node-b"
	})
	res, body = f.request(t, "GET", "/api/status", nil, f.cookie, "")
	if res.StatusCode != 200 || !strings.Contains(string(body), `"nodeID":"node-b"`) {
		t.Fatalf("A status omitted B probe %d %s", res.StatusCode, body)
	}
	waitFor(t, func() bool {
		f.nodes[1].observer.sync(context.Background())
		res, body = f.request(t, "GET", "/api/audit?nodeID=node-b&event=health_changed", nil, f.cookie, "")
		return res.StatusCode == 200 && strings.Contains(string(body), "node-b") && strings.Contains(string(body), svc.ID)
	})
	// Node targeting protects against an operator accidentally hitting another
	// listener through a load balancer.
	res, body = f.request(t, "POST", "/api/cluster/local/drain", map[string]any{"nodeID": "node-b", "draining": true}, f.cookie, "")
	if res.StatusCode != 409 {
		t.Fatalf("wrong node drain %d %s", res.StatusCode, body)
	}
	f.selected.Store(1)
	res, body = f.request(t, "POST", "/api/cluster/local/drain", map[string]any{"nodeID": "node-b", "draining": true}, f.cookie, "")
	if res.StatusCode != 200 {
		t.Fatalf("drain %d %s", res.StatusCode, body)
	}
	res, _ = f.request(t, "GET", "/healthz", nil, nil, "")
	if res.StatusCode != 503 {
		t.Fatal("draining node remained healthy for LB")
	}
	res, _ = f.request(t, "GET", "/", nil, cookie, host)
	if res.StatusCode != 503 {
		t.Fatal("draining node admitted public application request")
	}
	// Existing relay channels and their continuously renewed grants survive drain.
	ws.SetReadDeadline(time.Now().Add(3 * time.Second))
	if err := ws.WriteMessage(websocket.TextMessage, []byte("after-drain")); err != nil {
		t.Fatal(err)
	}
	if _, b, err := ws.ReadMessage(); err != nil || string(b) != "after-drain" {
		t.Fatal("drain terminated established stream", err)
	}
	f.selected.Store(0)
	waitFor(t, func() bool {
		view, err := f.nodes[0].cluster.repo.ClusterRoutes(context.Background(), site.ID)
		return err == nil && len(view.Routes) == 0
	})
	res, body = f.request(t, "GET", "/api/cluster/status", nil, f.cookie, "")
	if res.StatusCode != 200 || !strings.Contains(string(body), `"draining":true`) {
		t.Fatalf("shared drain state missing %d %s", res.StatusCode, body)
	}
	f.selected.Store(1)
	res, body = f.request(t, "POST", "/api/cluster/local/drain", map[string]any{"nodeID": "node-b", "draining": false}, f.cookie, "")
	if res.StatusCode != 200 {
		t.Fatalf("undrain %d %s", res.StatusCode, body)
	}
	res, _ = f.request(t, "GET", "/healthz", nil, nil, "")
	if res.StatusCode != http.StatusOK {
		t.Fatal("undrain did not restore health")
	}
	waitFor(t, func() bool {
		view, err := f.nodes[0].cluster.repo.ClusterRoutes(context.Background(), site.ID)
		return err == nil && len(view.Routes) == 1
	})
	stream.Body.Close()
	ws.Close()
}

func TestPostgresClusterObservationAmbiguousCommitKeepsLocalQueue(t *testing.T) {
	f := newClusterFixture(t)
	node := f.nodes[1]
	// Pause only this node's ticker publisher; heartbeat and authorization renewal
	// remain real and active. Drive the production sync routine deterministically.
	node.observer.mu.Lock()
	defer node.observer.mu.Unlock()
	manual := &observationRuntime{server: node, repo: f.replica}
	node.audit("ambiguous_observation", "admin", "", "", map[string]any{"nodeID": "forged", "eventID": "forged", "nodeEpoch": "forged", "sourceID": "forged"})
	f.replica.lostObservationCommit.Store(true)
	manual.sync(context.Background())
	queued, err := node.history.pendingObservations(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	eventID := ""
	for _, entry := range queued {
		if strings.Contains(string(entry.Record.Payload), "ambiguous_observation") {
			eventID = entry.Record.ID
			if entry.Record.NodeID != "node-b" || entry.Record.Epoch != node.cluster.node.Epoch || entry.Record.Source != node.history.observer.Source {
				t.Fatal("caller forged audit provenance")
			}
		}
	}
	if eventID == "" {
		t.Fatal("unacknowledged commit deleted its local recovery source")
	}
	query := func() int {
		res, body := f.request(t, "GET", "/api/audit?nodeID=node-b&event=ambiguous_observation", nil, f.cookie, "")
		if res.StatusCode != 200 {
			t.Fatalf("shared audit %d %s", res.StatusCode, body)
		}
		var page struct{ Entries []map[string]any }
		if err = json.Unmarshal(body, &page); err != nil {
			t.Fatal(err)
		}
		return len(page.Entries)
	}
	if query() != 1 {
		t.Fatal("actual ambiguous commit was not visible")
	}
	manual.sync(context.Background())
	queued, err = node.history.pendingObservations(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range queued {
		if entry.Record.ID == eventID {
			t.Fatal("successful retry was not acknowledged")
		}
	}
	if query() != 1 {
		t.Fatal("ambiguous retry counted the same event twice")
	}
}
