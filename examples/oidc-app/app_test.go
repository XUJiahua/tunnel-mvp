package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"github.com/go-jose/go-jose/v4"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type testProvider struct {
	server                 *httptest.Server
	key                    *rsa.PrivateKey
	mu                     sync.Mutex
	nonce, challenge, mode string
	exchanges              atomic.Int32
}

func newProvider(t *testing.T) *testProvider {
	t.Helper()
	key, e := rsa.GenerateKey(rand.Reader, 2048)
	if e != nil {
		t.Fatal(e)
	}
	p := &testProvider{key: key}
	p.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			json.NewEncoder(w).Encode(map[string]any{"issuer": p.server.URL, "authorization_endpoint": p.server.URL + "/authorize", "token_endpoint": p.server.URL + "/token", "jwks_uri": p.server.URL + "/keys", "id_token_signing_alg_values_supported": []string{"RS256"}})
		case "/keys":
			json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &p.key.PublicKey, KeyID: "fixture", Use: "sig", Algorithm: "RS256"}}})
		case "/token":
			p.exchanges.Add(1)
			r.ParseForm()
			user, password, ok := r.BasicAuth()
			user, _ = url.QueryUnescape(user)
			password, _ = url.QueryUnescape(password)
			if !ok || user != "demo" || password != "fixture-secret+/" {
				t.Error("bad client authentication")
			}
			p.mu.Lock()
			nonce, challenge, mode := p.nonce, p.challenge, p.mode
			p.mu.Unlock()
			hash := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
			if base64.RawURLEncoding.EncodeToString(hash[:]) != challenge || r.Form.Get("code") == "" {
				http.Error(w, "bad PKCE", 400)
				return
			}
			issuer, audience, name := p.server.URL, "demo", "operator"
			if mode == "nonce" {
				nonce = "wrong"
			}
			if mode == "issuer" {
				issuer += "/other"
			}
			if mode == "audience" {
				audience = "other-client"
			}
			if mode == "xss" {
				name = "<img src=x onerror=alert(1)>"
			}
			claims := map[string]any{"iss": issuer, "sub": "subject-a", "aud": audience, "azp": "demo", "exp": time.Now().Add(5 * time.Minute).Unix(), "iat": time.Now().Unix(), "nonce": nonce, "preferred_username": name}
			raw, _ := json.Marshal(claims)
			signer, _ := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: p.key}, (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "fixture"))
			signed, _ := signer.Sign(raw)
			token, _ := signed.CompactSerialize()
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"access_token": "never-in-the-browser", "token_type": "Bearer", "expires_in": 300, "id_token": token})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(p.server.Close)
	return p
}
func testApp(t *testing.T) (*App, *testProvider) {
	t.Helper()
	p := newProvider(t)
	app, e := NewApp(context.Background(), Config{PublicURL: "http://app.apps.localhost:18080/demo/", Issuer: p.server.URL, ClientID: "demo", ClientSecret: "fixture-secret+/"})
	if e != nil {
		t.Fatal(e)
	}
	return app, p
}
func begin(t *testing.T, a *App, p *testProvider) (string, *http.Cookie) {
	t.Helper()
	r := httptest.NewRequest("GET", a.public.String()+"login", nil)
	r.Host = "untrusted-host.invalid"
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, r)
	if w.Code != 303 {
		t.Fatal("login did not redirect")
	}
	u, e := url.Parse(w.Header().Get("Location"))
	if e != nil {
		t.Fatal(e)
	}
	query := u.Query()
	if query.Get("redirect_uri") != a.public.String()+"auth/callback" || query.Get("code_challenge_method") != "S256" {
		t.Fatal("public redirect or PKCE missing")
	}
	p.mu.Lock()
	p.nonce, p.challenge = query.Get("nonce"), query.Get("code_challenge")
	p.mu.Unlock()
	for _, c := range w.Result().Cookies() {
		if c.Name == browserCookie {
			if !c.HttpOnly || c.Path != "/demo/auth/" || c.SameSite != http.SameSiteLaxMode {
				t.Fatal("unsafe browser binding cookie")
			}
			return query.Get("state"), c
		}
	}
	t.Fatal("browser cookie missing")
	return "", nil
}
func callback(a *App, state string, cookie *http.Cookie) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", a.public.String()+"auth/callback?state="+url.QueryEscape(state)+"&code=one-use-code", nil)
	if cookie != nil {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, r)
	return w
}
func root(a *App, cookie *http.Cookie) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", a.public.String(), nil)
	if cookie != nil {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, r)
	return w
}
func TestOIDCApplicationPKCEStateSessionAndLogout(t *testing.T) {
	a, p := testApp(t)
	state, browser := begin(t, a, p)
	w := callback(a, state, browser)
	if w.Code != 303 {
		t.Fatalf("callback failed: %s", w.Body.String())
	}
	var session *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookie {
			session = c
		}
	}
	if session == nil || !session.HttpOnly || session.Path != "/demo/" || len(session.Value) != 64 {
		t.Fatal("unsafe application session")
	}
	page := root(a, session)
	if page.Code != 200 || !strings.Contains(page.Body.String(), "operator") || strings.Contains(page.Body.String(), "never-in-the-browser") || strings.Contains(page.Body.String(), "id_token") {
		t.Fatal("identity not verified or token exposed")
	}
	if again := callback(a, state, browser); again.Code != 403 || p.exchanges.Load() != 1 {
		t.Fatal("authorization state replay accepted")
	}
	csrf := regexp.MustCompile(`name="csrf" value="([a-f0-9]+)"`).FindStringSubmatch(page.Body.String())
	if len(csrf) != 2 {
		t.Fatal("logout CSRF token missing")
	}
	logout := func(value string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", a.public.String()+"logout", strings.NewReader(url.Values{"csrf": {value}}.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.AddCookie(session)
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		return w
	}
	if logout("wrong").Code != 403 || root(a, session).Code != 200 {
		t.Fatal("logout CSRF protection failed")
	}
	if logout(csrf[1]).Code != 303 || root(a, session).Code != 303 {
		t.Fatal("application session not revoked")
	}
}
func TestOIDCApplicationRejectsWrongCookieAndSignedClaims(t *testing.T) {
	for _, mode := range []string{"cookie", "nonce", "issuer", "audience"} {
		t.Run(mode, func(t *testing.T) {
			a, p := testApp(t)
			state, browser := begin(t, a, p)
			if mode == "cookie" {
				browser = &http.Cookie{Name: browserCookie, Value: "other-browser"}
			} else {
				p.mu.Lock()
				p.mode = mode
				p.mu.Unlock()
			}
			w := callback(a, state, browser)
			if w.Code != 403 {
				t.Fatal("invalid binding or signed claims accepted")
			}
			for _, c := range w.Result().Cookies() {
				if c.Name == sessionCookie && c.MaxAge >= 0 {
					t.Fatal("invalid login issued application session")
				}
			}
		})
	}
}
func TestOIDCApplicationEscapesIdentityAndExpiresSession(t *testing.T) {
	a, p := testApp(t)
	p.mu.Lock()
	p.mode = "xss"
	p.mu.Unlock()
	state, browser := begin(t, a, p)
	w := callback(a, state, browser)
	var cookie *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookie {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("callback failed")
	}
	page := root(a, cookie)
	body, _ := io.ReadAll(page.Result().Body)
	if strings.Contains(string(body), "<img") {
		t.Fatal("verified name was not HTML escaped")
	}
	a.mu.Lock()
	s := a.sessions[digest(cookie.Value)]
	s.Expires = time.Now().Add(-time.Second)
	a.sessions[digest(cookie.Value)] = s
	a.mu.Unlock()
	if root(a, cookie).Code != 303 {
		t.Fatal("expired application session accepted")
	}
}
func TestOIDCApplicationRejectsUnsafeURLs(t *testing.T) {
	for _, raw := range []string{"http://app.example.com/demo/", "https://app.example.com/demo/?", "https://app.example.com/demo/%2fprivate", "https://app.example.com/demo/../private", "https://user:password@app.example.com/"} {
		if _, e := NewApp(context.Background(), Config{PublicURL: raw, Issuer: "https://id.example.com", ClientID: "demo", ClientSecret: "secret"}); e == nil {
			t.Fatal("unsafe public URL accepted", raw)
		}
	}
}
