package control

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/XUJiahua/tunnel-mvp/internal/state"
)

type userView struct {
	User
	PasswordLogin bool     `json:"passwordLogin"`
	SessionMethod string   `json:"sessionMethod,omitempty"`
	Permissions   []string `json:"permissions"`
}

func publicUser(u User) userView {
	local := u.PasswordHash != ""
	u.PasswordHash = ""
	return userView{User: u, PasswordLogin: local, Permissions: u.Permissions}
}
func (s *Server) issueSession(w http.ResponseWriter, u User, expires time.Time, metadata session, started ...time.Time) bool {
	token := randomID()
	lifetime := time.Until(expires)
	err := s.authUpdate(context.Background(), true, func(a *state.AuthData, st State, now time.Time) error {
		var fresh User
		found := false
		for _, v := range st.Users {
			if v.ID == u.ID && !v.Disabled {
				fresh = v
				found = true
				break
			}
		}
		if !found {
			return state.ErrAuthChanged
		}
		if metadata.Method == "password" {
			if fresh.PasswordHash == "" || fresh.PasswordHash != u.PasswordHash {
				return state.ErrAuthChanged
			}
		} else {
			if fresh.Identity == nil || fresh.Identity.Issuer != metadata.Issuer || fresh.Identity.Subject != metadata.Subject {
				return state.ErrAuthChanged
			}
			began := now
			if len(started) > 0 {
				began = started[0]
			}
			for _, key := range []string{"sid:" + metadata.SID, "sub:" + metadata.Subject} {
				if cutoff, ok := a.Cutoff[key]; ok && !began.After(cutoff) {
					return state.ErrAuthChanged
				}
			}
			if s.identitySync != nil && s.auth.shared != nil {
				status, ok := a.Identities[u.ID]
				if !ok || !status.Enabled || status.Issuer != metadata.Issuer || status.Subject != metadata.Subject || !now.Before(status.ValidUntil) {
					return state.ErrAuthChanged
				}
				metadata.IdentityEpoch = status.Epoch
			}
		}
		if len(a.Sessions) >= 4096 {
			return state.ErrAuthCapacity
		}
		if s.auth.shared != nil {
			bounded := now.Add(lifetime)
			if bounded.Before(expires) {
				expires = bounded
			}
			metadata.CreatedAt = now
			metadata.LastSeen = now
		}
		if !now.Before(expires) {
			return state.ErrAuthChanged
		}
		metadata.UserID = u.ID
		metadata.Expires = expires
		a.Sessions[digest(token)] = metadata
		return nil
	})
	if err != nil {
		fail(w, 503, "authentication changed or unavailable; sign in again")
		return false
	}
	http.SetCookie(w, &http.Cookie{Name: consoleCookie, Value: token, Path: "/", HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteLaxMode, MaxAge: max(1, int(time.Until(expires).Seconds()))})
	return true
}
func (s *Server) updateIdentity(w http.ResponseWriter, r *http.Request, actor User) {
	var body struct {
		Subject string `json:"subject"`
	}
	if !decode(w, r, &body) {
		return
	}
	var binding *state.Identity
	if body.Subject != "" {
		if s.oidc == nil {
			fail(w, 400, "configure OIDC before binding identities")
			return
		}
		binding = &state.Identity{Issuer: s.cfg.OIDC.Issuer, Subject: body.Subject}
		if err := state.ValidateIdentity(*binding); err != nil {
			fail(w, 400, err.Error())
			return
		}
	}
	s.mutateMember(w, r, actor, "user_identity_updated", func(u *User) error {
		if binding == nil && u.PasswordHash == "" {
			return fmt.Errorf("OIDC-only members must retain an identity; disable or delete instead")
		}
		u.Identity = binding
		return nil
	})
}
func (s *Server) setDisabled(w http.ResponseWriter, r *http.Request, actor User) {
	var body struct {
		Disabled *bool `json:"disabled"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.Disabled == nil {
		fail(w, 400, "disabled is required")
		return
	}
	event := "user_enabled"
	if *body.Disabled {
		event = "user_disabled"
	}
	s.mutateMember(w, r, actor, event, func(u *User) error { u.Disabled = *body.Disabled; return nil })
}
func (s *Server) mutateMember(w http.ResponseWriter, r *http.Request, actor User, event string, change func(*User) error) {
	unlock := s.lockMutation(r)
	defer unlock()
	if current, _, ok := s.currentUser(r); !ok || !hasPermission(current, "members:manage") {
		fail(w, 401, "administrator sign in required")
		return
	}
	var out User
	err := s.updateAuthorized(r, actor, func(st *State) error {
		for i := range st.Users {
			if st.Users[i].ID == r.PathValue("id") && st.Users[i].Role != "admin" && canManageMember(actor, st.Users[i]) {
				if err := change(&st.Users[i]); err != nil {
					return err
				}
				out = st.Users[i]
				return nil
			}
		}
		return fmt.Errorf("member not found; administrators retain local recovery access")
	})
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	s.revokeUser(out.ID)
	s.audit(event, actor.Username, "", out.ID)
	jsonOut(w, 200, publicUser(out))
}

func (s *Server) profile(w http.ResponseWriter, r *http.Request, u User) {
	_, key, _ := s.currentUser(r)
	view := publicUser(u)
	if sess, ok, err := s.authSession(r.Context(), key); err == nil && ok {
		view.SessionMethod = sess.Method
	}
	jsonOut(w, 200, view)
}
