package control

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/XUJiahua/tunnel-mvp/internal/state"
)

type poolProofRepository struct {
	state.AuthRepository
	forKeys  func() state.AuthSnapshot
	snapshot func() state.AuthSnapshot
}

func (p *poolProofRepository) AuthSnapshotForKeys(context.Context, []string, []string, []string) (state.AuthSnapshot, error) {
	return p.forKeys(), nil
}
func (p *poolProofRepository) AuthSnapshot(context.Context, bool) (state.AuthSnapshot, error) {
	return p.snapshot(), nil
}
func (*poolProofRepository) AuthPoll(context.Context, string, int) (state.AuthEvents, error) {
	return state.AuthEvents{}, nil
}
func (*poolProofRepository) AuthAcknowledge(context.Context, string, uint64) error { return nil }

func TestSharedAuthProofPreservesPoolsCreatedDuringRead(t *testing.T) {
	for _, replacement := range []bool{false, true} {
		name := "new pool"
		if replacement {
			name = "replacement pool"
		}
		t.Run(name, func(t *testing.T) {
			var connections atomic.Int64
			upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "pooled") }))
			upstream.Config.ConnState = func(_ net.Conn, status http.ConnState) {
				if status == http.StateNew {
					connections.Add(1)
				}
			}
			upstream.Start()
			defer upstream.Close()
			fresh := &http.Transport{}
			defer fresh.CloseIdleConnections()
			old := &http.Transport{}
			defer old.CloseIdleConnections()
			svc := Service{ID: "app", URL: upstream.URL, SiteID: "site"}
			s := &Server{auth: newAuth(), pools: map[string]pooledTransport{}}
			if replacement {
				s.pools["app"] = pooledTransport{service: svc, transport: old}
			}
			request := func() {
				r, _ := http.NewRequest("GET", upstream.URL, nil)
				response, err := fresh.RoundTrip(r)
				if err != nil {
					t.Fatal(err)
				}
				_, err = io.Copy(io.Discard, response.Body)
				response.Body.Close()
				if err != nil {
					t.Fatal(err)
				}
			}
			s.auth.shared = &poolProofRepository{forKeys: func() state.AuthSnapshot {
				// This is the database-read boundary: the worker has already captured its
				// keys. A concurrent request creates/replaces a pool and warms its socket.
				s.poolMu.Lock()
				s.pools["app"] = pooledTransport{service: svc, transport: fresh}
				s.poolMu.Unlock()
				request()
				return state.AuthSnapshot{Data: state.NewAuthData(), Now: time.Now()}
			}}
			s.refreshSharedAuth(context.Background())
			if pool, ok := s.pools["app"]; !ok || pool.transport != fresh {
				t.Fatal("a proof for older pools deleted a new pool")
			}
			request()
			if connections.Load() != 1 {
				t.Fatalf("fresh pool lost its reusable socket: %d connections", connections.Load())
			}
		})
	}
}
func TestSharedAuthProofStillPrunesCapturedInvalidPool(t *testing.T) {
	transport := &http.Transport{}
	s := &Server{auth: newAuth(), pools: map[string]pooledTransport{"app": {service: Service{ID: "app"}, transport: transport}}}
	s.auth.shared = &poolProofRepository{forKeys: func() state.AuthSnapshot { return state.AuthSnapshot{Data: state.NewAuthData(), Now: time.Now()} }}
	s.refreshSharedAuth(context.Background())
	if len(s.pools) != 0 {
		t.Fatal("captured invalid pool survived its proof")
	}
}
func TestSessionCleanupPreservesPoolsCreatedDuringRead(t *testing.T) {
	old, fresh := &http.Transport{}, &http.Transport{}
	defer old.CloseIdleConnections()
	defer fresh.CloseIdleConnections()
	s := &Server{auth: newAuth(), pools: map[string]pooledTransport{"old": {sessionKey: "expired", transport: old}}}
	s.auth.shared = &poolProofRepository{snapshot: func() state.AuthSnapshot {
		s.poolMu.Lock()
		s.pools["fresh"] = pooledTransport{sessionKey: "new-session", transport: fresh}
		s.poolMu.Unlock()
		return state.AuthSnapshot{Data: state.NewAuthData(), Now: time.Now()}
	}}
	s.pruneSessionPools()
	if _, ok := s.pools["old"]; ok {
		t.Fatal("expired captured pool survived cleanup")
	}
	if pool, ok := s.pools["fresh"]; !ok || pool.transport != fresh {
		t.Fatal("old session snapshot deleted a newly authenticated pool")
	}
}
