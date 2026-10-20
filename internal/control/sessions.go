package control

import (
	"context"
	"errors"
	"github.com/XUJiahua/tunnel-mvp/internal/state"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"
)

var errSessionNotFound = errors.New("session not found")

type sessionInfo struct {
	ID        string    `json:"id"`
	UserID    string    `json:"userID"`
	Username  string    `json:"username"`
	Method    string    `json:"method"`
	CreatedAt time.Time `json:"createdAt"`
	LastSeen  time.Time `json:"lastSeen"`
	Expires   time.Time `json:"expires"`
	UserAgent string    `json:"userAgent"`
	RemoteIP  string    `json:"remoteIP"`
	Current   bool      `json:"current"`
}

func sessionMetadata(r *http.Request, method, issuer, subject, sid string) session {
	address, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		address = r.RemoteAddr
	}
	agent := r.UserAgent()
	if len(agent) > 512 {
		agent = agent[:512]
	}
	now := time.Now()
	return session{ID: randomID()[:24], CreatedAt: now, LastSeen: now, UserAgent: agent, RemoteIP: address, Method: method, Issuer: issuer, Subject: subject, SID: sid}
}
func (s *Server) listSessions(w http.ResponseWriter, r *http.Request, actor User) {
	_, current, _ := s.currentUser(r)
	users := map[string]string{}
	all := r.URL.Path == "/api/sessions"
	out := []sessionInfo{}
	snapshot, err := s.authSnapshot(r.Context(), true)
	if err != nil {
		fail(w, 503, "session storage unavailable")
		return
	}
	if s.auth.shared != nil {
		if err = s.validateAuthActor(snapshot.State, snapshot.Data, snapshot.Now, current, actor); err != nil {
			fail(w, 401, "authentication changed; reload")
			return
		}
	}
	for _, u := range snapshot.State.Users {
		users[u.ID] = u.Username
	}
	for key, session := range snapshot.Data.Sessions {
		if !all && session.UserID != actor.ID {
			continue
		}
		if filter := r.URL.Query().Get("userID"); all && filter != "" && session.UserID != filter {
			continue
		}
		out = append(out, sessionInfo{session.ID, session.UserID, users[session.UserID], session.Method, session.CreatedAt, session.LastSeen, session.Expires, session.UserAgent, session.RemoteIP, key == current})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	jsonOut(w, 200, out)
}
func (s *Server) revokeSessionKeys(keys map[string]bool) error {
	err := s.authUpdate(context.Background(), false, func(a *state.AuthData, _ State, _ time.Time) error {
		for key := range keys {
			delete(a.Sessions, key)
		}
		return nil
	})
	s.pruneSessionPools()
	s.cancelRequests(func(req liveRequest) bool { return keys[req.sessionKey] })
	return err
}
func (s *Server) deleteSession(w http.ResponseWriter, r *http.Request, actor User) {
	unlock := s.lockMutation(r)
	defer unlock()
	actor, current, ok := s.currentUser(r)
	if !ok {
		fail(w, 401, "sign in required")
		return
	}
	adminPath := strings.HasPrefix(r.URL.Path, "/api/sessions/")
	if adminPath && actor.Role != "admin" {
		fail(w, 403, "administrator required")
		return
	}
	target, owner := "", ""
	err := s.authUpdateAuthorized(r, actor, func(a *state.AuthData, _ State, _ time.Time) error {
		for key, value := range a.Sessions {
			if value.ID == r.PathValue("id") && (adminPath || value.UserID == actor.ID) {
				target, owner = key, value.UserID
				break
			}
		}
		if target == "" {
			return errSessionNotFound
		}
		delete(a.Sessions, target)
		return nil
	})
	if err != nil {
		if errors.Is(err, errSessionNotFound) {
			fail(w, 404, "session not found")
		} else {
			fail(w, 401, "authentication changed; reload")
		}
		return
	}
	s.pruneSessionPools()
	s.cancelRequests(func(req liveRequest) bool { return req.sessionKey == target })
	if target == current {
		s.clearConsoleCookie(w)
	}
	s.audit("session_revoked", actor.Username, "", owner, map[string]any{"sessionID": r.PathValue("id")})
	jsonOut(w, 200, map[string]bool{"ok": true, "reauthenticate": target == current})
}
func (s *Server) clearConsoleCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: consoleCookie, Path: "/", MaxAge: -1, HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteLaxMode})
}
