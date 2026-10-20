package state

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"
)

var ErrAuthUnavailable = errors.New("shared authentication unavailable")
var ErrAuthCapacity = errors.New("shared authentication capacity reached; retry after expiry or revoke unused sessions")
var ErrAuthChanged = errors.New("authentication or permissions changed; sign in and retry")
var ErrAuthPolicy = errors.New("shared authentication policy mismatch; stop all Servers before changing cluster authentication settings")

// Runtime credentials are deliberately excluded from configuration backups.
// Map keys for bearer credentials are SHA-256 digests, never browser tokens.
type AuthSession struct {
	ID                          string
	CreatedAt, LastSeen         time.Time
	UserAgent, RemoteIP, Method string
	Issuer, Subject, SID        string
	UserID                      string
	Expires                     time.Time
}
type AuthGrant struct {
	SessionKey, ServiceID string
	Expires               time.Time
}
type AuthLimit struct {
	Count int
	Until time.Time
}
type AuthOIDCAttempt struct {
	Browser, Nonce, Verifier      string
	Issuer, ClientID, RedirectURI string
	Expires, Started              time.Time
}
type AuthData struct {
	Sessions        map[string]AuthSession
	Tickets, Access map[string]AuthGrant
	Limits          map[string]AuthLimit
	Pending         map[string]AuthOIDCAttempt
	Replay, Cutoff  map[string]time.Time
}

func NewAuthData() AuthData {
	return AuthData{map[string]AuthSession{}, map[string]AuthGrant{}, map[string]AuthGrant{}, map[string]AuthLimit{}, map[string]AuthOIDCAttempt{}, map[string]time.Time{}, map[string]time.Time{}}
}
func (a *AuthData) normalize() {
	if a.Sessions == nil {
		a.Sessions = map[string]AuthSession{}
	}
	if a.Tickets == nil {
		a.Tickets = map[string]AuthGrant{}
	}
	if a.Access == nil {
		a.Access = map[string]AuthGrant{}
	}
	if a.Limits == nil {
		a.Limits = map[string]AuthLimit{}
	}
	if a.Pending == nil {
		a.Pending = map[string]AuthOIDCAttempt{}
	}
	if a.Replay == nil {
		a.Replay = map[string]time.Time{}
	}
	if a.Cutoff == nil {
		a.Cutoff = map[string]time.Time{}
	}
}
func (a *AuthData) Clean(now time.Time) {
	a.normalize()
	for key, s := range a.Sessions {
		if !now.Before(s.Expires) {
			delete(a.Sessions, key)
		}
	}
	for _, grants := range []map[string]AuthGrant{a.Tickets, a.Access} {
		for key, g := range grants {
			if _, ok := a.Sessions[g.SessionKey]; !ok || !now.Before(g.Expires) {
				delete(grants, key)
			}
		}
	}
	for key, v := range a.Limits {
		if !now.Before(v.Until) {
			delete(a.Limits, key)
		}
	}
	for key, v := range a.Pending {
		if !now.Before(v.Expires) {
			delete(a.Pending, key)
		}
	}
	for key, v := range a.Replay {
		if !now.Before(v) {
			delete(a.Replay, key)
		}
	}
	for key, v := range a.Cutoff {
		if !now.Before(v.Add(6 * time.Minute)) {
			delete(a.Cutoff, key)
		}
	}
}

type AuthSnapshot struct {
	Data                    AuthData
	State                   State
	Now                     time.Time
	EventID, ConfigRevision uint64
}
type AuthResolution struct {
	Session AuthSession
	Grant   AuthGrant
	State   State
	Now     time.Time
	Found   bool
}
type AuthEvent struct {
	ID, Revision uint64
	Kind         string
	CreatedAt    time.Time
}
type AuthEvents struct {
	Events []AuthEvent
	Reset  bool
}

// AuthRepository serializes security-sensitive writes with configuration commits.
// Reads and projections never retain an authorization cache across calls.
type AuthRepository interface {
	AuthSnapshot(context.Context, bool) (AuthSnapshot, error)
	AuthSnapshotForKeys(context.Context, []string, []string, []string) (AuthSnapshot, error)
	AuthResolve(context.Context, string, string) (AuthResolution, error)
	AuthUpdate(context.Context, bool, func(*AuthData, State, time.Time) error) error
	AuthRateLimit(context.Context, string, int, time.Duration) (bool, error)
	AuthBindPolicy(context.Context, string) error
	AuthPoll(context.Context, string, int) (AuthEvents, error)
	AuthAcknowledge(context.Context, string, uint64) error
	AuthMaintenance(context.Context) error
	UpdateAuthenticated(context.Context, func(State, AuthData, time.Time) error, func(*State) error) error
}

func authHash(key string) bool {
	if len(key) != 64 {
		return false
	}
	for _, r := range key {
		if r < '0' || r > '9' {
			if r < 'a' || r > 'f' {
				return false
			}
		}
	}
	return true
}
func validateAuth(a AuthData, now time.Time) error {
	if len(a.Sessions) > 4096 || len(a.Tickets) > 8192 || len(a.Access) > 8192 || len(a.Limits) > 256 || len(a.Pending) > 256 || len(a.Replay) > 4096 || len(a.Cutoff) > 4096 {
		return ErrAuthCapacity
	}
	ids := map[string]bool{}
	for key, s := range a.Sessions {
		if !authHash(key) || !resourceID.MatchString(s.UserID) || s.ID == "" || len(s.ID) > 64 || ids[s.ID] || s.CreatedAt.IsZero() || s.LastSeen.IsZero() || !now.Before(s.Expires) || len(s.UserAgent) > 512 || len(s.RemoteIP) > 128 || s.Method != "password" && s.Method != "oidc" {
			return fmt.Errorf("invalid shared session")
		}
		ids[s.ID] = true
		if s.Method == "oidc" && (ValidateIdentity(Identity{s.Issuer, s.Subject}) != nil || len(s.SID) > 1024) {
			return fmt.Errorf("invalid shared OIDC session")
		}
	}
	for _, grants := range []map[string]AuthGrant{a.Tickets, a.Access} {
		for key, g := range grants {
			s, ok := a.Sessions[g.SessionKey]
			if !authHash(key) || !ok || !resourceID.MatchString(g.ServiceID) || !now.Before(g.Expires) || g.Expires.After(s.Expires) {
				return fmt.Errorf("invalid shared resource grant")
			}
		}
	}
	for key, l := range a.Limits {
		if key == "" || len(key) > 256 || l.Count < 0 || !now.Before(l.Until) {
			return fmt.Errorf("invalid shared rate limit")
		}
	}
	for key, p := range a.Pending {
		if !authHash(key) || !authHash(p.Browser) || p.Nonce == "" || p.Verifier == "" || len(p.Nonce) > 128 || len(p.Verifier) > 128 || !now.Before(p.Expires) || p.Started.IsZero() || p.Issuer == "" || p.ClientID == "" || p.RedirectURI == "" {
			return fmt.Errorf("invalid shared OIDC attempt")
		}
	}
	for key, v := range a.Replay {
		if !authHash(key) || !now.Before(v) {
			return fmt.Errorf("invalid shared logout replay marker")
		}
	}
	for key, v := range a.Cutoff {
		if key == "" || len(key) > 1536 || v.IsZero() {
			return fmt.Errorf("invalid shared logout cutoff")
		}
	}
	return nil
}
func authRevoked(before, after AuthData) bool {
	for key, s := range before.Sessions {
		if next, ok := after.Sessions[key]; !ok || s.ID != next.ID || s.UserID != next.UserID || s.Method != next.Method || s.Issuer != next.Issuer || s.Subject != next.Subject || s.SID != next.SID || !s.Expires.Equal(next.Expires) {
			return true
		}
	}
	for key, g := range before.Access {
		if next, ok := after.Access[key]; !ok || g != next {
			return true
		}
	}
	return false
}
func authReconcile(a *AuthData, before, after State, now time.Time) {
	users := map[string]User{}
	for _, u := range after.Users {
		users[u.ID] = u
	}
	changed := map[string]bool{}
	for _, u := range before.Users {
		next, ok := users[u.ID]
		if !ok || !reflect.DeepEqual(u, next) {
			changed[u.ID] = true
		}
	}
	for key, s := range a.Sessions {
		if _, ok := users[s.UserID]; !ok || changed[s.UserID] {
			delete(a.Sessions, key)
		}
	}
	services := map[string]Service{}
	for _, s := range after.Services {
		services[s.ID] = s
	}
	changedServices := map[string]bool{}
	for _, s := range before.Services {
		next, ok := services[s.ID]
		if !ok || s.URL != next.URL || s.SiteID != next.SiteID || s.OrganizationID != next.OrganizationID || s.CustomDomain != next.CustomDomain || s.ConnectionMode != next.ConnectionMode {
			changedServices[s.ID] = true
		}
	}
	for _, grants := range []map[string]AuthGrant{a.Tickets, a.Access} {
		for key, g := range grants {
			if _, ok := services[g.ServiceID]; !ok || changedServices[g.ServiceID] {
				delete(grants, key)
			}
		}
	}
	a.Clean(now)
}
func authKind(kind string) bool {
	switch kind {
	case "session", "ticket", "access", "limit", "oidc", "replay", "cutoff":
		return true
	}
	return false
}
