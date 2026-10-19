package control

import (
	"net"
	"net/http"
	"sort"
	"strings"
	"time"
)

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
	for _, u := range s.store.snapshot().Users {
		users[u.ID] = u.Username
	}
	all := r.URL.Path == "/api/sessions"
	out := []sessionInfo{}
	s.auth.mu.Lock()
	s.auth.clean(time.Now())
	for key, session := range s.auth.sessions {
		if !all && session.UserID != actor.ID {
			continue
		}
		if filter := r.URL.Query().Get("userID"); all && filter != "" && session.UserID != filter {
			continue
		}
		out = append(out, sessionInfo{session.ID, session.UserID, users[session.UserID], session.Method, session.CreatedAt, session.LastSeen, session.Expires, session.UserAgent, session.RemoteIP, key == current})
	}
	s.auth.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	jsonOut(w, 200, out)
}
func (s *Server) revokeSessionKeys(keys map[string]bool) {
	s.auth.mu.Lock()
	for key := range keys {
		delete(s.auth.sessions, key)
	}
	s.auth.clean(time.Now())
	s.auth.mu.Unlock()
	s.cancelRequests(func(req liveRequest) bool { return keys[req.sessionKey] })
}
func (s *Server) deleteSession(w http.ResponseWriter, r *http.Request, actor User) {
	s.accessMu.Lock()
	defer s.accessMu.Unlock()
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
	s.auth.mu.Lock()
	for key, value := range s.auth.sessions {
		if value.ID == r.PathValue("id") && (adminPath || value.UserID == actor.ID) {
			target, owner = key, value.UserID
			break
		}
	}
	s.auth.mu.Unlock()
	if target == "" {
		fail(w, 404, "session not found")
		return
	}
	s.revokeSessionKeys(map[string]bool{target: true})
	if target == current {
		s.clearConsoleCookie(w)
	}
	s.audit("session_revoked", actor.Username, "", owner, map[string]any{"sessionID": r.PathValue("id")})
	jsonOut(w, 200, map[string]bool{"ok": true, "reauthenticate": target == current})
}
func (s *Server) clearConsoleCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: consoleCookie, Path: "/", MaxAge: -1, HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteLaxMode})
}
