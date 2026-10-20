package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"html/template"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

const sessionCookie = "oidc_demo_session"
const browserCookie = "oidc_demo_browser"

type Config struct{ PublicURL, Issuer, ClientID, ClientSecret string }
type attempt struct {
	browser, nonce, verifier string
	expires                  time.Time
}
type appSession struct {
	Subject, Name, CSRF string
	Expires             time.Time
}
type App struct {
	public   *url.URL
	base     string
	oauth    oauth2.Config
	verifier *oidc.IDTokenVerifier
	client   *http.Client
	mu       sync.Mutex
	attempts map[string]attempt
	sessions map[string]appSession
}

func validateEndpoint(raw string) (*url.URL, error) {
	u, e := url.Parse(raw)
	if e != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("invalid HTTP(S) endpoint")
	}
	if u.Scheme == "http" && u.Hostname() != "localhost" && !net.ParseIP(u.Hostname()).IsLoopback() {
		return nil, fmt.Errorf("HTTPS required except loopback")
	}
	return u, nil
}
func NewApp(ctx context.Context, c Config) (*App, error) {
	public, e := url.Parse(c.PublicURL)
	if e != nil || public.Hostname() == "" || public.User != nil || public.RawQuery != "" || public.Fragment != "" || public.RawPath != "" || public.ForceQuery || (public.Scheme != "http" && public.Scheme != "https") {
		return nil, fmt.Errorf("valid public application URL required")
	}
	if public.Scheme == "http" && public.Hostname() != "localhost" && !strings.HasSuffix(public.Hostname(), ".localhost") && !net.ParseIP(public.Hostname()).IsLoopback() {
		return nil, fmt.Errorf("public application requires HTTPS except localhost development")
	}
	for _, segment := range strings.Split(public.Path, "/") {
		if segment == "." || segment == ".." || segment == "_tunnel" {
			return nil, fmt.Errorf("invalid application mount path")
		}
		for _, ch := range segment {
			if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || strings.ContainsRune("-._~", ch)) {
				return nil, fmt.Errorf("plain application mount path required")
			}
		}
	}
	if public.Path == "" {
		public.Path = "/"
	}
	if !strings.HasSuffix(public.Path, "/") {
		public.Path += "/"
	}
	if _, e = validateEndpoint(c.Issuer); e != nil {
		return nil, e
	}
	if strings.TrimSpace(c.ClientID) == "" || c.ClientSecret == "" {
		return nil, fmt.Errorf("client ID and OIDC_APP_CLIENT_SECRET required")
	}
	client := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	base := oidc.ClientContext(ctx, client)
	discovery, cancel := context.WithTimeout(base, 10*time.Second)
	defer cancel()
	provider, e := oidc.NewProvider(discovery, c.Issuer)
	if e != nil {
		return nil, fmt.Errorf("OIDC discovery failed")
	}
	endpoint := provider.Endpoint()
	for _, raw := range []string{endpoint.AuthURL, endpoint.TokenURL} {
		if _, e = validateEndpoint(raw); e != nil {
			return nil, e
		}
	}
	var metadata struct {
		JWKS string `json:"jwks_uri"`
	}
	if e = provider.Claims(&metadata); e != nil {
		return nil, e
	}
	if _, e = validateEndpoint(metadata.JWKS); e != nil {
		return nil, e
	}
	endpoint.AuthStyle = oauth2.AuthStyleInHeader
	return &App{public: public, base: public.Path, oauth: oauth2.Config{ClientID: c.ClientID, ClientSecret: c.ClientSecret, Endpoint: endpoint, RedirectURL: public.String() + "auth/callback", Scopes: []string{oidc.ScopeOpenID, "profile"}}, verifier: provider.VerifierContext(base, &oidc.Config{ClientID: c.ClientID}), client: client, attempts: map[string]attempt{}, sessions: map[string]appSession{}}, nil
}
func token() (string, error) {
	b := make([]byte, 32)
	if _, e := rand.Read(b); e != nil {
		return "", e
	}
	return hex.EncodeToString(b), nil
}
func digest(raw string) string { h := sha256.Sum256([]byte(raw)); return hex.EncodeToString(h[:]) }
func (a *App) cookie(w http.ResponseWriter, name, value, path string, maxAge int) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: path, MaxAge: maxAge, HttpOnly: true, Secure: a.public.Scheme == "https", SameSite: http.SameSiteLaxMode})
}
func (a *App) clean(now time.Time) {
	for key, v := range a.attempts {
		if !now.Before(v.expires) {
			delete(a.attempts, key)
		}
	}
	for key, v := range a.sessions {
		if !now.Before(v.Expires) {
			delete(a.sessions, key)
		}
	}
}
func (a *App) login(w http.ResponseWriter, r *http.Request) {
	state, e := token()
	if e != nil {
		http.Error(w, "entropy unavailable", 503)
		return
	}
	browser, e := token()
	if e != nil {
		http.Error(w, "entropy unavailable", 503)
		return
	}
	nonce, e := token()
	if e != nil {
		http.Error(w, "entropy unavailable", 503)
		return
	}
	v := attempt{browser: digest(browser), nonce: nonce, verifier: oauth2.GenerateVerifier(), expires: time.Now().Add(5 * time.Minute)}
	a.mu.Lock()
	a.clean(time.Now())
	if len(a.attempts) >= 256 {
		a.mu.Unlock()
		http.Error(w, "too many login attempts", 429)
		return
	}
	a.attempts[digest(state)] = v
	a.mu.Unlock()
	a.cookie(w, browserCookie, browser, a.base+"auth/", 300)
	http.Redirect(w, r, a.oauth.AuthCodeURL(state, oidc.Nonce(nonce), oauth2.S256ChallengeOption(v.verifier)), http.StatusSeeOther)
}
func (a *App) callback(w http.ResponseWriter, r *http.Request) {
	fail := func() { http.Error(w, "Application sign in failed", http.StatusForbidden) }
	a.cookie(w, browserCookie, "", a.base+"auth/", -1)
	cookie, e := r.Cookie(browserCookie)
	state := r.URL.Query().Get("state")
	a.mu.Lock()
	a.clean(time.Now())
	v, ok := a.attempts[digest(state)]
	delete(a.attempts, digest(state))
	a.mu.Unlock()
	if e != nil || !ok || state == "" || subtle.ConstantTimeCompare([]byte(digest(cookie.Value)), []byte(v.browser)) != 1 || r.URL.Query().Get("error") != "" {
		fail()
		return
	}
	ctx, cancel := context.WithTimeout(oidc.ClientContext(r.Context(), a.client), 10*time.Second)
	defer cancel()
	tokens, e := a.oauth.Exchange(ctx, r.URL.Query().Get("code"), oauth2.VerifierOption(v.verifier))
	if e != nil {
		fail()
		return
	}
	raw, ok := tokens.Extra("id_token").(string)
	if !ok {
		fail()
		return
	}
	id, e := a.verifier.Verify(ctx, raw)
	if e != nil || id.Subject == "" {
		fail()
		return
	}
	var claims struct {
		AuthorizedParty string `json:"azp"`
		Name            string `json:"preferred_username"`
	}
	if e = id.Claims(&claims); e != nil || id.Nonce != v.nonce || (len(id.Audience) > 1 && claims.AuthorizedParty == "") || (claims.AuthorizedParty != "" && claims.AuthorizedParty != a.oauth.ClientID) {
		fail()
		return
	}
	name := claims.Name
	if name == "" {
		name = id.Subject
	}
	if len(name) > 255 {
		name = name[:255]
	}
	sessionToken, e := token()
	if e != nil {
		fail()
		return
	}
	csrf, e := token()
	if e != nil {
		fail()
		return
	}
	expires := time.Now().Add(30 * time.Minute)
	if id.Expiry.Before(expires) {
		expires = id.Expiry
	}
	if !time.Now().Before(expires) {
		fail()
		return
	}
	a.mu.Lock()
	a.clean(time.Now())
	if old, e := r.Cookie(sessionCookie); e == nil {
		delete(a.sessions, digest(old.Value))
	}
	if len(a.sessions) >= 1024 {
		a.mu.Unlock()
		http.Error(w, "application session limit reached", 503)
		return
	}
	a.sessions[digest(sessionToken)] = appSession{Subject: id.Subject, Name: name, CSRF: csrf, Expires: expires}
	a.mu.Unlock()
	a.cookie(w, sessionCookie, sessionToken, a.base, int(time.Until(expires).Seconds()))
	http.Redirect(w, r, a.public.String(), http.StatusSeeOther)
}
func (a *App) session(r *http.Request) (appSession, bool) {
	cookie, e := r.Cookie(sessionCookie)
	if e != nil {
		return appSession{}, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.clean(time.Now())
	session, ok := a.sessions[digest(cookie.Value)]
	return session, ok
}

var pageTemplate = template.Must(template.New("page").Parse(`<!doctype html><html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>OIDC demo application</title><style>body{font:17px system-ui;max-width:680px;margin:10vh auto;padding:24px;color:#17392e;background:#f4f7f4}main{background:white;border:1px solid #dce5de;border-radius:18px;padding:32px}h1{font-size:30px}button,a{font:inherit}button{padding:10px 16px;background:#214f3b;color:white;border:0;border-radius:8px;cursor:pointer}small{color:#607569}</style><main><h1>OIDC demo application</h1>{{if .SignedIn}}<p>Signed in as <strong data-testid="application-user">{{.Name}}</strong></p><p>The application verified your identity with its configured identity provider.</p><form method="post" action="{{.Base}}logout"><input type="hidden" name="csrf" value="{{.CSRF}}"><button>Sign out of this application</button></form>{{else}}<p>You signed out of this application.</p><a href="{{.Base}}login">Sign in again</a><p><small>Your identity provider session is managed separately.</small></p>{{end}}</main></html>`))

func (a *App) logout(w http.ResponseWriter, r *http.Request) {
	session, ok := a.session(r)
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if !ok || r.ParseForm() != nil || subtle.ConstantTimeCompare([]byte(r.PostForm.Get("csrf")), []byte(session.CSRF)) != 1 {
		http.Error(w, "Invalid application logout request", 403)
		return
	}
	cookie, _ := r.Cookie(sessionCookie)
	a.mu.Lock()
	delete(a.sessions, digest(cookie.Value))
	a.mu.Unlock()
	a.cookie(w, sessionCookie, "", a.base, -1)
	http.Redirect(w, r, a.public.String()+"signed-out", http.StatusSeeOther)
}
func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+a.base+"login", a.login)
	mux.HandleFunc("GET "+a.base+"auth/callback", a.callback)
	mux.HandleFunc("POST "+a.base+"logout", a.logout)
	mux.HandleFunc("GET "+a.base+"healthz", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") })
	mux.HandleFunc("GET "+a.base+"signed-out", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		pageTemplate.Execute(w, map[string]any{"Base": a.base})
	})
	mux.HandleFunc("GET "+a.base, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != a.base {
			http.NotFound(w, r)
			return
		}
		session, ok := a.session(r)
		if !ok {
			a.cookie(w, sessionCookie, "", a.base, -1)
			http.Redirect(w, r, a.public.String()+"login", http.StatusSeeOther)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		pageTemplate.Execute(w, map[string]any{"Base": a.base, "SignedIn": true, "Name": session.Name, "CSRF": session.CSRF})
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
		mux.ServeHTTP(w, r)
	})
}
