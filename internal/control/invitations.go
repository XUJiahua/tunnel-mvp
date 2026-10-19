package control

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/XUJiahua/tunnel-mvp/internal/state"
	"golang.org/x/crypto/bcrypt"
)

type inviteView struct {
	ID         string    `json:"id"`
	Username   string    `json:"username"`
	CreatedAt  time.Time `json:"createdAt"`
	ExpiresAt  time.Time `json:"expiresAt"`
	SiteIDs    []string  `json:"siteIDs"`
	ServiceIDs []string  `json:"serviceIDs"`
	GroupIDs   []string  `json:"groupIDs"`
	Expired    bool      `json:"expired"`
}

func viewInvite(v state.Invite) inviteView {
	return inviteView{v.ID, v.Username, v.CreatedAt, v.ExpiresAt, v.SiteIDs, v.ServiceIDs, v.GroupIDs, !time.Now().Before(v.ExpiresAt)}
}
func (s *Server) listInvites(w http.ResponseWriter, r *http.Request, actor User) {
	out := []inviteView{}
	for _, v := range s.store.snapshot().Invites {
		out = append(out, viewInvite(v))
	}
	jsonOut(w, 200, out)
}
func (s *Server) createInvite(w http.ResponseWriter, r *http.Request, actor User) {
	var body struct {
		Username   string   `json:"username"`
		Hours      int      `json:"hours"`
		SiteIDs    []string `json:"siteIDs"`
		ServiceIDs []string `json:"serviceIDs"`
		GroupIDs   []string `json:"groupIDs"`
	}
	if !decode(w, r, &body) {
		return
	}
	body.Username = strings.TrimSpace(body.Username)
	if body.Hours == 0 {
		body.Hours = 72
	}
	if body.Username == "" || len(body.Username) > 100 || body.Hours < 1 || body.Hours > 168 {
		fail(w, 400, "username and 1–168 validity hours required")
		return
	}
	token := randomID()
	now := time.Now().UTC()
	invite := state.Invite{ID: randomID()[:16], Username: body.Username, TokenHash: digest(token), CreatedAt: now, ExpiresAt: now.Add(time.Duration(body.Hours) * time.Hour), SiteIDs: body.SiteIDs, ServiceIDs: body.ServiceIDs, GroupIDs: body.GroupIDs}
	s.accessMu.Lock()
	defer s.accessMu.Unlock()
	if current, _, ok := s.currentUser(r); !ok || current.Role != "admin" {
		fail(w, 401, "administrator sign in required")
		return
	}
	err := s.store.update(func(st *State) error {
		// Expired invites stop reserving a username when a new invite is created.
		active := []state.Invite{}
		for _, v := range st.Invites {
			if now.Before(v.ExpiresAt) {
				active = append(active, v)
			}
		}
		st.Invites = active
		st.Invites = append(st.Invites, invite)
		return nil
	})
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	s.audit("invitation_created", actor.Username, "", invite.ID)
	jsonOut(w, 201, map[string]any{"invitation": viewInvite(invite), "url": s.cfg.PublicURL + "/#invite=" + token})
}
func (s *Server) deleteInvite(w http.ResponseWriter, r *http.Request, actor User) {
	s.accessMu.Lock()
	defer s.accessMu.Unlock()
	if current, _, ok := s.currentUser(r); !ok || current.Role != "admin" {
		fail(w, 401, "administrator sign in required")
		return
	}
	id := r.PathValue("id")
	err := s.store.update(func(st *State) error {
		for i, v := range st.Invites {
			if v.ID == id {
				st.Invites = append(st.Invites[:i], st.Invites[i+1:]...)
				return nil
			}
		}
		return fmt.Errorf("invitation not found")
	})
	if err != nil {
		fail(w, 404, "invitation not found")
		return
	}
	s.audit("invitation_revoked", actor.Username, "", id)
	jsonOut(w, 200, map[string]bool{"ok": true})
}
func (s *Server) inviteAttempt(key string, max int) bool {
	s.auth.mu.Lock()
	defer s.auth.mu.Unlock()
	now := time.Now()
	s.auth.clean(now)
	lim := s.auth.attempts[key]
	if lim.Count >= max && now.Before(lim.Until) {
		return false
	}
	if lim.Until.IsZero() {
		lim.Until = now.Add(time.Minute)
	}
	lim.Count++
	s.auth.attempts[key] = lim
	return true
}
func findInvite(st State, token string) (state.Invite, bool) {
	if len(token) != 48 {
		return state.Invite{}, false
	}
	hash := digest(token)
	for _, v := range st.Invites {
		if constantEqual(v.TokenHash, hash) && time.Now().Before(v.ExpiresAt) {
			return v, true
		}
	}
	return state.Invite{}, false
}
func (s *Server) inspectInvite(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token string `json:"token"`
	}
	if !decode(w, r, &body) {
		return
	}
	if !s.inviteAttempt("invitation_inspect", 120) {
		fail(w, 429, "too many invitation requests")
		return
	}
	v, ok := findInvite(s.store.snapshot(), body.Token)
	if !ok {
		fail(w, 404, "invitation invalid or expired")
		return
	}
	jsonOut(w, 200, map[string]any{"username": v.Username, "expiresAt": v.ExpiresAt})
}
func (s *Server) acceptInvite(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token    string `json:"token"`
		Password string `json:"password"`
	}
	if !decode(w, r, &body) {
		return
	}
	if !s.inviteAttempt("invitation_accept", 30) {
		fail(w, 429, "too many invitation attempts")
		return
	}
	if len(body.Password) < 12 || len(body.Password) > 72 {
		fail(w, 400, "password must be 12–72 bytes")
		return
	}
	if _, ok := findInvite(s.store.snapshot(), body.Token); !ok {
		fail(w, 404, "invitation invalid or expired")
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(body.Password), bcrypt.DefaultCost)
	if err != nil {
		fail(w, 400, "invalid password")
		return
	}
	s.accessMu.Lock()
	defer s.accessMu.Unlock()
	var member User
	err = s.store.update(func(st *State) error {
		invite, ok := findInvite(*st, body.Token)
		if !ok {
			return fmt.Errorf("invitation invalid or expired")
		}
		member = User{ID: randomID()[:16], Username: invite.Username, PasswordHash: string(hash), Role: "viewer", SiteIDs: invite.SiteIDs, ServiceIDs: invite.ServiceIDs}
		for _, groupID := range invite.GroupIDs {
			for i := range st.Groups {
				if st.Groups[i].ID == groupID {
					st.Groups[i].MemberIDs = append(st.Groups[i].MemberIDs, member.ID)
				}
			}
		}
		st.Users = append(st.Users, member)
		for i, v := range st.Invites {
			if v.ID == invite.ID {
				st.Invites = append(st.Invites[:i], st.Invites[i+1:]...)
				break
			}
		}
		return nil
	})
	if err != nil {
		fail(w, 404, "invitation invalid or expired")
		return
	}
	s.issueSession(w, member, time.Now().Add(12*time.Hour), sessionMetadata(r, "password", "", "", ""))
	s.audit("invitation_accepted", member.Username, "", member.ID)
	jsonOut(w, 201, publicUser(member))
}
