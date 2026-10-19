package control

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/XUJiahua/tunnel-mvp/internal/config"
)

// A separate protocol peer: real discovery, code exchange, PKCE verification,
// RS256 signing and JWKS retrieval, rather than mocking our verifier.
type testIdP struct {
	server        *httptest.Server
	key, wrongKey *rsa.PrivateKey
	mu            sync.Mutex
	codes         map[string]url.Values
	mode          string
	exchanges     int
	beforeToken   func()
}

func newTestIdP(t *testing.T) *testIdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p := &testIdP{key: key, wrongKey: other, codes: map[string]url.Values{}}
	p.server = httptest.NewServer(http.HandlerFunc(p.serve))
	t.Cleanup(p.server.Close)
	t.Setenv("TUNNEL_OIDC_CLIENT_SECRET", "test-confidential-secret")
	return p
}
func (p *testIdP) serve(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/.well-known/openid-configuration":
		jsonOut(w, 200, map[string]any{"issuer": p.server.URL, "authorization_endpoint": p.server.URL + "/authorize", "token_endpoint": p.server.URL + "/token", "jwks_uri": p.server.URL + "/keys", "id_token_signing_alg_values_supported": []string{"RS256"}})
	case "/keys":
		enc := base64.RawURLEncoding.EncodeToString
		jsonOut(w, 200, map[string]any{"keys": []any{map[string]any{"kty": "RSA", "kid": "key1", "alg": "RS256", "use": "sig", "n": enc(p.key.N.Bytes()), "e": enc(big.NewInt(int64(p.key.E)).Bytes())}}})
	case "/authorize":
		q := r.URL.Query()
		if q.Get("client_id") != "console" || q.Get("response_type") != "code" || q.Get("scope") != "openid" || q.Get("code_challenge_method") != "S256" || q.Get("nonce") == "" {
			fail(w, 400, "invalid authorization request")
			return
		}
		code := randomID()
		p.mu.Lock()
		p.codes[code] = q
		p.mu.Unlock()
		dest, _ := url.Parse(q.Get("redirect_uri"))
		dest.RawQuery = url.Values{"code": {code}, "state": {q.Get("state")}}.Encode()
		http.Redirect(w, r, dest.String(), 302)
	case "/token":
		_ = r.ParseForm()
		client, secret, ok := r.BasicAuth()
		p.mu.Lock()
		q, exists := p.codes[r.Form.Get("code")]
		delete(p.codes, r.Form.Get("code"))
		mode, hook := p.mode, p.beforeToken
		p.exchanges++
		p.mu.Unlock()
		challenge := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
		if !ok || client != "console" || secret != "test-confidential-secret" || !exists || r.Form.Get("grant_type") != "authorization_code" || r.Form.Get("redirect_uri") != q.Get("redirect_uri") || base64.RawURLEncoding.EncodeToString(challenge[:]) != q.Get("code_challenge") {
			fail(w, 400, "invalid grant or PKCE")
			return
		}
		if mode == "pkce-error" {
			fail(w, 400, "invalid grant")
			return
		}
		if mode == "missing-token" {
			jsonOut(w, 200, map[string]string{"access_token": "private", "token_type": "Bearer"})
			return
		}
		if hook != nil {
			hook()
		}
		claims := map[string]any{"iss": p.server.URL, "sub": "subject-A", "aud": "console", "exp": time.Now().Add(30 * time.Minute).Unix(), "iat": time.Now().Unix(), "nonce": q.Get("nonce"), "email": "admin", "preferred_username": "admin", "roles": []string{"admin"}}
		signing := p.key
		switch mode {
		case "signature":
			signing = p.wrongKey
		case "nonce":
			claims["nonce"] = "other"
		case "issuer":
			claims["iss"] = "https://other.example"
		case "audience":
			claims["aud"] = "other"
		case "expired":
			claims["exp"] = time.Now().Add(-time.Minute).Unix()
		case "subject":
			claims["sub"] = ""
		case "unknown":
			claims["sub"] = "unbound"
		case "case":
			claims["sub"] = "subject-a"
		case "azp":
			claims["aud"] = []string{"console", "other"}
			claims["azp"] = "other"
		}
		header, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": "key1"})
		body, _ := json.Marshal(claims)
		enc := base64.RawURLEncoding.EncodeToString
		unsigned := enc(header) + "." + enc(body)
		sum := sha256.Sum256([]byte(unsigned))
		sig, _ := rsa.SignPKCS1v15(rand.Reader, signing, crypto.SHA256, sum[:])
		jsonOut(w, 200, map[string]any{"access_token": "never-forward-this", "token_type": "Bearer", "expires_in": 1800, "id_token": unsigned + "." + enc(sig)})
	default:
		http.NotFound(w, r)
	}
}
func oidcFixture(t *testing.T) (*fixture, *testIdP, User) {
	t.Helper()
	p := newTestIdP(t)
	f := setup(t)
	f.s.cfg.OIDC = config.OIDC{Issuer: p.server.URL, ClientID: "console", SessionMinutes: 60}
	if err := f.s.configureOIDC(); err != nil {
		t.Fatal(err)
	}
	res, b := f.request(t, "POST", "/api/users", map[string]any{"username": "external", "oidcSubject": "subject-A"}, f.cookie, "")
	if res.StatusCode != 201 {
		t.Fatalf("create OIDC member: %s", b)
	}
	return f, p, mustJSON[User](t, b)
}
func startFlow(t *testing.T, f *fixture, p *testIdP) (string, *http.Cookie, string) {
	t.Helper()
	res, _ := f.request(t, "GET", "/auth/oidc/start", nil, nil, "")
	if res.StatusCode != 302 {
		t.Fatalf("start: %d", res.StatusCode)
	}
	cookie := res.Cookies()[0]
	if cookie.Name != oidcCookie || !cookie.HttpOnly || cookie.Domain != "" || cookie.Path != "/auth/oidc" {
		t.Fatal("bad flow cookie")
	}
	authorize, _ := url.Parse(res.Header.Get("Location"))
	if authorize.Query().Get("redirect_uri") != f.h.URL+"/auth/oidc/callback" {
		t.Fatal("untrusted callback")
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	result, err := client.Get(authorize.String())
	if err != nil {
		t.Fatal(err)
	}
	result.Body.Close()
	callback, _ := url.Parse(result.Header.Get("Location"))
	if result.StatusCode != 302 {
		t.Fatal("authorization failed")
	}
	return callback.RequestURI(), cookie, authorize.Query().Get("state")
}
func consoleSession(res *http.Response) *http.Cookie {
	for _, c := range res.Cookies() {
		if c.Name == consoleCookie && c.MaxAge > 0 {
			return c
		}
	}
	return nil
}
func TestOIDCProtocolAndMemberLifecycle(t *testing.T) {
	f, p, u := oidcFixture(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "OIDC application") }))
	defer upstream.Close()
	site, agentToken := f.site(t, "OIDC site")
	f.agent(t, site, agentToken, portOf(upstream.URL))
	svc := f.service(t, site, "OIDC app", upstream.URL)
	res, b := f.request(t, "PATCH", "/api/users/"+u.ID, map[string]any{"serviceIDs": []string{svc.ID}}, f.cookie, "")
	if res.StatusCode != 200 {
		t.Fatalf("grant: %s", b)
	}
	path, cookie, _ := startFlow(t, f, p)
	res, b = f.request(t, "GET", path, nil, cookie, "")
	login := consoleSession(res)
	if login == nil || res.Header.Get("Location") != f.h.URL+"/" {
		t.Fatalf("callback: %d %s %s", res.StatusCode, res.Header.Get("Location"), b)
	}
	res, b = f.request(t, "GET", "/api/me", nil, login, "")
	if res.StatusCode != 200 || mustJSON[User](t, b).ID != u.ID || mustJSON[User](t, b).Role != "viewer" || strings.Contains(string(b), "passwordHash") || strings.Contains(string(b), "never-forward") {
		t.Fatalf("local mapping: %s", b)
	}
	f.s.auth.mu.Lock()
	sess := f.s.auth.sessions[digest(login.Value)]
	f.s.auth.mu.Unlock()
	access, host, _ := f.grant(t, svc, login)
	res, b = f.request(t, "GET", "/", nil, access, host)
	if res.StatusCode != 200 || string(b) != "OIDC application" {
		t.Fatal("OIDC app proxy failed", res.StatusCode, string(b))
	}
	if time.Until(sess.Expires) > 31*time.Minute {
		t.Fatal("session outlived ID token")
	}
	res, _ = f.request(t, "GET", path, nil, cookie, "")
	if consoleSession(res) != nil {
		t.Fatal("callback replay accepted")
	}
	res, _ = f.request(t, "POST", "/api/users/"+u.ID+"/password", map[string]string{"password": testPassword}, f.cookie, "")
	if res.StatusCode != 400 {
		t.Fatal("reset added local password to OIDC-only account")
	}
	res, _ = f.request(t, "PATCH", "/api/users/"+u.ID+"/identity", map[string]string{"subject": ""}, f.cookie, "")
	if res.StatusCode != 400 {
		t.Fatal("removed sole login method")
	}
	res, _ = f.request(t, "PATCH", "/api/users/"+u.ID+"/disabled", map[string]bool{"disabled": true}, f.cookie, "")
	if res.StatusCode != 200 {
		t.Fatal("disable failed")
	}
	res, _ = f.request(t, "GET", "/api/me", nil, login, "")
	if res.StatusCode != 401 {
		t.Fatal("disabled session survived")
	}
	res, _ = f.request(t, "GET", "/", nil, access, host)
	if res.StatusCode != 401 {
		t.Fatal("disabled OIDC app credential survived", res.StatusCode)
	}
	path, cookie, _ = startFlow(t, f, p)
	res, _ = f.request(t, "GET", path, nil, cookie, "")
	if consoleSession(res) != nil {
		t.Fatal("disabled OIDC login succeeded")
	}
	_, _ = f.request(t, "PATCH", "/api/users/"+u.ID+"/disabled", map[string]bool{"disabled": false}, f.cookie, "")
	path, cookie, _ = startFlow(t, f, p)
	res, _ = f.request(t, "GET", path, nil, cookie, "")
	login = consoleSession(res)
	if login == nil {
		t.Fatal("re-enable failed")
	}
	res, _ = f.request(t, "PATCH", "/api/users/"+u.ID+"/identity", map[string]string{"subject": "new-subject"}, f.cookie, "")
	if res.StatusCode != 200 {
		t.Fatal("rebind failed")
	}
	res, _ = f.request(t, "GET", "/api/me", nil, login, "")
	if res.StatusCode != 401 {
		t.Fatal("rebind did not revoke session")
	}
	path, cookie, _ = startFlow(t, f, p)
	res, _ = f.request(t, "GET", path, nil, cookie, "")
	if consoleSession(res) != nil {
		t.Fatal("old identity still worked")
	}
	// Identity uniqueness is enforced across both storage backends.
	res, _ = f.request(t, "POST", "/api/users", map[string]string{"username": "duplicate", "oidcSubject": "new-subject"}, f.cookie, "")
	if res.StatusCode != 400 {
		t.Fatal("duplicate identity accepted")
	}
	res, _ = f.request(t, "PATCH", "/api/users/"+u.ID+"/disabled", map[string]bool{"disabled": true}, login, "")
	if res.StatusCode != 401 {
		t.Fatal("revoked member mutated accounts")
	}
}
func TestOIDCRejectsInvalidProtocolAndClaims(t *testing.T) {
	f, p, _ := oidcFixture(t)
	for _, mode := range []string{"signature", "nonce", "issuer", "audience", "expired", "subject", "unknown", "case", "azp", "missing-cookie", "wrong-cookie", "state", "expired-state", "denied", "pkce-error", "missing-token"} {
		t.Run(mode, func(t *testing.T) {
			p.mu.Lock()
			p.mode = mode
			p.mu.Unlock()
			path, cookie, state := startFlow(t, f, p)
			switch mode {
			case "missing-cookie":
				cookie = nil
			case "wrong-cookie":
				cookie = &http.Cookie{Name: oidcCookie, Value: "other"}
			case "state":
				path = strings.ReplaceAll(path, state, strings.Repeat("a", 48))
			case "expired-state":
				f.s.oidc.mu.Lock()
				attempt := f.s.oidc.pending[digest(state)]
				attempt.expires = time.Now().Add(-time.Minute)
				f.s.oidc.pending[digest(state)] = attempt
				f.s.oidc.mu.Unlock()
			case "denied":
				path = "/auth/oidc/callback?state=" + state + "&error=access_denied"
			}
			p.mu.Lock()
			before := p.exchanges
			p.mu.Unlock()
			res, _ := f.request(t, "GET", path, nil, cookie, "")
			if mode == "missing-cookie" || mode == "wrong-cookie" || mode == "state" || mode == "expired-state" || mode == "denied" {
				p.mu.Lock()
				after := p.exchanges
				p.mu.Unlock()
				if before != after {
					t.Fatal("invalid callback reached token endpoint")
				}
			}
			if consoleSession(res) != nil || res.Header.Get("Location") != f.h.URL+"/?login_error=oidc" {
				t.Fatalf("accepted %s", mode)
			}
		})
	}
	f.s.oidc.mu.Lock()
	for i := 0; i < 256; i++ {
		f.s.oidc.pending[fmt.Sprint(i)] = oidcAttempt{expires: time.Now().Add(time.Minute)}
	}
	f.s.oidc.mu.Unlock()
	res, _ := f.request(t, "GET", "/auth/oidc/start", nil, nil, "")
	if res.StatusCode != 429 {
		t.Fatal("pending flows unbounded")
	}
}
func TestOIDCConcurrentDisableBeforeSessionIssuance(t *testing.T) {
	f, p, u := oidcFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	p.mu.Lock()
	p.beforeToken = func() { close(entered); <-release }
	p.mu.Unlock()
	path, cookie, _ := startFlow(t, f, p)
	done := make(chan *http.Response, 1)
	go func() { res, _ := f.request(t, "GET", path, nil, cookie, ""); done <- res }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("callback did not reach token endpoint")
	}
	res, _ := f.request(t, "PATCH", "/api/users/"+u.ID+"/disabled", map[string]bool{"disabled": true}, f.cookie, "")
	if res.StatusCode != 200 {
		t.Error("disable failed")
	}
	close(release)
	select {
	case res := <-done:
		if consoleSession(res) != nil {
			t.Fatal("stale callback minted session after disable")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("callback did not finish")
	}
}
func TestOIDCStartupFailureReleasesStore(t *testing.T) {
	t.Setenv("TUNNEL_OIDC_CLIENT_SECRET", "")
	dir := t.TempDir()
	cfg := config.Server{PublicURL: "http://localhost:8080", AccessURL: "http://apps.localhost:8080", DataDir: dir, Storage: os.Getenv("TUNNEL_TEST_STORAGE"), OIDC: config.OIDC{Issuer: "https://idp.example", ClientID: "console"}}
	if _, err := New(cfg, testPassword); err == nil || !strings.Contains(err.Error(), "TUNNEL_OIDC_CLIENT_SECRET") {
		t.Fatal("missing secret accepted", err)
	}
	cfg.OIDC = config.OIDC{}
	s, err := New(cfg, "")
	if err != nil {
		t.Fatal("startup failure retained store lock", err)
	}
	s.Close()
}

func TestDisableLocalMemberAndProtectRecovery(t *testing.T) {
	f := setup(t)
	u, login := f.member(t, nil, nil)
	res, _ := f.request(t, "PATCH", "/api/users/"+u.ID+"/disabled", map[string]bool{"disabled": true}, login, "")
	if res.StatusCode != 403 {
		t.Fatal("member disabled own account through admin API")
	}
	res, _ = f.request(t, "PATCH", "/api/users/"+u.ID+"/disabled", map[string]bool{"disabled": true}, f.cookie, "")
	if res.StatusCode != 200 {
		t.Fatal("disable failed")
	}
	res, _ = f.request(t, "GET", "/api/me", nil, login, "")
	if res.StatusCode != 401 {
		t.Fatal("disabled local session survived")
	}
	res, _ = f.request(t, "POST", "/api/login", map[string]string{"username": u.Username, "password": testPassword}, nil, "")
	if res.StatusCode != 401 {
		t.Fatal("disabled local password accepted")
	}
	admin := f.s.store.snapshot().Users[0]
	for _, operation := range []string{"identity", "disabled"} {
		var body any = map[string]bool{"disabled": true}
		if operation == "identity" {
			body = map[string]string{"subject": ""}
		}
		res, _ = f.request(t, "PATCH", "/api/users/"+admin.ID+"/"+operation, body, f.cookie, "")
		if res.StatusCode != 400 {
			t.Fatal("recovery administrator mutation accepted")
		}
	}
	res, _ = f.request(t, "PATCH", "/api/users/"+u.ID+"/disabled", map[string]bool{"disabled": false}, f.cookie, "")
	if res.StatusCode != 200 {
		t.Fatal("enable failed")
	}
	res, _ = f.request(t, "POST", "/api/login", map[string]string{"username": u.Username, "password": testPassword}, nil, "")
	if res.StatusCode != 200 {
		t.Fatal("enabled member could not sign in")
	}
	res, _ = f.request(t, "GET", "/api/me", nil, login, "")
	if res.StatusCode != 401 {
		t.Fatal("enable revived old session")
	}
}
