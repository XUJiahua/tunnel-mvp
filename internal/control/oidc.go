package control

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/XUJiahua/tunnel-mvp/internal/state"
	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

const oidcCookie = "tunnel_oidc"

type oidcAttempt struct {
	browser, nonce, verifier string
	expires                  time.Time
}
type oidcLogin struct {
	config   oauth2.Config
	verifier *oidc.IDTokenVerifier
	client   *http.Client
	mu       sync.Mutex
	pending  map[string]oidcAttempt
}

func (s *Server) configureOIDC() error {
	if s.cfg.OIDC.Issuer == "" {
		return nil
	}
	secret := os.Getenv("TUNNEL_OIDC_CLIENT_SECRET")
	if secret == "" {
		return fmt.Errorf("OIDC requires TUNNEL_OIDC_CLIENT_SECRET")
	}
	client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(r *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return fmt.Errorf("too many OIDC redirects")
		}
		return state.ValidateIdentity(state.Identity{Issuer: r.URL.String(), Subject: "endpoint"})
	}}
	// Provider/JWKS lifecycle must not inherit the short discovery timeout.
	base := oidc.ClientContext(context.Background(), client)
	ctx, cancel := context.WithTimeout(base, 10*time.Second)
	defer cancel()
	provider, err := oidc.NewProvider(ctx, s.cfg.OIDC.Issuer)
	if err != nil {
		return fmt.Errorf("OIDC discovery failed: %w", err)
	}
	var metadata struct {
		JWKS string `json:"jwks_uri"`
	}
	if err = provider.Claims(&metadata); err != nil {
		return fmt.Errorf("invalid OIDC discovery document")
	}
	endpoint := provider.Endpoint()
	for _, raw := range []string{endpoint.AuthURL, endpoint.TokenURL, metadata.JWKS} {
		if err := state.ValidateIdentity(state.Identity{Issuer: raw, Subject: "endpoint"}); err != nil {
			return fmt.Errorf("OIDC endpoints require HTTPS (HTTP allowed only on loopback)")
		}
	}
	// Confidential clients use client_secret_basic; tokens never reach the browser.
	endpoint.AuthStyle = oauth2.AuthStyleInHeader
	s.oidc = &oidcLogin{config: oauth2.Config{ClientID: s.cfg.OIDC.ClientID, ClientSecret: secret, Endpoint: endpoint, RedirectURL: s.cfg.PublicURL + "/auth/oidc/callback", Scopes: []string{oidc.ScopeOpenID}}, verifier: provider.VerifierContext(base, &oidc.Config{ClientID: s.cfg.OIDC.ClientID}), client: client, pending: map[string]oidcAttempt{}}
	return nil
}
func (s *Server) authOptions(w http.ResponseWriter, r *http.Request) {
	jsonOut(w, 200, map[string]any{"oidc": s.oidc != nil, "issuer": s.cfg.OIDC.Issuer})
}
func (s *Server) beginOIDC(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if s.oidc == nil {
		http.NotFound(w, r)
		return
	}
	now := time.Now()
	token, browser := randomID(), randomID()
	attempt := oidcAttempt{browser: digest(browser), nonce: randomID(), verifier: oauth2.GenerateVerifier(), expires: now.Add(5 * time.Minute)}
	s.oidc.mu.Lock()
	for key, value := range s.oidc.pending {
		if !now.Before(value.expires) {
			delete(s.oidc.pending, key)
		}
	}
	if len(s.oidc.pending) >= 256 {
		s.oidc.mu.Unlock()
		fail(w, 429, "too many pending logins; retry in five minutes")
		return
	}
	s.oidc.pending[digest(token)] = attempt
	s.oidc.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: oidcCookie, Value: browser, Path: "/auth/oidc", HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteLaxMode, MaxAge: 300})
	http.Redirect(w, r, s.oidc.config.AuthCodeURL(token, oidc.Nonce(attempt.nonce), oauth2.S256ChallengeOption(attempt.verifier)), http.StatusFound)
}
func (s *Server) oidcFailed(w http.ResponseWriter, r *http.Request) {
	s.audit("oidc_login_failed", "", "", "")
	http.Redirect(w, r, s.cfg.PublicURL+"/?login_error=oidc", http.StatusSeeOther)
}
func (s *Server) callbackOIDC(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if s.oidc == nil {
		http.NotFound(w, r)
		return
	}
	cookie, err := r.Cookie(oidcCookie)
	if err != nil || len(r.URL.Query().Get("state")) != 48 {
		s.oidcFailed(w, r)
		return
	}
	key := digest(r.URL.Query().Get("state"))
	s.oidc.mu.Lock()
	attempt, ok := s.oidc.pending[key]
	if ok && constantEqual(attempt.browser, digest(cookie.Value)) {
		delete(s.oidc.pending, key)
	} else {
		ok = false
	}
	s.oidc.mu.Unlock()
	if !ok || !time.Now().Before(attempt.expires) {
		s.oidcFailed(w, r)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: oidcCookie, Path: "/auth/oidc", HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteLaxMode, MaxAge: -1})
	if r.URL.Query().Get("error") != "" || r.URL.Query().Get("code") == "" || len(r.URL.Query().Get("code")) > 4096 {
		s.oidcFailed(w, r)
		return
	}
	ctx, cancel := context.WithTimeout(oidc.ClientContext(r.Context(), s.oidc.client), 10*time.Second)
	defer cancel()
	token, err := s.oidc.config.Exchange(ctx, r.URL.Query().Get("code"), oauth2.VerifierOption(attempt.verifier))
	if err != nil {
		s.oidcFailed(w, r)
		return
	}
	raw, ok := token.Extra("id_token").(string)
	if !ok {
		s.oidcFailed(w, r)
		return
	}
	id, err := s.oidc.verifier.Verify(ctx, raw)
	if err != nil || !constantEqual(id.Nonce, attempt.nonce) || state.ValidateIdentity(state.Identity{Issuer: id.Issuer, Subject: id.Subject}) != nil {
		s.oidcFailed(w, r)
		return
	}
	var claims struct {
		AuthorizedParty string `json:"azp"`
	}
	if err := id.Claims(&claims); err != nil || (len(id.Audience) > 1 && claims.AuthorizedParty == "") || (claims.AuthorizedParty != "" && claims.AuthorizedParty != s.cfg.OIDC.ClientID) {
		s.oidcFailed(w, r)
		return
	}
	// The binding and disabled flag are read after external IO under the same lock
	// used by mutation/revocation. A concurrent disable/rebind cannot mint a session.
	s.accessMu.Lock()
	defer s.accessMu.Unlock()
	for _, u := range s.store.snapshot().Users {
		if !u.Disabled && u.Identity != nil && u.Identity.Issuer == id.Issuer && u.Identity.Subject == id.Subject {
			expires := time.Now().Add(time.Duration(s.cfg.OIDC.SessionMinutes) * time.Minute)
			if id.Expiry.Before(expires) {
				expires = id.Expiry
			}
			if !time.Now().Before(expires) {
				break
			}
			s.issueSession(w, u, expires)
			s.audit("oidc_login", u.Username, "", u.ID)
			http.Redirect(w, r, s.cfg.PublicURL+"/", http.StatusSeeOther)
			return
		}
	}
	s.oidcFailed(w, r)
}
