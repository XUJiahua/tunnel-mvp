package control

import (
	"context"
	"encoding/json"
	"github.com/XUJiahua/tunnel-mvp/internal/config"
	"github.com/XUJiahua/tunnel-mvp/internal/state"
	"golang.org/x/oauth2"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func syncFixture(t *testing.T) (*fixture, *keycloakSync, User, *http.Cookie, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	f := setup(t)
	var mode, tokenCalls atomic.Int32
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			tokenCalls.Add(1)
			user, password, ok := r.BasicAuth()
			user, _ = url.QueryUnescape(user)
			password, _ = url.QueryUnescape(password)
			if !ok || user != "reader" || password != "fixture-secret+/=" {
				t.Error("service credential encoding incorrect")
			}
			json.NewEncoder(w).Encode(map[string]any{"access_token": "fixture-access-token", "expires_in": 300})
			return
		}
		if r.Header.Get("Authorization") != "Bearer fixture-access-token" {
			t.Error("service token missing")
		}
		switch mode.Load() {
		case 1:
			http.Error(w, "unavailable", 503)
		case 2:
			json.NewEncoder(w).Encode(map[string]any{"id": "subject-a", "enabled": false})
		case 3:
			http.NotFound(w, r)
		case 4:
			json.NewEncoder(w).Encode(map[string]any{"id": "other-user", "enabled": true})
		default:
			json.NewEncoder(w).Encode(map[string]any{"id": "subject-a", "enabled": true})
		}
	}))
	t.Cleanup(idp.Close)
	u, passwordCookie := f.member(t, nil, nil)
	issuer := idp.URL + "/realms/fixture"
	if e := f.s.store.update(func(st *State) error {
		for i := range st.Users {
			if st.Users[i].ID == u.ID {
				st.Users[i].Identity = &state.Identity{Issuer: issuer, Subject: "subject-a"}
			}
		}
		return nil
	}); e != nil {
		t.Fatal(e)
	}
	u, _ = f.s.store.user(u.ID)
	// Binding changes invalidate existing sessions on shared storage.
	loginResponse, _ := f.request(t, "POST", "/api/login", map[string]string{"username": u.Username, "password": testPassword}, nil, "")
	if loginResponse.StatusCode != 200 {
		t.Fatal("recovery login after binding change failed")
	}
	passwordCookie = loginResponse.Cookies()[0]
	f.s.cfg.OIDC = config.OIDC{Issuer: issuer, ClientID: "browser", KeycloakSync: config.KeycloakSync{Enabled: true, ClientID: "reader", SubjectIsUserID: true, PollSeconds: 1, MaxStaleSeconds: 10}}
	f.s.oidc = &oidcLogin{config: oauth2.Config{Endpoint: oauth2.Endpoint{TokenURL: issuer + "/protocol/openid-connect/token"}}}
	t.Setenv("TUNNEL_KEYCLOAK_SYNC_CLIENT_SECRET", "fixture-secret+/=")
	if e := f.s.configureKeycloakSync(); e != nil {
		t.Fatal(e)
	}
	k := f.s.identitySync
	k.tokenURL = idp.URL + "/token"
	k.adminURL = idp.URL + "/admin/realms/fixture"
	return f, k, u, passwordCookie, &mode, &tokenCalls
}
func issueSyncCookie(t *testing.T, f *fixture, u User) *http.Cookie {
	t.Helper()
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", f.h.URL+"/", nil)
	f.s.accessMu.Lock()
	f.s.issueSession(w, u, time.Now().Add(time.Minute), sessionMetadata(req, "oidc", u.Identity.Issuer, u.Identity.Subject, "sid-fixture"))
	f.s.accessMu.Unlock()
	return w.Result().Cookies()[0]
}
func TestKeycloakSyncDisableRevokesOIDCButPreservesPassword(t *testing.T) {
	f, k, u, passwordCookie, mode, tokenCalls := syncFixture(t)
	if result := k.check(context.Background(), u); result.err != nil || !result.enabled {
		t.Fatal("enabled identity did not verify", result.err)
	}
	cookie := issueSyncCookie(t, f, u)
	for _, c := range []*http.Cookie{cookie, passwordCookie} {
		r, _ := f.request(t, "GET", "/api/me", nil, c, "")
		if r.StatusCode != 200 {
			t.Fatal("expected authenticated session")
		}
	}
	mode.Store(2)
	if result := k.check(context.Background(), u); result.err != nil || result.enabled {
		t.Fatal("disabled identity not detected")
	}
	r, _ := f.request(t, "GET", "/api/me", nil, cookie, "")
	if r.StatusCode != 401 {
		t.Fatal("disabled IdP cookie remained usable")
	}
	r, _ = f.request(t, "GET", "/api/me", nil, passwordCookie, "")
	if r.StatusCode != 200 {
		t.Fatal("independent local recovery password session revoked")
	}
	if tokenCalls.Load() != 1 || f.s.stats.identitySyncRevocations.Load() != 1 {
		t.Fatal("token cache or revocation accounting incorrect")
	}
	mode.Store(0)
	if result := k.check(context.Background(), u); result.err != nil || !result.enabled {
		t.Fatal("re-enabled identity did not verify")
	}
	r, _ = f.request(t, "GET", "/api/me", nil, cookie, "")
	if r.StatusCode != 401 {
		t.Fatal("old revoked cookie revived")
	}
}
func TestKeycloakSyncFailureExpiresAndFailsClosed(t *testing.T) {
	f, k, u, local, mode, _ := syncFixture(t)
	k.maxStale = 60 * time.Millisecond
	if result := k.check(context.Background(), u); result.err != nil {
		t.Fatal(result.err)
	}
	cookie := issueSyncCookie(t, f, u)
	mode.Store(1)
	if result := k.check(context.Background(), u); result.err == nil {
		t.Fatal("unavailable API accepted")
	}
	time.Sleep(80 * time.Millisecond)
	r, _ := f.request(t, "GET", "/api/me", nil, cookie, "")
	if r.StatusCode != 401 {
		t.Fatal("stale status allowed new request")
	}
	k.expire()
	_, exists, err := f.s.authSession(context.Background(), digest(cookie.Value))
	if err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("expired OIDC session not removed")
	}
	r, _ = f.request(t, "GET", "/api/me", nil, local, "")
	if r.StatusCode != 200 {
		t.Fatal("provider outage blocked local recovery")
	}
}
func TestKeycloakSyncRejectsWrongUserAndDeletion(t *testing.T) {
	_, k, u, _, mode, _ := syncFixture(t)
	mode.Store(4)
	if result := k.check(context.Background(), u); result.err == nil {
		t.Fatal("wrong admin user mapping accepted")
	}
	mode.Store(3)
	if result := k.check(context.Background(), u); result.err != nil || result.enabled {
		t.Fatal("deleted identity not denied")
	}
}
func TestKeycloakSyncCheckDeduplicatesConcurrentRequests(t *testing.T) {
	_, k, u, _, _, tokenCalls := syncFixture(t)
	var wg sync.WaitGroup
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if result := k.check(context.Background(), u); result.err != nil {
				t.Error(result.err)
			}
		}()
	}
	wg.Wait()
	if tokenCalls.Load() != 1 {
		t.Fatal("service token refresh was not serialized")
	}
}
func TestKeycloakSyncConfigurationRequiresExplicitSubjectMapping(t *testing.T) {
	cfg := config.Server{PublicURL: "http://localhost:18080", AccessURL: "http://apps.localhost:18080", OIDC: config.OIDC{Issuer: "http://localhost:8080/realms/test", ClientID: "browser", KeycloakSync: config.KeycloakSync{Enabled: true, ClientID: "reader"}}}
	if cfg.Validate() == nil {
		t.Fatal("implicit Keycloak subject mapping accepted")
	}
	cfg.OIDC.KeycloakSync.SubjectIsUserID = true
	if e := cfg.Validate(); e != nil {
		t.Fatal(e)
	}
	cfg.OIDC.KeycloakSync.MaxStaleSeconds = 1
	if cfg.Validate() == nil {
		t.Fatal("invalid verification staleness accepted")
	}
}

type syncTransportFunc func(*http.Request) (*http.Response, error)

func (f syncTransportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestKeycloakSyncBindingChangeDuringCheck(t *testing.T) {
	f, k, u, _, _, _ := syncFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	k.client.Transport = syncTransportFunc(func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Path, "/users/") {
			close(entered)
			<-release
		}
		return http.DefaultTransport.RoundTrip(r)
	})
	done := make(chan syncResult, 1)
	go func() { done <- k.check(context.Background(), u) }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("status request did not start")
	}
	res, b := f.request(t, "PATCH", "/api/users/"+u.ID+"/identity", map[string]string{"subject": "changed-user"}, f.cookie, "")
	if res.StatusCode != 200 {
		t.Fatalf("rebind failed: %s", b)
	}
	once.Do(func() { close(release) })
	if result := <-done; result.err == nil {
		t.Fatal("old identity check authorized a rebound member")
	}
	k.mu.Lock()
	_, cached := k.records[u.ID]
	k.mu.Unlock()
	if cached {
		t.Fatal("old status cached after identity rebind")
	}
}
func TestKeycloakSyncDoesNotForwardCredentialsOnRedirect(t *testing.T) {
	_, k, u, _, _, _ := syncFixture(t)
	var forwarded atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { forwarded.Add(1); w.WriteHeader(200) }))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 302) }))
	defer redirect.Close()
	k.adminURL = redirect.URL
	if result := k.check(context.Background(), u); result.err == nil {
		t.Fatal("redirected user status accepted")
	}
	k.tokenMu.Lock()
	k.token = ""
	k.tokenMu.Unlock()
	k.tokenURL = redirect.URL
	if result := k.check(context.Background(), u); result.err == nil {
		t.Fatal("redirected service token accepted")
	}
	if forwarded.Load() != 0 {
		t.Fatal("credentials forwarded to redirect target")
	}
}
