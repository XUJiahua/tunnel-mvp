package control

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/XUJiahua/tunnel-mvp/internal/config"
)

type serviceInput struct {
	Name                 string `json:"name"`
	SiteID               string `json:"siteID"`
	URL                  string `json:"url"`
	CustomDomain         string `json:"customDomain"`
	ConnectionMode       string `json:"connectionMode"`
	HealthPath           string `json:"healthPath"`
	HealthMethod         string `json:"healthMethod"`
	HealthExpectedStatus int    `json:"healthExpectedStatus"`
}

func (s *Server) bulkCreate(w http.ResponseWriter, r *http.Request, actor User) {
	var body struct {
		Services []serviceInput `json:"services"`
	}
	if !decode(w, r, &body) {
		return
	}
	if len(body.Services) < 1 || len(body.Services) > 100 {
		fail(w, 400, "1–100 applications required")
		return
	}
	out := []Service{}
	err := s.updateAuthorized(r, actor, func(st *State) error {
		for _, in := range body.Services {
			in.Name = strings.TrimSpace(in.Name)
			if in.Name == "" || len(in.Name) > 100 {
				return fmt.Errorf("name must be 1–100 bytes")
			}
			target, err := config.TargetURL(in.URL)
			if err != nil {
				return err
			}
			in.CustomDomain = strings.ToLower(strings.TrimSpace(in.CustomDomain))
			if err = s.validateDomain(actor, in.CustomDomain); err != nil {
				return err
			}
			found := false
			for _, site := range st.Sites {
				if site.ID == in.SiteID && canManage(actor, site.OrganizationID, "applications:manage") {
					svc := Service{ConnectionMode: in.ConnectionMode, HealthPath: in.HealthPath, HealthMethod: in.HealthMethod, HealthExpectedStatus: in.HealthExpectedStatus, ID: randomID()[:16], OrganizationID: site.OrganizationID, Name: in.Name, SiteID: site.ID, URL: target.String(), CustomDomain: in.CustomDomain}
					st.Services = append(st.Services, svc)
					out = append(out, svc)
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("site not found")
			}
		}
		return nil
	})
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	for _, svc := range out {
		s.audit("service_created", actor.Username, svc.SiteID, svc.ID, map[string]any{"bulk": true})
	}
	jsonOut(w, 201, out)
}
func (s *Server) bulkServices(w http.ResponseWriter, r *http.Request, actor User) {
	var body struct {
		IDs    []string `json:"ids"`
		SiteID string   `json:"siteID"`
	}
	if !decode(w, r, &body) {
		return
	}
	ids := map[string]bool{}
	for _, id := range body.IDs {
		if ids[id] || id == "" {
			fail(w, 400, "unique application IDs required")
			return
		}
		ids[id] = true
	}
	if len(ids) < 1 || len(ids) > 100 {
		fail(w, 400, "1–100 applications required")
		return
	}
	move := strings.HasSuffix(r.URL.Path, "/bulk-move")
	if !move && body.SiteID != "" {
		fail(w, 400, "destination site is not accepted for deletion")
		return
	}
	changed := []Service{}
	err := s.updateAuthorized(r, actor, func(st *State) error {
		var destination Site
		found := false
		if move {
			for _, site := range st.Sites {
				if site.ID == body.SiteID && canManage(actor, site.OrganizationID, "applications:manage") {
					destination = site
					found = true
				}
			}
			if !found {
				return fmt.Errorf("destination site not found")
			}
		}
		kept := []Service{}
		for _, svc := range st.Services {
			if !ids[svc.ID] {
				kept = append(kept, svc)
				continue
			}
			if !canManage(actor, svc.OrganizationID, "applications:manage") || move && svc.OrganizationID != destination.OrganizationID {
				return fmt.Errorf("application not found in destination organization")
			}
			changed = append(changed, svc)
			if move {
				svc.SiteID = destination.ID
				kept = append(kept, svc)
			}
		}
		if len(changed) != len(ids) {
			return fmt.Errorf("application not found")
		}
		st.Services = kept
		if !move {
			pruneGrantReferences(st)
		}
		return nil
	})
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	event := "service_deleted"
	if move {
		event = "service_moved"
	}
	for _, svc := range changed {
		s.revokeService(svc.ID)
		s.audit(event, actor.Username, svc.SiteID, svc.ID, map[string]any{"organizationID": svc.OrganizationID, "bulk": true, "destinationSiteID": body.SiteID})
	}
	jsonOut(w, 200, map[string]int{"changed": len(changed)})
}
