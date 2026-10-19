package control

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/XUJiahua/tunnel-mvp/internal/config"
	"golang.org/x/crypto/bcrypt"
)

func (s *Server) updateSite(w http.ResponseWriter, r *http.Request, u User) {
	var body struct {
		Name string `json:"name"`
	}
	if !decode(w, r, &body) {
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	if body.Name == "" || len(body.Name) > 100 {
		fail(w, 400, "name must be 1–100 bytes")
		return
	}
	var out Site
	err := s.store.update(func(st *State) error {
		for i := range st.Sites {
			if st.Sites[i].ID == r.PathValue("id") {
				st.Sites[i].Name = body.Name
				out = st.Sites[i]
				return nil
			}
		}
		return fmt.Errorf("site not found")
	})
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	out.TokenHash = ""
	s.audit("site_updated", u.Username, out.ID, "")
	jsonOut(w, 200, out)
}
func (s *Server) updateService(w http.ResponseWriter, r *http.Request, u User) {
	var body struct {
		Name string `json:"name"`
		URL  string `json:"url"`
	}
	if !decode(w, r, &body) {
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	target, err := config.Origin(body.URL)
	if err != nil || body.Name == "" || len(body.Name) > 100 {
		fail(w, 400, "name and HTTP/HTTPS origin required")
		return
	}
	s.accessMu.Lock()
	defer s.accessMu.Unlock()
	var out Service
	changed := false
	err = s.store.update(func(st *State) error {
		for i := range st.Services {
			if st.Services[i].ID == r.PathValue("id") {
				changed = st.Services[i].URL != target.String()
				st.Services[i].Name, st.Services[i].URL = body.Name, target.String()
				out = st.Services[i]
				return nil
			}
		}
		return fmt.Errorf("service not found")
	})
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	if changed {
		s.revokeService(out.ID)
	}
	s.audit("service_updated", u.Username, out.SiteID, out.ID)
	jsonOut(w, 200, out)
}

func validateGrants(st *State, sites, services []string) error {
	for _, id := range sites {
		found := false
		for _, site := range st.Sites {
			if site.ID == id {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("unknown site grant")
		}
	}
	for _, id := range services {
		found := false
		for _, svc := range st.Services {
			if svc.ID == id {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("unknown application grant")
		}
	}
	return nil
}
func (s *Server) updateUser(w http.ResponseWriter, r *http.Request, actor User) {
	var body struct {
		SiteIDs    []string `json:"siteIDs"`
		ServiceIDs []string `json:"serviceIDs"`
	}
	if !decode(w, r, &body) {
		return
	}
	s.accessMu.Lock()
	defer s.accessMu.Unlock()
	if body.SiteIDs == nil {
		body.SiteIDs = []string{}
	}
	if body.ServiceIDs == nil {
		body.ServiceIDs = []string{}
	}
	var out User
	err := s.store.update(func(st *State) error {
		if err := validateGrants(st, body.SiteIDs, body.ServiceIDs); err != nil {
			return err
		}
		for i := range st.Users {
			if st.Users[i].ID == r.PathValue("id") && st.Users[i].Role != "admin" {
				st.Users[i].SiteIDs, st.Users[i].ServiceIDs = body.SiteIDs, body.ServiceIDs
				out = st.Users[i]
				return nil
			}
		}
		return fmt.Errorf("member not found")
	})
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	// Keep the console session; close requests that lost their service grant.
	s.cancelRequests(func(req liveRequest) bool {
		svc, ok := s.store.service(req.serviceID)
		return req.userID == out.ID && (!ok || !canAccessService(out, svc))
	})
	out.PasswordHash = ""
	s.audit("user_grants_updated", actor.Username, "", out.ID)
	jsonOut(w, 200, out)
}
func (s *Server) changePassword(w http.ResponseWriter, r *http.Request, actor User) {
	var body struct {
		CurrentPassword string `json:"currentPassword"`
		Password        string `json:"password"`
	}
	if !decode(w, r, &body) {
		return
	}
	s.setPassword(w, r, actor, actor.ID, body.CurrentPassword, body.Password, true)
}
func (s *Server) resetPassword(w http.ResponseWriter, r *http.Request, actor User) {
	var body struct {
		Password string `json:"password"`
	}
	if !decode(w, r, &body) {
		return
	}
	s.setPassword(w, r, actor, r.PathValue("id"), "", body.Password, false)
}
func (s *Server) setPassword(w http.ResponseWriter, r *http.Request, actor User, id, current, password string, verify bool) {
	if len(password) < 12 || len(password) > 72 {
		fail(w, 400, "password must be 12–72 bytes")
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		fail(w, 400, "invalid password")
		return
	}
	s.accessMu.Lock()
	defer s.accessMu.Unlock()
	// Reject requests authorized before a concurrent password change/logout.
	if _, _, ok := s.currentUser(r); !ok {
		fail(w, 401, "sign in required")
		return
	}
	target, ok := s.store.user(id)
	if !ok || (!verify && target.Role == "admin") {
		fail(w, 404, "member not found")
		return
	}
	if verify && bcrypt.CompareHashAndPassword([]byte(target.PasswordHash), []byte(current)) != nil {
		fail(w, 403, "current password is incorrect")
		return
	}
	err = s.store.update(func(st *State) error {
		for i := range st.Users {
			if st.Users[i].ID == id {
				st.Users[i].PasswordHash = string(hash)
				return nil
			}
		}
		return fmt.Errorf("user not found")
	})
	if err != nil {
		fail(w, 500, "could not save password")
		return
	}
	s.revokeUser(id)
	event := "password_reset"
	if verify {
		event = "password_changed"
	}
	s.audit(event, actor.Username, "", id)
	jsonOut(w, 200, map[string]bool{"ok": true, "reauthenticate": id == actor.ID})
}
