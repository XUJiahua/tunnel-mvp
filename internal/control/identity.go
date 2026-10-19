package control

import (
	"fmt"
	"net/http"
	"time"

	"github.com/XUJiahua/tunnel-mvp/internal/state"
)

type userView struct {
	User
	PasswordLogin bool   `json:"passwordLogin"`
	SessionMethod string `json:"sessionMethod,omitempty"`
}

func publicUser(u User) userView {
	local := u.PasswordHash != ""
	u.PasswordHash = ""
	return userView{User: u, PasswordLogin: local}
}
func (s *Server) issueSession(w http.ResponseWriter, u User, expires time.Time, metadata session) {
	token := randomID()
	s.auth.mu.Lock()
	s.auth.clean(time.Now())
	metadata.UserID, metadata.Expires = u.ID, expires
	s.auth.sessions[digest(token)] = metadata
	s.auth.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: consoleCookie, Value: token, Path: "/", HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteLaxMode, MaxAge: max(1, int(time.Until(expires).Seconds()))})
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
	s.accessMu.Lock()
	defer s.accessMu.Unlock()
	if current, _, ok := s.currentUser(r); !ok || current.Role != "admin" {
		fail(w, 401, "administrator sign in required")
		return
	}
	var out User
	err := s.store.update(func(st *State) error {
		for i := range st.Users {
			if st.Users[i].ID == r.PathValue("id") && st.Users[i].Role != "admin" {
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
	s.auth.mu.Lock()
	view.SessionMethod = s.auth.sessions[key].Method
	s.auth.mu.Unlock()
	jsonOut(w, 200, view)
}
