package control

import (
	"context"
	"github.com/XUJiahua/tunnel-mvp/internal/state"
	"net/http"
	"time"
)

// accessMu orders credential/resource changes with authentication and live
// request registration. It is never held while forwarding network traffic.
type liveRequest struct {
	system                        bool
	route                         Service
	accessKey, method             string
	lease                         *authLease
	userID, sessionKey, serviceID string
	expires                       time.Time
	cancel                        context.CancelFunc
}

func (s *Server) trackRequest(parent context.Context, u User, g grant, accessKey string, resolution state.AuthResolution, proofStarted time.Time) (context.Context, func()) {
	expires := resolution.Session.Expires
	if g.Expires.Before(expires) {
		expires = g.Expires
	}
	var deadline time.Time
	if s.auth.shared != nil {
		deadline = proofStarted.Add(expires.Sub(resolution.Now))
	} else {
		deadline = expires
	}
	ctx, cancel := context.WithDeadline(parent, deadline)
	var lease *authLease
	if s.auth.shared != nil {
		until := proofStarted.Add(authorizationLease)
		if s.identitySync != nil && resolution.Session.Method == "oidc" {
			expires := proofStarted.Add(resolution.Identity.ValidUntil.Sub(resolution.Now))
			if expires.Before(until) {
				until = expires
			}
		}
		lease = newAuthLease(ctx, cancel, until)
	}
	var route Service
	for _, v := range resolution.State.Services {
		if v.ID == g.ServiceID {
			route = v
			break
		}
	}
	cancelRequest := func() {
		if lease != nil {
			lease.stop()
		}
		cancel()
	}
	id := randomID()
	s.liveMu.Lock()
	if s.storageFenced.Load() {
		cancel()
	}
	s.live[id] = liveRequest{userID: u.ID, sessionKey: g.SessionKey, serviceID: g.ServiceID, expires: expires, cancel: cancelRequest, accessKey: accessKey, method: resolution.Session.Method, lease: lease, route: route}
	s.liveMu.Unlock()
	return ctx, func() {
		cancelRequest()
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
	// PostgreSQL configuration commits already revoke affected sessions atomically.
	// A second blanket delete here could revoke a fresh login on another node.
	if s.auth.shared == nil {
		_ = s.authUpdate(context.Background(), false, func(a *state.AuthData, _ State, _ time.Time) error {
			for key, sess := range a.Sessions {
				if sess.UserID == id {
					delete(a.Sessions, key)
				}
			}
			return nil
		})
	}
	s.pruneSessionPools()
	s.cancelRequests(func(r liveRequest) bool { return r.userID == id })
}
func (s *Server) revokeService(id string) {
	if s.auth.shared == nil {
		_ = s.authUpdate(context.Background(), false, func(a *state.AuthData, _ State, _ time.Time) error {
			for _, grants := range []map[string]grant{a.Tickets, a.Access} {
				for key, g := range grants {
					if g.ServiceID == id {
						delete(grants, key)
					}
				}
			}
			return nil
		})
	}
	s.pruneSessionPools()
	s.cancelRequests(func(r liveRequest) bool { return r.serviceID == id })
	s.dropTransport(id)
	s.monitorMu.Lock()
	delete(s.reports, id)
	s.monitorMu.Unlock()
}
func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	unlock := s.lockMutation(r)
	defer unlock()
	u, key, ok := s.currentUser(r)
	if err := s.revokeSessionKeys(map[string]bool{key: true}); err != nil {
		fail(w, 503, "session revocation unavailable; retry logout")
		return
	}
	s.pruneSessionPools()
	s.cancelRequests(func(req liveRequest) bool { return req.sessionKey == key })
	s.clearConsoleCookie(w)
	if ok {
		s.audit("logout", u.Username, "", "")
	}
	jsonOut(w, 200, map[string]bool{"ok": true})
}

// A request may enter through require (administrator mutation) or a public/
// member handler. Never recursively lock the non-reentrant access mutex.
type mutationKey struct{}

func (s *Server) lockMutation(r *http.Request) func() {
	if held, _ := r.Context().Value(mutationKey{}).(bool); held {
		return func() {}
	}
	s.accessMu.Lock()
	return s.accessMu.Unlock
}
