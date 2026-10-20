package control

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/XUJiahua/tunnel-mvp/internal/state"
)

// Memory backends retain their process-local lifecycle. PostgreSQL never reads
// these maps: every lookup/transaction goes through the shared repository.
func (s *Server) authSnapshot(ctx context.Context, withConfig bool) (state.AuthSnapshot, error) {
	if s.auth.shared != nil {
		return s.auth.shared.AuthSnapshot(ctx, withConfig)
	}
	var st State
	if withConfig {
		st = s.store.snapshot()
	}
	s.auth.mu.Lock()
	defer s.auth.mu.Unlock()
	now := time.Now()
	s.auth.clean(now)
	data, err := state.CloneAuthData(s.auth.AuthData)
	if err != nil {
		return state.AuthSnapshot{}, err
	}
	return state.AuthSnapshot{Data: data, State: st, Now: now}, nil
}
func (s *Server) authUpdate(ctx context.Context, withConfig bool, fn func(*state.AuthData, State, time.Time) error) error {
	if s.auth.shared != nil {
		return s.auth.shared.AuthUpdate(ctx, withConfig, fn)
	}
	var st State
	if withConfig {
		st = s.store.snapshot()
	}
	s.auth.mu.Lock()
	defer s.auth.mu.Unlock()
	now := time.Now()
	next, err := state.CloneAuthData(s.auth.AuthData)
	if err != nil {
		return err
	}
	next.Clean(now)
	if err = fn(&next, st, now); err != nil {
		return err
	}
	next.Clean(now)
	s.auth.AuthData = next
	return nil
}
func (s *Server) resolveAuth(ctx context.Context, kind, key string) (state.AuthResolution, error) {
	if s.auth.shared != nil {
		return s.auth.shared.AuthResolve(ctx, kind, key)
	}
	out := state.AuthResolution{Now: time.Now()}
	sessionKey := key
	s.auth.mu.Lock()
	if kind == "access" {
		var ok bool
		out.Grant, ok = s.auth.Access[key]
		if !ok || !out.Now.Before(out.Grant.Expires) {
			s.auth.mu.Unlock()
			return out, nil
		}
		sessionKey = out.Grant.SessionKey
	}
	var ok bool
	out.Session, ok = s.auth.Sessions[sessionKey]
	if ok && out.Now.Sub(out.Session.LastSeen) >= time.Minute {
		v := out.Session
		v.LastSeen = out.Now
		s.auth.Sessions[sessionKey] = v
	}
	s.auth.mu.Unlock()
	if !ok || !out.Now.Before(out.Session.Expires) {
		return out, nil
	}
	if reader, exists := s.store.Repository.(state.UserReader); exists {
		var err error
		out.State, ok, err = reader.UserContext(out.Session.UserID)
		if err != nil {
			return state.AuthResolution{}, err
		}
		if !ok {
			return out, nil
		}
	} else {
		out.State = s.store.snapshot()
	}
	for _, u := range out.State.Users {
		if u.ID == out.Session.UserID && !u.Disabled {
			out.Found = true
			break
		}
	}
	if kind == "access" && out.Found {
		svc, found := s.store.service(out.Grant.ServiceID)
		if !found {
			out.Found = false
		} else {
			out.State.Services = []Service{svc}
		}
	}
	return out, nil
}
func (s *Server) resolvedUser(result state.AuthResolution) (User, bool) {
	if !result.Found {
		return User{}, false
	}
	if s.identitySync != nil && result.Session.Method == "oidc" {
		if s.auth.shared != nil {
			if result.Session.IdentityEpoch == "" || !state.AuthIdentityAllowed(result.Identity, result.Session, result.Now) {
				return User{}, false
			}
		} else if !s.identitySessionAllowed(result.Session) {
			return User{}, false
		}
	}
	for _, u := range result.State.Users {
		if u.ID == result.Session.UserID {
			return effectiveUser(result.State, u), !u.Disabled
		}
	}
	return User{}, false
}
func (s *Server) authRateLimit(ctx context.Context, key string, maximum int, period time.Duration) (bool, error) {
	if s.auth.shared != nil {
		return s.auth.shared.AuthRateLimit(ctx, key, maximum, period)
	}
	allowed := false
	err := s.authUpdate(ctx, false, func(a *state.AuthData, _ State, now time.Time) error {
		v, exists := a.Limits[key]
		if !exists {
			if len(a.Limits) >= 256 {
				return nil
			}
			v.Until = now.Add(period)
		}
		if v.Count >= maximum {
			return nil
		}
		v.Count++
		a.Limits[key] = v
		allowed = true
		return nil
	})
	return allowed, err
}
func (s *Server) authSession(ctx context.Context, key string) (session, bool, error) {
	result, err := s.resolveAuth(ctx, "session", key)
	return result.Session, result.Found, err
}
func actorFingerprint(u User) string {
	raw, _ := json.Marshal(struct {
		User        User
		Permissions []string
	}{u, u.Permissions})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
func (s *Server) validateAuthActor(st State, a state.AuthData, now time.Time, key string, actor User) error {
	sess, ok := a.Sessions[key]
	if !ok || sess.UserID != actor.ID || !now.Before(sess.Expires) || !s.authIdentityAllowed(a, sess, now) {
		return state.ErrAuthChanged
	}
	for _, u := range st.Users {
		if u.ID == actor.ID && !u.Disabled && actorFingerprint(effectiveUser(st, u)) == actorFingerprint(actor) {
			return nil
		}
	}
	return state.ErrAuthChanged
}
func (s *Server) updateAuthorized(r *http.Request, actor User, fn func(*State) error) error {
	if s.auth.shared == nil {
		return s.store.update(fn)
	}
	cookie, err := r.Cookie(consoleCookie)
	if err != nil {
		return state.ErrAuthChanged
	}
	key := digest(cookie.Value)
	return s.auth.shared.UpdateAuthenticated(r.Context(), func(st State, a state.AuthData, now time.Time) error {
		return s.validateAuthActor(st, a, now, key, actor)
	}, fn)
}
func (s *Server) authUpdateAuthorized(r *http.Request, actor User, fn func(*state.AuthData, State, time.Time) error) error {
	cookie, err := r.Cookie(consoleCookie)
	if err != nil {
		return state.ErrAuthChanged
	}
	key := digest(cookie.Value)
	return s.authUpdate(r.Context(), true, func(a *state.AuthData, st State, now time.Time) error {
		if err := s.validateAuthActor(st, *a, now, key, actor); err != nil {
			return err
		}
		return fn(a, st, now)
	})
}
func (s *Server) configureSharedAuth() error {
	repository, ok := s.store.Repository.(state.AuthRepository)
	if !ok {
		return nil
	}
	raw, err := json.Marshal(struct {
		PublicURL, AccessURL string
		OIDC                 any
		Cluster              string `json:",omitempty"`
	}{s.cfg.PublicURL, s.cfg.AccessURL, s.cfg.OIDC, s.clusterPolicyHash()})
	if err != nil {
		return err
	}
	fingerprint := sha256.Sum256(raw)
	if err = repository.AuthBindPolicy(context.Background(), hex.EncodeToString(fingerprint[:])); err != nil {
		return err
	}
	s.auth.shared = repository
	path := filepath.Join(s.cfg.DataDir, "node.id")
	raw, err = os.ReadFile(path)
	if os.IsNotExist(err) {
		id := randomID()
		if s.cfg.Cluster.Enabled {
			id = s.cfg.Cluster.NodeID
		}
		file, e := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e != nil {
			return e
		}
		_, writeErr := file.WriteString(id + "\n")
		closeErr := file.Close()
		if writeErr != nil {
			return writeErr
		}
		if closeErr != nil {
			return closeErr
		}
		s.nodeID = id
		return nil
	}
	if err != nil {
		return err
	}
	id := strings.TrimSpace(string(raw))
	if s.cfg.Cluster.Enabled {
		if id != s.cfg.Cluster.NodeID {
			return fmt.Errorf("node.id differs from cluster.nodeID; use the matching runtime directory")
		}
	} else {
		if len(id) != 48 {
			return fmt.Errorf("invalid node.id")
		}
		if _, err = hex.DecodeString(id); err != nil {
			return fmt.Errorf("invalid node.id")
		}
	}

	s.nodeID = id
	return nil
}

func (s *Server) authIdentityAllowed(a state.AuthData, sess session, now time.Time) bool {
	if s.identitySync == nil || sess.Method != "oidc" {
		return true
	}
	if s.auth.shared == nil {
		return s.identitySessionAllowed(sess)
	}
	return sess.IdentityEpoch != "" && state.AuthIdentityAllowed(a.Identities[sess.UserID], sess, now)
}
