package control

import (
	"fmt"
	"net/http"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

// Administrator changes require a fresh local-password proof, including when
// the caller originally signed in with OIDC. Sessions never cache this proof.
func (s *Server) manageAdministrator(w http.ResponseWriter, r *http.Request, actor User) {
	var body struct {
		CurrentPassword string `json:"currentPassword"`
		Username        string `json:"username"`
		Password        string `json:"password"`
		Action          string `json:"action"`
	}
	if !decode(w, r, &body) {
		return
	}
	if !s.inviteAttempt("administrator-change", 20) {
		fail(w, 429, "too many administrator changes; retry in one minute")
		return
	}
	unlock := s.lockMutation(r)
	defer unlock()
	current, _, ok := s.currentUser(r)
	if !ok || current.Role != "admin" {
		fail(w, 401, "administrator sign in required")
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(current.PasswordHash), []byte(body.CurrentPassword)) != nil {
		fail(w, 403, "current password is incorrect")
		return
	}
	create := r.Method == "POST"
	var out User
	if create {
		body.Username = strings.TrimSpace(body.Username)
		if body.Action != "" || body.Username == "" || len(body.Username) > 100 || len(body.Password) < 12 || len(body.Password) > 72 {
			fail(w, 400, "username and a 12–72 byte password required")
			return
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(body.Password), bcrypt.DefaultCost)
		if err != nil {
			fail(w, 400, "invalid password")
			return
		}
		out = User{ID: randomID()[:16], Username: body.Username, PasswordHash: string(hash), Role: "admin", SiteIDs: []string{}, ServiceIDs: []string{}}
	} else if body.Username != "" || body.Password != "" {
		fail(w, 400, "only action and currentPassword are accepted")
		return
	}
	id := r.PathValue("id")
	action := body.Action
	if r.Method == "DELETE" {
		if action != "" {
			fail(w, 400, "action is not accepted for deletion")
			return
		}
		action = "delete"
	}
	err := s.updateAuthorized(r, actor, func(st *State) error {
		if create {
			st.Users = append(st.Users, out)
			return nil
		}
		for i := range st.Users {
			u := &st.Users[i]
			if u.ID != id {
				continue
			}
			if action == "promote" {
				if u.Role != "viewer" || u.OrganizationID != "" || u.Disabled || u.PasswordHash == "" {
					return fmt.Errorf("promotion requires an enabled local-password member")
				}
				u.Role = "admin"
				u.RoleID = ""
			} else {
				if u.Role != "admin" {
					return fmt.Errorf("administrator not found")
				}
				switch action {
				case "enable":
					u.Disabled = false
				case "disable":
					u.Disabled = true
				case "demote":
					u.Role = "viewer"
				case "delete":
					out = *u
					st.Users = append(st.Users[:i], st.Users[i+1:]...)
					pruneGrantReferences(st)
					return nil
				default:
					return fmt.Errorf("action must be promote, demote, disable or enable")
				}
			}
			out = *u
			return nil
		}
		return fmt.Errorf("account not found")
	})
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	code := 200
	if create {
		action = "create"
		code = 201
	} else {
		s.revokeUser(out.ID)
	}
	s.audit("administrator_"+action, actor.Username, "", out.ID)
	jsonOut(w, code, map[string]any{"user": publicUser(out), "reauthenticate": !create && out.ID == actor.ID})
}
