package control

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/XUJiahua/tunnel-mvp/internal/state"
	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

const oidcCookie = "tunnel_oidc"

type oidcAttempt struct {
	browser, nonce, verifier string
	expires, started         time.Time
}
type oidcLogin struct {
	config       oauth2.Config
	verifier     *oidc.IDTokenVerifier
	client       *http.Client
	endSession   string
	mu           sync.Mutex
	pending      map[string]oidcAttempt
	logoutReplay map[string]time.Time
	logoutCutoff map[string]time.Time
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
		JWKS       string `json:"jwks_uri"`
		EndSession string `json:"end_session_endpoint"`
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
	if metadata.EndSession != "" {
		if err := state.ValidateIdentity(state.Identity{Issuer: metadata.EndSession, Subject: "endpoint"}); err != nil {
			return fmt.Errorf("OIDC logout endpoint requires HTTPS (HTTP allowed only on loopback)")
		}
	}
	// Confidential clients use client_secret_basic; tokens never reach the browser.
	endpoint.AuthStyle = oauth2.AuthStyleInHeader
	s.oidc = &oidcLogin{config: oauth2.Config{ClientID: s.cfg.OIDC.ClientID, ClientSecret: secret, Endpoint: endpoint, RedirectURL: s.cfg.PublicURL + "/auth/oidc/callback", Scopes: []string{oidc.ScopeOpenID}}, verifier: provider.VerifierContext(base, &oidc.Config{ClientID: s.cfg.OIDC.ClientID}), client: client, endSession: metadata.EndSession, pending: map[string]oidcAttempt{}, logoutReplay: map[string]time.Time{}, logoutCutoff: map[string]time.Time{}}
	return s.configureKeycloakSync()
}
func (s *Server) authOptions(w http.ResponseWriter, r *http.Request) {
	jsonOut(w, 200, map[string]any{"oidc": s.oidc != nil, "issuer": s.cfg.OIDC.Issuer, "oidcLogout": s.oidc != nil && s.oidc.endSession != ""})
}
func (s *Server) beginOIDC(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if s.oidc == nil {
		http.NotFound(w, r)
		return
	}
	now := time.Now()
	token, browser := randomID(), randomID()
	attempt := oidcAttempt{browser: digest(browser), nonce: randomID(), verifier: oauth2.GenerateVerifier(), expires: now.Add(5 * time.Minute), started: now}
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
	options := []oauth2.AuthCodeOption{oidc.Nonce(attempt.nonce), oauth2.S256ChallengeOption(attempt.verifier)}
	if len(s.cfg.OIDC.RequiredACR) > 0 {
		options = append(options, oauth2.SetAuthURLParam("acr_values", strings.Join(s.cfg.OIDC.RequiredACR, " ")))
	}
	http.Redirect(w, r, s.oidc.config.AuthCodeURL(token, options...), http.StatusFound)
}
func (s *Server) oidcFailed(w http.ResponseWriter, r *http.Request, reason ...string) {
	stage := "protocol"
	if len(reason) > 0 {
		stage = reason[0]
	}
	s.audit("oidc_login_failed", "", "", "", map[string]any{"stage": stage})
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
		s.oidcFailed(w, r, "code_exchange")
		return
	}
	raw, ok := token.Extra("id_token").(string)
	if !ok {
		s.oidcFailed(w, r)
		return
	}
	id, err := s.oidc.verifier.Verify(ctx, raw)
	if err != nil || !constantEqual(id.Nonce, attempt.nonce) || state.ValidateIdentity(state.Identity{Issuer: id.Issuer, Subject: id.Subject}) != nil {
		s.oidcFailed(w, r, "id_token")
		return
	}
	var claims struct {
		AuthorizedParty string   `json:"azp"`
		SID             string   `json:"sid"`
		ACR             string   `json:"acr"`
		AMR             []string `json:"amr"`
	}
	if err := id.Claims(&claims); err != nil || (len(id.Audience) > 1 && claims.AuthorizedParty == "") || (claims.AuthorizedParty != "" && claims.AuthorizedParty != s.cfg.OIDC.ClientID) {
		s.oidcFailed(w, r, "authorized_party")
		return
	}
	if !s.acceptAuthenticationContext(claims.ACR, claims.AMR) {
		s.oidcFailed(w, r, "authentication_context")
		return
	}
	if s.identitySync != nil {
		checked := false
		for _, u := range s.store.snapshot().Users {
			if !u.Disabled && u.Identity != nil && u.Identity.Issuer == id.Issuer && u.Identity.Subject == id.Subject {
				checkCtx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
				result := s.identitySync.check(checkCtx, u)
				cancel()
				checked = result.err == nil && result.enabled
				break
			}
		}
		if !checked {
			s.oidcFailed(w, r, "identity_sync")
			return
		}
	}
	// The binding and disabled flag are read after external IO under the same lock
	// used by mutation/revocation. A concurrent disable/rebind cannot mint a session.
	unlock := s.lockMutation(r)
	defer unlock()
	s.oidc.mu.Lock()
	blocked := false
	for _, key := range []string{"sid:" + claims.SID, "sub:" + id.Subject} {
		if cutoff, ok := s.oidc.logoutCutoff[key]; ok && !attempt.started.After(cutoff) {
			blocked = true
		}
	}
	s.oidc.mu.Unlock()
	if blocked {
		s.oidcFailed(w, r)
		return
	}
	for _, u := range s.store.snapshot().Users {
		if !u.Disabled && u.Identity != nil && u.Identity.Issuer == id.Issuer && u.Identity.Subject == id.Subject {
			if !s.identitySessionAllowed(session{UserID: u.ID, Method: "oidc", Issuer: id.Issuer, Subject: id.Subject}) {
				break
			}
			expires := time.Now().Add(time.Duration(s.cfg.OIDC.SessionMinutes) * time.Minute)
			if id.Expiry.Before(expires) {
				expires = id.Expiry
			}
			if !time.Now().Before(expires) {
				break
			}
			s.issueSession(w, u, expires, sessionMetadata(r, "oidc", id.Issuer, id.Subject, claims.SID))
			s.audit("oidc_login", u.Username, "", u.ID)
			http.Redirect(w, r, s.cfg.PublicURL+"/", http.StatusSeeOther)
			return
		}
	}
	s.oidcFailed(w, r, "identity_binding")
}

func (s *Server) acceptAuthenticationContext(acr string, amr []string) bool {
	if len(s.cfg.OIDC.RequiredACR) > 0 {
		found := false
		for _, allowed := range s.cfg.OIDC.RequiredACR {
			if acr == allowed {
				found = true
			}
		}
		if !found {
			return false
		}
	}
	for _, required := range s.cfg.OIDC.RequiredAMR {
		found := false
		for _, method := range amr {
			if method == required {
				found = true
			}
		}
		if !found {
			return false
		}
	}
	return true
}
