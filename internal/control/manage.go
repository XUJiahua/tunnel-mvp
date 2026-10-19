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
			if st.Sites[i].ID == r.PathValue("id") && canManage(u, st.Sites[i].OrganizationID, "sites:manage") {
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
		Name                 string  `json:"name"`
		SiteID               *string `json:"siteID"`
		CustomDomain         *string `json:"customDomain"`
		HealthPath           *string `json:"healthPath"`
		HealthMethod         *string `json:"healthMethod"`
		HealthExpectedStatus *int    `json:"healthExpectedStatus"`
		URL                  string  `json:"url"`
	}
	if !decode(w, r, &body) {
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	target, err := config.TargetURL(body.URL)
	if err != nil || body.Name == "" || len(body.Name) > 100 {
		fail(w, 400, "name and valid HTTP/HTTPS target required")
		return
	}
	unlock := s.lockMutation(r)
	defer unlock()
	var out Service
	changed := false
	healthChanged := false
	err = s.store.update(func(st *State) error {
		for i := range st.Services {
			if st.Services[i].ID == r.PathValue("id") && canManage(u, st.Services[i].OrganizationID, "applications:manage") {
				svc := &st.Services[i]
				if body.SiteID != nil && *body.SiteID != svc.SiteID {
					found := false
					for _, site := range st.Sites {
						if site.ID == *body.SiteID && site.OrganizationID == svc.OrganizationID && canManage(u, site.OrganizationID, "applications:manage") {
							found = true
						}
					}
					if !found {
						return fmt.Errorf("destination site not found in application organization")
					}
					svc.SiteID = *body.SiteID
					changed = true
				}
				if body.CustomDomain != nil {
					domain := strings.ToLower(strings.TrimSpace(*body.CustomDomain))
					if domain != svc.CustomDomain {
						if u.Role != "admin" {
							return fmt.Errorf("custom domains require a global administrator")
						}
						if err := s.validateDomain(u, domain); err != nil {
							return err
						}
						svc.CustomDomain = domain
						changed = true
					}
				}
				beforePath, beforeMethod, beforeStatus := svc.HealthPath, svc.HealthMethod, svc.HealthExpectedStatus
				if body.HealthPath != nil {
					svc.HealthPath = *body.HealthPath
				}
				if body.HealthMethod != nil {
					svc.HealthMethod = *body.HealthMethod
				}
				if body.HealthExpectedStatus != nil {
					svc.HealthExpectedStatus = *body.HealthExpectedStatus
				}
				if beforeMethod == "" {
					beforeMethod = "HEAD"
				}
				healthChanged = beforePath != svc.HealthPath || beforeMethod != probeMethod(*svc) || beforeStatus != svc.HealthExpectedStatus
				changed = changed || svc.URL != target.String()
				svc.Name, svc.URL = body.Name, target.String()
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
	} else if healthChanged {
		s.monitorMu.Lock()
		delete(s.reports, out.ID)
		s.monitorMu.Unlock()
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
	unlock := s.lockMutation(r)
	defer unlock()
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
			if st.Users[i].ID == r.PathValue("id") && st.Users[i].Role != "admin" && canManageMember(actor, st.Users[i]) {
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
	// Direct and group grants are additive; only close genuinely lost access.
	s.cancelUnauthorizedRequests()
	s.audit("user_grants_updated", actor.Username, "", out.ID)
	jsonOut(w, 200, publicUser(out))
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
	unlock := s.lockMutation(r)
	defer unlock()
	// Reject requests authorized before a concurrent password change/logout.
	if _, _, ok := s.currentUser(r); !ok {
		fail(w, 401, "sign in required")
		return
	}
	target, ok := s.store.user(id)
	if !ok || (!verify && (target.Role == "admin" || !canManageMember(actor, target))) {
		fail(w, 404, "member not found")
		return
	}
	if target.PasswordHash == "" {
		fail(w, 400, "OIDC-only members manage passwords at their identity provider")
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
