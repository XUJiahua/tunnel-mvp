package control

import (
	"fmt"
	"github.com/XUJiahua/tunnel-mvp/internal/state"
	"net/http"
)

func (s *Server) receivePolicy(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != "GET" || r.Header.Get("Origin") != "" {
		fail(w, 403, "Agent policy requests only")
		return
	}
	s.accessMu.RLock()
	defer s.accessMu.RUnlock()
	_, ok := s.authenticatedAgent(r)
	if !ok || r.Header.Get("X-Tunnel-Instance") == "" {
		fail(w, 401, "registered Agent credentials required")
		return
	}
	a, _ := s.store.agent(r.Header.Get("X-Tunnel-Instance"))
	p := a.Policy
	if p == nil {
		empty := state.EmptyAgentPolicy()
		p = &empty
	}
	jsonOut(w, 200, p)
}
func (s *Server) manageAgentPolicy(w http.ResponseWriter, r *http.Request, u User) {
	siteID, id := r.PathValue("id"), r.PathValue("agentID")
	site, ok := s.store.site(siteID)
	if !ok || !canManage(u, site.OrganizationID, "sites:manage") {
		fail(w, 404, "site not found")
		return
	}
	a, ok := s.store.agent(id)
	if !ok || a.SiteID != siteID {
		fail(w, 404, "registered Agent not found")
		return
	}
	if r.Method == "GET" {
		if a.Policy == nil {
			p := state.EmptyAgentPolicy()
			jsonOut(w, 200, p)
		} else {
			jsonOut(w, 200, a.Policy)
		}
		return
	}
	var body struct {
		Allow []state.NetworkRule `json:"allow"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.Allow == nil {
		fail(w, 400, "allow must be an array; empty denies all")
		return
	}
	p := state.AgentPolicy{Version: state.PolicyVersion(body.Allow), Allow: body.Allow}
	if err := state.ValidateAgentPolicy(&p); err != nil {
		fail(w, 400, err.Error())
		return
	}
	err := s.updateAuthorized(r, u, func(st *State) error {
		for i := range st.Agents {
			if st.Agents[i].ID == id && st.Agents[i].SiteID == siteID {
				st.Agents[i].Policy = &p
				return nil
			}
		}
		return fmt.Errorf("registered Agent not found")
	})
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	s.audit("agent_policy_updated", u.Username, siteID, id, map[string]any{"version": p.Version, "ruleCount": len(p.Allow)})
	jsonOut(w, 200, p)
}
