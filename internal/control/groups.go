package control

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/XUJiahua/tunnel-mvp/internal/state"
)

type Group = state.Group

func containsID(ids []string, id string) bool {
	for _, v := range ids {
		if v == id {
			return true
		}
	}
	return false
}
func effectiveUser(st State, u User) User {
	u.Permissions = rolePermissions(st, u)
	u.SiteIDs = append([]string{}, u.SiteIDs...)
	u.ServiceIDs = append([]string{}, u.ServiceIDs...)
	for _, g := range st.Groups {
		if containsID(g.MemberIDs, u.ID) {
			u.SiteIDs = append(u.SiteIDs, g.SiteIDs...)
			u.ServiceIDs = append(u.ServiceIDs, g.ServiceIDs...)
		}
	}
	return u
}
func (s *Server) cancelUnauthorizedRequests() {
	st := s.store.snapshot()
	users := map[string]User{}
	services := map[string]Service{}
	for _, u := range st.Users {
		users[u.ID] = effectiveUser(st, u)
	}
	for _, v := range st.Services {
		services[v.ID] = v
	}
	s.cancelRequests(func(req liveRequest) bool {
		u, exists := users[req.userID]
		v, found := services[req.serviceID]
		return !exists || !found || !canAccessService(u, v)
	})
}
func (s *Server) listGroups(w http.ResponseWriter, r *http.Request, actor User) {
	out := []Group{}
	filter := directoryFilter(actor)
	filter.All = hasPermission(actor, "groups:manage")
	filter.MemberID = actor.ID
	st, ok := s.directory(w, r, "groups", filter)
	if !ok {
		return
	}
	for _, g := range st.Groups {
		if canManage(actor, g.OrganizationID, "groups:manage") || containsID(g.MemberIDs, actor.ID) {
			if !canManage(actor, g.OrganizationID, "groups:manage") {
				g.MemberIDs = nil
			}
			out = append(out, g)
		}
	}
	jsonOut(w, 200, out)
}
func (s *Server) saveGroup(w http.ResponseWriter, r *http.Request, actor User) {
	var body struct {
		Name           string   `json:"name"`
		OrganizationID string   `json:"organizationID"`
		MemberIDs      []string `json:"memberIDs"`
		SiteIDs        []string `json:"siteIDs"`
		ServiceIDs     []string `json:"serviceIDs"`
	}
	if !decode(w, r, &body) {
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	if body.Name == "" || len(body.Name) > 100 {
		fail(w, 400, "name must be 1–100 bytes")
		return
	}
	unlock := s.lockMutation(r)
	defer unlock()
	if current, _, ok := s.currentUser(r); !ok || !hasPermission(current, "groups:manage") {
		fail(w, 401, "administrator sign in required")
		return
	}
	create := r.Method == "POST"
	id := r.PathValue("id")
	if create {
		id = randomID()[:16]
	}
	org, orgErr := selectedOrganization(s.store.snapshot(), actor, body.OrganizationID)
	if orgErr != nil {
		fail(w, 400, orgErr.Error())
		return
	}
	out := Group{OrganizationID: org, ID: id, Name: body.Name, MemberIDs: body.MemberIDs, SiteIDs: body.SiteIDs, ServiceIDs: body.ServiceIDs}
	err := s.store.update(func(st *State) error {
		if err := validateGrants(st, body.SiteIDs, body.ServiceIDs); err != nil {
			return err
		}
		for _, id := range body.MemberIDs {
			found := false
			for _, u := range st.Users {
				if u.ID == id {
					found = true
				}
			}
			if !found {
				return fmt.Errorf("unknown member")
			}
		}
		if create {
			st.Groups = append(st.Groups, out)
			return nil
		}
		for i := range st.Groups {
			if st.Groups[i].ID == id && canManage(actor, st.Groups[i].OrganizationID, "groups:manage") {
				out.OrganizationID = st.Groups[i].OrganizationID
				st.Groups[i] = out
				return nil
			}
		}
		return fmt.Errorf("group not found")
	})
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	s.cancelUnauthorizedRequests()
	event, code := "group_updated", 200
	if create {
		event, code = "group_created", 201
	}
	s.audit(event, actor.Username, "", id)
	jsonOut(w, code, out)
}
func (s *Server) deleteGroup(w http.ResponseWriter, r *http.Request, actor User) {
	unlock := s.lockMutation(r)
	defer unlock()
	if current, _, ok := s.currentUser(r); !ok || !hasPermission(current, "groups:manage") {
		fail(w, 401, "administrator sign in required")
		return
	}
	id := r.PathValue("id")
	org := ""
	err := s.store.update(func(st *State) error {
		for i, g := range st.Groups {
			if g.ID == id && canManage(actor, g.OrganizationID, "groups:manage") {
				org = g.OrganizationID
				st.Groups = append(st.Groups[:i], st.Groups[i+1:]...)
				for j := range st.Invites {
					st.Invites[j].GroupIDs = withoutID(st.Invites[j].GroupIDs, id)
				}
				return nil
			}
		}
		return fmt.Errorf("group not found")
	})
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	s.cancelUnauthorizedRequests()
	s.audit("group_deleted", actor.Username, "", id, map[string]any{"organizationID": org})
	jsonOut(w, 200, map[string]bool{"ok": true})
}
func withoutID(ids []string, id string) []string {
	out := []string{}
	for _, v := range ids {
		if v != id {
			out = append(out, v)
		}
	}
	return out
}

// Resource deletion must prune all grant owners in the same state transaction.
func pruneGrantReferences(st *State) {
	users, sites, services, groups := map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, v := range st.Users {
		users[v.ID] = true
	}
	for _, v := range st.Sites {
		sites[v.ID] = true
	}
	for _, v := range st.Services {
		services[v.ID] = true
	}
	for _, v := range st.Groups {
		groups[v.ID] = true
	}
	filter := func(ids []string, known map[string]bool) []string {
		out := []string{}
		for _, id := range ids {
			if known[id] {
				out = append(out, id)
			}
		}
		return out
	}
	agents := []state.AgentRegistration{}
	for _, a := range st.Agents {
		if sites[a.SiteID] {
			agents = append(agents, a)
		}
	}
	if len(st.Agents) > 0 {
		st.Agents = agents
	}
	for i := range st.Users {
		st.Users[i].SiteIDs = filter(st.Users[i].SiteIDs, sites)
		st.Users[i].ServiceIDs = filter(st.Users[i].ServiceIDs, services)
	}
	for i := range st.Groups {
		g := &st.Groups[i]
		g.MemberIDs = filter(g.MemberIDs, users)
		g.SiteIDs = filter(g.SiteIDs, sites)
		g.ServiceIDs = filter(g.ServiceIDs, services)
	}
	for i := range st.Invites {
		v := &st.Invites[i]
		v.SiteIDs = filter(v.SiteIDs, sites)
		v.ServiceIDs = filter(v.ServiceIDs, services)
		v.GroupIDs = filter(v.GroupIDs, groups)
	}
}
