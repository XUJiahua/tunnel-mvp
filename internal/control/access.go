package control

import (
	"context"
	"net/http"
	"time"
)

// accessMu orders credential/resource changes with authentication and live
// request registration. It is never held while forwarding network traffic.
type liveRequest struct {
	userID, sessionKey, serviceID string
	expires                       time.Time
	cancel                        context.CancelFunc
}

func (s *Server) trackRequest(parent context.Context, u User, g grant) (context.Context, func()) {
	s.auth.mu.Lock()
	expires := s.auth.sessions[g.SessionKey].Expires
	s.auth.mu.Unlock()
	if g.Expires.Before(expires) {
		expires = g.Expires
	}
	ctx, cancel := context.WithDeadline(parent, expires)
	id := randomID()
	s.liveMu.Lock()
	s.live[id] = liveRequest{u.ID, g.SessionKey, g.ServiceID, expires, cancel}
	s.liveMu.Unlock()
	return ctx, func() {
		cancel()
		s.liveMu.Lock()
		delete(s.live, id)
		s.liveMu.Unlock()
	}
}

func (s *Server) cancelRequests(match func(liveRequest) bool) {
	s.liveMu.Lock()
	defer s.liveMu.Unlock()
	for _, req := range s.live {
		if match(req) {
			req.cancel()
		}
	}
}

// Callers hold accessMu exclusively. Removing sessions invalidates console,
// application cookies and outstanding tickets together.
func (s *Server) revokeUser(id string) {
	s.auth.mu.Lock()
	for key, sess := range s.auth.sessions {
		if sess.UserID == id {
			delete(s.auth.sessions, key)
		}
	}
	s.auth.clean(time.Now())
	s.auth.mu.Unlock()
	s.cancelRequests(func(r liveRequest) bool { return r.userID == id })
}
func (s *Server) revokeService(id string) {
	s.auth.mu.Lock()
	for key, g := range s.auth.tickets {
		if g.ServiceID == id {
			delete(s.auth.tickets, key)
		}
	}
	for key, g := range s.auth.access {
		if g.ServiceID == id {
			delete(s.auth.access, key)
		}
	}
	s.auth.mu.Unlock()
	s.cancelRequests(func(r liveRequest) bool { return r.serviceID == id })
	s.dropTransport(id)
	s.monitorMu.Lock()
	delete(s.reports, id)
	s.monitorMu.Unlock()
}
func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	s.accessMu.Lock()
	defer s.accessMu.Unlock()
	u, key, ok := s.currentUser(r)
	s.auth.mu.Lock()
	delete(s.auth.sessions, key)
	s.auth.clean(time.Now())
	s.auth.mu.Unlock()
	s.cancelRequests(func(req liveRequest) bool { return req.sessionKey == key })
	s.clearConsoleCookie(w)
	if ok {
		s.audit("logout", u.Username, "", "")
	}
	jsonOut(w, 200, map[string]bool{"ok": true})
}
