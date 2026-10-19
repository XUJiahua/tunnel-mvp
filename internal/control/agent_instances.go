package control

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/XUJiahua/tunnel-mvp/internal/state"
)

type agentConnection struct {
	key, siteID, instanceID, organizationID, remoteIP, version string
	connectedAt                                                time.Time
	cancel                                                     context.CancelFunc
	closing                                                    atomic.Bool
	channels                                                   atomic.Int64
}

func agentKey(site, instance string) string {
	if instance == "" {
		return site
	}
	return site + "~" + instance
}
func (s *Store) agent(id string) (state.AgentRegistration, bool) {
	for _, a := range s.snapshot().Agents {
		if a.ID == id {
			return a, true
		}
	}
	return state.AgentRegistration{}, false
}
func (s *Server) authenticatedTunnel(r *http.Request) (string, bool) {
	site, ok := s.authenticatedAgent(r)
	return agentKey(site, r.Header.Get("X-Tunnel-Instance")), ok
}
func (s *Server) connectedAgents(siteID string) []*agentConnection {
	s.tunnelMu.Lock()
	out := []*agentConnection{}
	for _, c := range s.connections {
		if c.siteID == siteID && !c.closing.Load() {
			out = append(out, c)
		}
	}
	s.tunnelMu.Unlock()
	live := out[:0]
	for _, c := range out {
		if s.tunnel.HasSession(c.key) {
			live = append(live, c)
		}
	}
	sort.Slice(live, func(i, j int) bool { return live[i].key < live[j].key })
	return live
}
func (s *Server) siteOnline(id string) bool { return len(s.connectedAgents(id)) > 0 }
func (s *Server) disconnectAgent(site, instance string) {
	key := agentKey(site, instance)
	s.tunnelMu.Lock()
	if c := s.connections[key]; c != nil {
		c.closing.Store(true)
		c.cancel()
	}
	s.tunnelMu.Unlock()
	s.dropSiteTransports(site)
}

type agentChannel struct {
	net.Conn
	agent *agentConnection
	once  sync.Once
}

func (c *agentChannel) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { c.agent.channels.Add(-1) })
	return err
}
func (s *Server) dialSite(ctx context.Context, site, service, network, address string) (net.Conn, error) {
	candidates := s.connectedAgents(site)
	if len(candidates) == 0 {
		return nil, fmt.Errorf("site Agent is offline")
	}
	s.tunnelMu.Lock()
	offset := s.routes[site]
	s.routes[site]++
	s.tunnelMu.Unlock()
	var last error
	for i := 0; i < len(candidates); i++ {
		c := candidates[(int(offset%uint64(len(candidates)))+i)%len(candidates)]
		if c.closing.Load() {
			continue
		}
		conn, err := s.tunnel.Dialer(c.key)(ctx, network, address)
		if err != nil {
			last = err
			continue
		}
		c.channels.Add(1)
		s.audit("agent_channel_opened", "", site, service, map[string]any{"instanceID": c.instanceID, "organizationID": c.organizationID, "target": address})
		return &agentChannel{Conn: conn, agent: c}, nil
	}
	if last == nil {
		last = fmt.Errorf("site Agent is offline")
	}
	return nil, last
}

type agentView struct {
	RequiresRotation bool       `json:"requiresRotation"`
	ID               string     `json:"id"`
	SiteID           string     `json:"siteID"`
	Name             string     `json:"name"`
	Disabled         bool       `json:"disabled"`
	Legacy           bool       `json:"legacy"`
	Online           bool       `json:"online"`
	CreatedAt        time.Time  `json:"createdAt"`
	ConnectedAt      *time.Time `json:"connectedAt,omitempty"`
	ActiveChannels   int64      `json:"activeChannels"`
	RemoteIP         string     `json:"remoteIP,omitempty"`
	ReportedVersion  string     `json:"reportedVersion,omitempty"`
}

func (s *Server) agentView(a state.AgentRegistration) agentView {
	v := agentView{RequiresRotation: a.RequiresRotation, ID: a.ID, SiteID: a.SiteID, Name: a.Name, Disabled: a.Disabled, CreatedAt: a.CreatedAt}
	if a.Disabled {
		return v
	}
	for _, c := range s.connectedAgents(a.SiteID) {
		if c.instanceID == a.ID {
			v.Online = true
			t := c.connectedAt
			v.ConnectedAt = &t
			v.ActiveChannels = c.channels.Load()
			v.RemoteIP = c.remoteIP
			v.ReportedVersion = c.version
			break
		}
	}
	return v
}
func (s *Server) listAgents(w http.ResponseWriter, r *http.Request, u User) {
	if !hasPermission(u, "sites:manage") {
		fail(w, 403, "site management permission required")
		return
	}
	st := s.store.snapshot()
	visible := map[string]Site{}
	for _, site := range st.Sites {
		if (r.PathValue("id") == "" || site.ID == r.PathValue("id")) && canManage(u, site.OrganizationID, "sites:manage") {
			visible[site.ID] = site
		}
	}
	if r.PathValue("id") != "" && len(visible) == 0 {
		fail(w, 404, "site not found")
		return
	}
	out := []agentView{}
	for _, a := range st.Agents {
		if _, ok := visible[a.SiteID]; ok {
			out = append(out, s.agentView(a))
		}
	}
	for _, site := range visible {
		for _, c := range s.connectedAgents(site.ID) {
			if c.instanceID == "" {
				t := c.connectedAt
				out = append(out, agentView{ID: "legacy", SiteID: site.ID, Name: "兼容站点令牌实例", Legacy: true, Online: true, CreatedAt: site.CreatedAt, ConnectedAt: &t, ActiveChannels: c.channels.Load(), RemoteIP: c.remoteIP, ReportedVersion: c.version})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].SiteID == out[j].SiteID {
			return out[i].ID < out[j].ID
		}
		return out[i].SiteID < out[j].SiteID
	})
	jsonOut(w, 200, out)
}
func (s *Server) createAgent(w http.ResponseWriter, r *http.Request, u User) {
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
	siteID := r.PathValue("id")
	site, ok := s.store.site(siteID)
	if !ok || !canManage(u, site.OrganizationID, "sites:manage") {
		fail(w, 404, "site not found")
		return
	}
	token := randomID()
	a := state.AgentRegistration{ID: randomID()[:16], SiteID: siteID, Name: body.Name, TokenHash: digest(token), CreatedAt: time.Now().UTC()}
	err := s.store.update(func(st *State) error {
		count := 0
		for _, a := range st.Agents {
			if a.SiteID == siteID {
				count++
			}
		}
		if count >= 16 {
			return fmt.Errorf("at most 16 registered Agents per site")
		}
		st.Agents = append(st.Agents, a)
		return nil
	})
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	s.audit("agent_registered", u.Username, siteID, a.ID)
	jsonOut(w, 201, map[string]any{"agent": s.agentView(a), "token": token})
}
func (s *Server) manageAgent(w http.ResponseWriter, r *http.Request, u User) {
	siteID, id := r.PathValue("id"), r.PathValue("agentID")
	site, ok := s.store.site(siteID)
	if !ok || !canManage(u, site.OrganizationID, "sites:manage") {
		fail(w, 404, "site not found")
		return
	}
	if id == "legacy" {
		fail(w, 400, "legacy mode is managed by site legacyDisabled/token rotation")
		return
	}
	var body struct {
		Name     *string `json:"name"`
		Disabled *bool   `json:"disabled"`
	}
	rotate := strings.HasSuffix(r.URL.Path, "/rotate")
	if r.Method == "PATCH" {
		if !decode(w, r, &body) {
			return
		}
		if body.Name == nil && body.Disabled == nil {
			fail(w, 400, "name or disabled required")
			return
		}
		if body.Name != nil {
			v := strings.TrimSpace(*body.Name)
			if v == "" || len(v) > 100 {
				fail(w, 400, "name must be 1–100 bytes")
				return
			}
			body.Name = &v
		}
	}
	token := ""
	if rotate {
		token = randomID()
	}
	var out state.AgentRegistration
	disconnect := rotate || r.Method == "DELETE"
	err := s.store.update(func(st *State) error {
		for i := range st.Agents {
			a := &st.Agents[i]
			if a.ID != id || a.SiteID != siteID {
				continue
			}
			if r.Method == "DELETE" {
				out = *a
				st.Agents = append(st.Agents[:i], st.Agents[i+1:]...)
				return nil
			}
			if rotate {
				a.TokenHash = digest(token)
				a.Disabled = false
				a.RequiresRotation = false
			}
			if body.Name != nil {
				a.Name = *body.Name
			}
			if body.Disabled != nil {
				if !*body.Disabled && a.RequiresRotation {
					return fmt.Errorf("rotate this instance credential after site token rotation")
				}
				a.Disabled = *body.Disabled
				disconnect = disconnect || a.Disabled
			}
			out = *a
			return nil
		}
		return fmt.Errorf("registered Agent not found")
	})
	if err != nil {
		fail(w, 404, err.Error())
		return
	}
	if disconnect {
		s.disconnectAgent(siteID, id)
	}
	event := "agent_updated"
	if rotate {
		event = "agent_token_rotated"
	} else if r.Method == "DELETE" {
		event = "agent_deleted"
	}
	s.audit(event, u.Username, siteID, id)
	if rotate {
		jsonOut(w, 200, map[string]any{"agent": s.agentView(out), "token": token})
	} else {
		jsonOut(w, 200, s.agentView(out))
	}
}
