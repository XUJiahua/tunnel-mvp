package control

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/XUJiahua/tunnel-mvp/internal/config"
	"github.com/XUJiahua/tunnel-mvp/internal/state"
	"github.com/XUJiahua/tunnel-mvp/internal/testpostgres"
	"github.com/gorilla/websocket"
	"golang.org/x/crypto/bcrypt"
)

type interruptedAuthRepository struct {
	*state.PostgresRepository
	blockProof atomic.Bool
	dropPoll   bool
	writeGate  atomic.Pointer[authWriteGate]
}

func (p *interruptedAuthRepository) AuthSnapshotForKeys(ctx context.Context, a, b, c []string) (state.AuthSnapshot, error) {
	if p.blockProof.Load() {
		<-ctx.Done()
		return state.AuthSnapshot{}, ctx.Err()
	}
	return p.PostgresRepository.AuthSnapshotForKeys(ctx, a, b, c)
}
func (p *interruptedAuthRepository) AuthPoll(ctx context.Context, node string, n int) (state.AuthEvents, error) {
	if p.dropPoll {
		return state.AuthEvents{}, errors.New("injected notification loss")
	}
	return p.PostgresRepository.AuthPoll(ctx, node, n)
}

func sharedHTTPNodes(t *testing.T, oidc config.OIDC, identity *state.Identity) (*fixture, *Server, *interruptedAuthRepository, *atomic.Int32) {
	t.Helper()
	ctx := context.Background()
	dsn := testpostgres.Database(t)
	hash, e := bcrypt.GenerateFromPassword([]byte(testPassword), bcrypt.DefaultCost)
	if e != nil {
		t.Fatal(e)
	}
	seed := State{Users: []User{{ID: "admin", Username: "admin", Role: "admin", PasswordHash: string(hash), SiteIDs: []string{}, ServiceIDs: []string{}}}}
	if identity != nil {
		seed.Users = append(seed.Users, User{ID: "external", Username: "external", Role: "viewer", Identity: identity, SiteIDs: []string{}, ServiceIDs: []string{}})
	}
	p, e := state.OpenPostgres(ctx, dsn, func() (State, error) { return seed, nil })
	if e != nil {
		t.Fatal(e)
	}
	q, e := state.OpenPostgres(ctx, dsn, nil)
	if e != nil {
		_ = p.Close()
		t.Fatal(e)
	}
	replica := &interruptedAuthRepository{PostgresRepository: q, dropPoll: true}
	lb := httptest.NewUnstartedServer(nil)
	cfg := config.Server{PublicURL: "http://" + lb.Listener.Addr().String(), AccessURL: "http://apps.localhost:" + strconv.Itoa(lb.Listener.Addr().(*net.TCPAddr).Port), Storage: state.Postgres, DataDir: t.TempDir(), OIDC: oidc}
	a, e := newServer(cfg, "", p)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(a.Close)
	cfg.DataDir = t.TempDir()
	b, e := newServer(cfg, "", replica)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(b.Close)
	ha, hb := httptest.NewServer(a.Handler()), httptest.NewServer(b.Handler())
	t.Cleanup(ha.Close)
	t.Cleanup(hb.Close)
	ua, _ := url.Parse(ha.URL)
	ub, _ := url.Parse(hb.URL)
	proxies := []*httputil.ReverseProxy{httputil.NewSingleHostReverseProxy(ua), httputil.NewSingleHostReverseProxy(ub)}
	selected := &atomic.Int32{}
	lb.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := selected.Load()
		if strings.HasPrefix(r.URL.Path, "/tunnel") {
			n = 1
		}
		proxies[n].ServeHTTP(w, r)
	})
	lb.Start()
	t.Cleanup(lb.Close)
	f := &fixture{s: b, h: lb, dir: cfg.DataDir}
	return f, a, replica, selected
}

// The private constructor bypasses only the production single-active guard.
// Two independent repositories, two HTTP servers and a real load balancer are
// required here; agent ownership remains on B until HA-3 implements routing.
func TestPostgresSharedAuthAcrossHTTPNodes(t *testing.T) {
	for _, mode := range []string{"notification-loss", "blocked-proof"} {
		t.Run(mode, func(t *testing.T) {
			f, _, replica, selected := sharedHTTPNodes(t, config.OIDC{}, nil)
			response, _ := f.request(t, "POST", "/api/login", map[string]string{"username": "admin", "password": testPassword}, nil, "")
			if response.StatusCode != 200 {
				t.Fatal("login on A failed", response.StatusCode)
			}
			f.cookie = response.Cookies()[0]
			selected.Store(1)
			response, _ = f.request(t, "GET", "/api/me", nil, f.cookie, "")
			if response.StatusCode != 200 {
				t.Fatal("A cookie rejected on B", response.StatusCode)
			}
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/events":
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprint(w, "data: ready\n\n")
					w.(http.Flusher).Flush()
					<-r.Context().Done()
				case "/ws":
					c, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
					if err != nil {
						return
					}
					defer c.Close()
					for {
						kind, data, err := c.ReadMessage()
						if err != nil {
							return
						}
						if c.WriteMessage(kind, data) != nil {
							return
						}
					}
				default:
					io.WriteString(w, "shared auth proxy")
				}
			}))
			defer up.Close()
			site, token := f.site(t, "shared site")
			stopAgent := f.agent(t, site, token, portOf(up.URL))
			defer stopAgent()
			svc := f.service(t, site, "shared app", up.URL)
			// Mint on B, redeem on A, replay on B.
			response, body := f.request(t, "POST", "/api/services/"+svc.ID+"/launch", nil, f.cookie, "")
			if response.StatusCode != 200 {
				t.Fatal(string(body))
			}
			dest, _ := url.Parse(mustJSON[map[string]string](t, body)["url"])
			selected.Store(0)
			response, body = f.request(t, "GET", dest.RequestURI(), nil, nil, dest.Host)
			if response.StatusCode != 303 {
				t.Fatal("cross-node ticket redemption", response.StatusCode, string(body))
			}
			access := response.Cookies()[0]
			selected.Store(1)
			response, _ = f.request(t, "GET", dest.RequestURI(), nil, nil, dest.Host)
			if response.StatusCode != 401 {
				t.Fatal("one-time ticket replay accepted")
			}
			response, body = f.request(t, "GET", "/", nil, access, dest.Host)
			if response.StatusCode != 200 || string(body) != "shared auth proxy" {
				t.Fatal(response.StatusCode, string(body))
			}
			req, _ := http.NewRequest("GET", f.h.URL+"/events", nil)
			req.Host = dest.Host
			req.AddCookie(access)
			stream, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer stream.Body.Close()
			if stream.StatusCode != 200 {
				t.Fatal("SSE failed", stream.StatusCode)
			}
			if _, err = io.ReadFull(stream.Body, make([]byte, 13)); err != nil {
				t.Fatal(err)
			}
			ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(f.h.URL, "http")+"/ws", http.Header{"Host": {dest.Host}, "Cookie": {access.String()}})
			if err != nil {
				t.Fatal(err)
			}
			defer ws.Close()
			if err = ws.WriteMessage(websocket.TextMessage, []byte("ready")); err != nil {
				t.Fatal(err)
			}
			if _, msg, err := ws.ReadMessage(); err != nil || string(msg) != "ready" {
				t.Fatal(err)
			}
			if mode == "blocked-proof" {
				replica.blockProof.Store(true)
			}
			// A has no local copy of B's connections. Only shared revocation/leases can close them.
			selected.Store(0)
			response, _ = f.request(t, "POST", "/api/logout", nil, f.cookie, "")
			if response.StatusCode != 200 {
				t.Fatal("logout on A failed")
			}
			selected.Store(1)
			response, _ = f.request(t, "GET", "/", nil, access, dest.Host)
			if response.StatusCode != 401 {
				t.Fatal("revoked grant admitted on B")
			}
			response, _ = f.request(t, "GET", "/api/me", nil, f.cookie, "")
			if response.StatusCode != 401 {
				t.Fatal("revoked console session admitted on B")
			}
			closed := make(chan error, 1)
			go func() { _, err := stream.Body.Read(make([]byte, 1)); closed <- err }()
			select {
			case err := <-closed:
				if err == nil {
					t.Fatal("SSE survived revocation")
				}
			case <-time.After(authorizationLease + 2*time.Second):
				t.Fatal("cross-node SSE revocation exceeded lease")
			}
			_ = ws.SetReadDeadline(time.Now().Add(2 * time.Second))
			if _, _, err = ws.ReadMessage(); err == nil {
				t.Fatal("WebSocket survived revocation")
			}
		})
	}
}

func TestPostgresSharedOIDCCallbackAndLogoutAcrossNodes(t *testing.T) {
	idp := newTestIdP(t)
	f, _, _, selected := sharedHTTPNodes(t, config.OIDC{Issuer: idp.server.URL, ClientID: "console", SessionMinutes: 60}, &state.Identity{Issuer: idp.server.URL, Subject: "subject-A"})
	// The authorization redirect starts on A and its PKCE exchange completes on B.
	selected.Store(0)
	path, browser, _ := startFlow(t, f, idp)
	selected.Store(1)
	response, body := f.request(t, "GET", path, nil, browser, "")
	cookie := consoleSession(response)
	if cookie == nil {
		t.Fatal("cross-node OIDC callback failed", response.StatusCode, string(body))
	}
	selected.Store(0)
	response, _ = f.request(t, "GET", "/api/me", nil, cookie, "")
	if response.StatusCode != 200 {
		t.Fatal("OIDC cookie not shared")
	}
	response, _ = f.request(t, "GET", path, nil, browser, "")
	if consoleSession(response) != nil {
		t.Fatal("OIDC state replay accepted on other node")
	}
	token := signLogout(t, idp, logoutClaims(idp), false)
	if sendLogout(t, f, token) != 200 {
		t.Fatal("logout on A failed")
	}
	selected.Store(1)
	response, _ = f.request(t, "GET", "/api/me", nil, cookie, "")
	if response.StatusCode != 401 {
		t.Fatal("logout did not revoke session on B")
	}
	fresh := oidcLoginCookie(t, f, idp)
	if sendLogout(t, f, token) != 200 {
		t.Fatal("duplicate logout not idempotent")
	}
	response, _ = f.request(t, "GET", "/api/me", nil, fresh, "")
	if response.StatusCode != 200 {
		t.Fatal("cross-node JTI replay revoked a fresh session")
	}
	selected.Store(0)
	pending, binding, _ := startFlow(t, f, idp)
	selected.Store(1)
	if sendLogout(t, f, signLogout(t, idp, logoutClaims(idp), false)) != 200 {
		t.Fatal("pending logout failed")
	}
	selected.Store(0)
	response, _ = f.request(t, "GET", pending, nil, binding, "")
	if consoleSession(response) != nil {
		t.Fatal("cross-node pending flow resurrected logged out identity")
	}
}

type authWriteGate struct{ entered, release chan struct{} }

func (p *interruptedAuthRepository) UpdateAuthenticated(ctx context.Context, guard func(State, state.AuthData, time.Time) error, fn func(*State) error) error {
	if gate := p.writeGate.Swap(nil); gate != nil {
		close(gate.entered)
		select {
		case <-gate.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return p.PostgresRepository.UpdateAuthenticated(ctx, guard, fn)
}
func TestPostgresSharedAuthRejectsRevokedActorBeforeMutation(t *testing.T) {
	f, _, replica, selected := sharedHTTPNodes(t, config.OIDC{}, nil)
	login := func(node int32) *http.Cookie {
		selected.Store(node)
		response, _ := f.request(t, "POST", "/api/login", map[string]string{"username": "admin", "password": testPassword}, nil, "")
		if response.StatusCode != 200 {
			t.Fatal("login failed")
		}
		return response.Cookies()[0]
	}
	aCookie, bCookie := login(0), login(1)
	bSession, found, err := f.s.authSession(context.Background(), digest(bCookie.Value))
	if err != nil || !found {
		t.Fatal(err)
	}
	gate := &authWriteGate{entered: make(chan struct{}), release: make(chan struct{})}
	replica.writeGate.Store(gate)
	req, _ := http.NewRequest("POST", f.h.URL+"/api/sites", strings.NewReader(`{"name":"must-not-commit"}`))
	req.Header.Set("Origin", f.h.URL)
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(bCookie)
	type result struct {
		status int
		err    error
	}
	done := make(chan result, 1)
	go func() {
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			done <- result{err: err}
			return
		}
		defer response.Body.Close()
		_, _ = io.Copy(io.Discard, response.Body)
		done <- result{status: response.StatusCode}
	}()
	select {
	case <-gate.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("B never reached the transaction boundary")
	}
	selected.Store(0)
	response, body := f.request(t, "DELETE", "/api/sessions/"+bSession.ID, nil, aCookie, "")
	if response.StatusCode != 200 {
		close(gate.release)
		t.Fatal(response.StatusCode, string(body))
	}
	close(gate.release)
	select {
	case r := <-done:
		if r.err != nil || r.status < 400 {
			t.Fatal("revoked actor mutation committed", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("blocked writer did not finish")
	}
	if len(replica.Snapshot().Sites) != 0 {
		t.Fatal("unauthorized site was persisted")
	}
}
