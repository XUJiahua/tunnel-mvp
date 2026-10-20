package control

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/XUJiahua/tunnel-mvp/internal/state"
)

type syncRecord struct {
	identity state.Identity
	enabled  bool
	verified time.Time
}
type syncResult struct {
	enabled bool
	err     error
}
type syncFlight struct {
	done   chan struct{}
	result syncResult
}
type keycloakSync struct {
	server                                       *Server
	issuer, clientID, secret, tokenURL, adminURL string
	interval, maxStale                           time.Duration
	client                                       *http.Client
	slots                                        chan struct{}
	mu                                           sync.Mutex
	records                                      map[string]syncRecord
	flights                                      map[string]*syncFlight
	tokenMu                                      sync.Mutex
	token                                        string
	tokenExpiry                                  time.Time
}

func (s *Server) configureKeycloakSync() error {
	cfg := s.cfg.OIDC.KeycloakSync
	if !cfg.Enabled {
		return nil
	}
	secret := os.Getenv("TUNNEL_KEYCLOAK_SYNC_CLIENT_SECRET")
	if secret == "" {
		return fmt.Errorf("Keycloak sync requires TUNNEL_KEYCLOAK_SYNC_CLIENT_SECRET")
	}
	issuer, e := url.Parse(s.cfg.OIDC.Issuer)
	if e != nil {
		return e
	}
	path := strings.TrimSuffix(issuer.Path, "/")
	i := strings.LastIndex(path, "/realms/")
	if i < 0 || issuer.RawPath != "" {
		return fmt.Errorf("Keycloak sync requires a plain /realms/{realm} issuer")
	}
	realm := strings.TrimPrefix(path[i:], "/realms/")
	if realm == "" || len(realm) > 100 || strings.ContainsAny(realm, "/\\%?#") || realm == "." || realm == ".." {
		return fmt.Errorf("unsupported Keycloak realm path")
	}
	for _, segment := range strings.Split(path, "/") {
		if segment == "." || segment == ".." {
			return fmt.Errorf("ambiguous Keycloak issuer path")
		}
	}
	token, e := url.Parse(s.oidc.config.Endpoint.TokenURL)
	if e != nil || token.Scheme != issuer.Scheme || token.Host != issuer.Host || token.Path != path+"/protocol/openid-connect/token" || token.RawQuery != "" || token.User != nil {
		return fmt.Errorf("Keycloak token endpoint must match the configured realm and origin")
	}
	admin := *issuer
	admin.Path = path[:i] + "/admin/realms/" + realm
	admin.RawPath = ""
	s.identitySync = &keycloakSync{server: s, issuer: s.cfg.OIDC.Issuer, clientID: cfg.ClientID, secret: secret, tokenURL: token.String(), adminURL: admin.String(), interval: time.Duration(cfg.PollSeconds) * time.Second, maxStale: time.Duration(cfg.MaxStaleSeconds) * time.Second, client: &http.Client{Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, slots: make(chan struct{}, 4), records: map[string]syncRecord{}, flights: map[string]*syncFlight{}}
	return nil
}
func decodeSyncJSON(body io.Reader, target any) error {
	raw, e := io.ReadAll(io.LimitReader(body, 65537))
	if e != nil || len(raw) > 65536 {
		return fmt.Errorf("identity sync response cannot be read or exceeds limit")
	}
	d := json.NewDecoder(strings.NewReader(string(raw)))
	if e = d.Decode(target); e != nil {
		return fmt.Errorf("invalid identity sync JSON")
	}
	if d.Decode(new(any)) != io.EOF {
		return fmt.Errorf("trailing identity sync JSON")
	}
	return nil
}
func (k *keycloakSync) accessToken(ctx context.Context) (string, error) {
	k.tokenMu.Lock()
	defer k.tokenMu.Unlock()
	if k.token != "" && time.Now().Add(10*time.Second).Before(k.tokenExpiry) {
		return k.token, nil
	}
	form := url.Values{"grant_type": {"client_credentials"}}
	req, e := http.NewRequestWithContext(ctx, "POST", k.tokenURL, strings.NewReader(form.Encode()))
	if e != nil {
		return "", e
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(url.QueryEscape(k.clientID), url.QueryEscape(k.secret))
	res, e := k.client.Do(req)
	if e != nil {
		return "", fmt.Errorf("Keycloak service token request failed")
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return "", fmt.Errorf("Keycloak service token returned HTTP %d", res.StatusCode)
	}
	var token struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if e = decodeSyncJSON(res.Body, &token); e != nil {
		return "", e
	}
	if token.AccessToken == "" || len(token.AccessToken) > 32768 || token.ExpiresIn < 1 || token.ExpiresIn > 86400 {
		return "", fmt.Errorf("invalid Keycloak service token response")
	}
	k.token = token.AccessToken
	k.tokenExpiry = time.Now().Add(time.Duration(token.ExpiresIn) * time.Second)
	return k.token, nil
}
func (k *keycloakSync) fetch(ctx context.Context, u User) syncResult {
	if u.Identity == nil || u.Identity.Issuer != k.issuer || u.Identity.Subject == "." || u.Identity.Subject == ".." {
		return syncResult{err: fmt.Errorf("identity does not match Keycloak status mapping")}
	}
	token, e := k.accessToken(ctx)
	if e != nil {
		return syncResult{err: e}
	}
	req, e := http.NewRequestWithContext(ctx, "GET", k.adminURL+"/users/"+url.PathEscape(u.Identity.Subject), nil)
	if e != nil {
		return syncResult{err: e}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	res, e := k.client.Do(req)
	if e != nil {
		return syncResult{err: fmt.Errorf("Keycloak user status request failed")}
	}
	defer res.Body.Close()
	if res.StatusCode == 404 {
		return syncResult{enabled: false}
	}
	if res.StatusCode == 401 {
		k.tokenMu.Lock()
		k.token = ""
		k.tokenExpiry = time.Time{}
		k.tokenMu.Unlock()
	}
	if res.StatusCode != 200 {
		return syncResult{err: fmt.Errorf("Keycloak user status returned HTTP %d", res.StatusCode)}
	}
	var user struct {
		ID      string `json:"id"`
		Enabled *bool  `json:"enabled"`
	}
	if e = decodeSyncJSON(res.Body, &user); e != nil {
		return syncResult{err: e}
	}
	if user.ID != u.Identity.Subject || user.Enabled == nil {
		return syncResult{err: fmt.Errorf("Keycloak user status mapping or enabled flag missing")}
	}
	return syncResult{enabled: *user.Enabled}
}
func (k *keycloakSync) check(ctx context.Context, u User) syncResult {
	if u.Identity == nil {
		return syncResult{err: fmt.Errorf("OIDC identity required")}
	}
	key := u.ID + "\x00" + u.Identity.Issuer + "\x00" + u.Identity.Subject
	k.mu.Lock()
	if flight := k.flights[key]; flight != nil {
		k.mu.Unlock()
		select {
		case <-ctx.Done():
			return syncResult{err: ctx.Err()}
		case <-flight.done:
			return flight.result
		}
	}
	flight := &syncFlight{done: make(chan struct{})}
	k.flights[key] = flight
	k.mu.Unlock()
	defer func() { k.mu.Lock(); delete(k.flights, key); close(flight.done); k.mu.Unlock() }()
	select {
	case k.slots <- struct{}{}:
		defer func() { <-k.slots }()
	case <-ctx.Done():
		flight.result = syncResult{err: ctx.Err()}
		return flight.result
	}
	if k.server.auth.shared != nil {
		result := k.checkShared(ctx, u)
		flight.result = result
		return result
	}
	result := k.fetch(ctx, u)
	verified := time.Now()
	k.server.stats.identitySyncChecks.Add(1)
	if result.err != nil {
		k.server.stats.identitySyncErrors.Add(1)
		slog.Warn("Keycloak identity status check failed", "user_id", u.ID, "error", result.err)
		flight.result = result
		return result
	}
	s := k.server
	s.accessMu.Lock()
	defer s.accessMu.Unlock()
	if err := ctx.Err(); err != nil {
		result.err = err
		flight.result = result
		return result
	}
	fresh, found := s.store.user(u.ID)
	if !found || fresh.Disabled || fresh.Identity == nil || *fresh.Identity != *u.Identity {
		result.err = fmt.Errorf("identity binding changed during status check")
		flight.result = result
		return result
	}
	k.mu.Lock()
	k.records[u.ID] = syncRecord{identity: *u.Identity, enabled: result.enabled, verified: verified}
	k.mu.Unlock()
	if !result.enabled {
		s.revokeIdentitySessions(u.ID, "disabled_or_deleted")
	}
	flight.result = result
	return result
}
func (s *Server) identitySessionAllowed(sess session) bool {
	if s.identitySync == nil || sess.Method != "oidc" {
		return true
	}
	if s.auth.shared != nil {
		status, found, now, err := s.auth.shared.AuthIdentityRead(context.Background(), sess.UserID)
		return err == nil && found && sess.IdentityEpoch != "" && state.AuthIdentityAllowed(status, sess, now)
	}
	k := s.identitySync
	k.mu.Lock()
	record, ok := k.records[sess.UserID]
	k.mu.Unlock()
	return ok && record.enabled && record.identity.Issuer == sess.Issuer && record.identity.Subject == sess.Subject && time.Since(record.verified) <= k.maxStale
}

// Caller holds accessMu. Local password sessions remain independent recovery credentials.
func (s *Server) revokeIdentitySessions(userID, reason string) {
	snapshot, err := s.authSnapshot(context.Background(), false)
	if err != nil {
		return
	}
	keys := map[string]bool{}
	for key, sess := range snapshot.Data.Sessions {
		if sess.UserID == userID && sess.Method == "oidc" {
			keys[key] = true
		}
	}
	s.revokeIdentityKeys(keys, reason)
}
func (s *Server) revokeIdentityKeys(keys map[string]bool, reason string) {
	if len(keys) == 0 {
		return
	}
	actual := map[string]bool{}
	counts := map[string]int{}
	err := s.authUpdate(context.Background(), false, func(a *state.AuthData, _ State, _ time.Time) error {
		for key := range keys {
			if sess, ok := a.Sessions[key]; ok && sess.Method == "oidc" {
				actual[key] = true
				counts[sess.UserID]++
				delete(a.Sessions, key)
			}
		}
		return nil
	})
	s.cancelRequests(func(r liveRequest) bool { return keys[r.sessionKey] })
	s.pruneSessionPools()
	if err != nil {
		return
	}
	s.stats.identitySyncRevocations.Add(uint64(len(actual)))
	for user, count := range counts {
		u, _ := s.store.user(user)
		s.audit("identity_sync_session_revoked", u.Username, "", user, map[string]any{"reason": reason, "sessions": count})
	}
}
func (k *keycloakSync) expire() {
	s := k.server
	keys := map[string]bool{}
	snapshot, err := s.authSnapshot(context.Background(), false)
	if err != nil {
		return
	}
	sessions := snapshot.Data.Sessions
	for key, sess := range sessions {
		if sess.Method == "oidc" && !s.identitySessionAllowed(sess) {
			keys[key] = true
		}
	}
	if len(keys) == 0 {
		return
	}
	s.cancelRequests(func(r liveRequest) bool { return keys[r.sessionKey] })
	s.accessMu.Lock()
	defer s.accessMu.Unlock()
	s.revokeIdentityKeys(keys, "verification_expired")
}
func (k *keycloakSync) poll(ctx context.Context) {
	s := k.server
	active := map[string]bool{}
	snapshot, err := s.authSnapshot(ctx, false)
	if err != nil {
		return
	}
	for _, sess := range snapshot.Data.Sessions {
		if sess.Method == "oidc" && sess.Issuer == k.issuer && snapshot.Now.Before(sess.Expires) {
			active[sess.UserID] = true
		}
	}
	jobs := make(chan User)
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for user := range jobs {
				k.check(ctx, user)
			}
		}()
	}
	for _, u := range s.store.snapshot().Users {
		if active[u.ID] && !u.Disabled && u.Identity != nil && u.Identity.Issuer == k.issuer {
			select {
			case jobs <- u:
			case <-ctx.Done():
				close(jobs)
				wg.Wait()
				return
			}
		}
	}
	close(jobs)
	wg.Wait()
	k.mu.Lock()
	for id, record := range k.records {
		if !active[id] && time.Since(record.verified) > 2*k.maxStale {
			delete(k.records, id)
		}
	}
	k.mu.Unlock()
}
func (s *Server) startIdentitySync(ctx context.Context) {
	k := s.identitySync
	if k == nil {
		return
	}
	s.backgroundWG.Add(2)
	go func() {
		defer s.backgroundWG.Done()
		for {
			k.poll(ctx)
			timer := time.NewTimer(k.interval)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}()
	go func() {
		defer s.backgroundWG.Done()
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				k.expire()
			}
		}
	}()
}
