package control

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/XUJiahua/tunnel-mvp/internal/config"
	"github.com/XUJiahua/tunnel-mvp/internal/state"
	"github.com/XUJiahua/tunnel-mvp/internal/web"
	"github.com/rancher/remotedialer"
	"golang.org/x/crypto/bcrypt"
)

type Server struct {
	history          *historyStore
	oidc             *oidcLogin
	accessMu         sync.RWMutex
	liveMu           sync.Mutex
	live             map[string]liveRequest
	poolMu           sync.Mutex
	pools            map[string]pooledTransport
	monitorMu        sync.Mutex
	reports          map[string]probeReport
	backgroundCancel context.CancelFunc
	backgroundWG     sync.WaitGroup
	metricsToken     string
	stats            counters

	diagnosticMu    sync.Mutex
	diagnosticCache map[string]diagnosticRecord
	cfg             config.Server
	store           *Store
	tunnel          *remotedialer.Server
	auth            *authState
	secure          bool
	dummyHash       string
	public, access  *url.URL
	auditMu         sync.Mutex
	auditFile       *os.File
	tunnelMu        sync.Mutex
	tunnelWG        sync.WaitGroup
	closing         bool
	connections     map[string]*agentConnection
	routes          map[string]uint64
}

func New(c config.Server, password string) (*Server, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	if err := state.CheckOperationsRestore(c.DataDir); err != nil {
		return nil, err
	}
	st, err := openStore(c.DataDir, c.Storage, c.AdminUsername, password)
	if err != nil {
		return nil, err
	}
	af, err := os.OpenFile(filepath.Join(c.DataDir, "audit.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		_ = st.Close()
		return nil, err
	}
	p, _ := url.Parse(c.PublicURL)
	a, _ := url.Parse(c.AccessURL)
	h, _ := bcrypt.GenerateFromPassword([]byte(randomID()), bcrypt.DefaultCost)
	s := &Server{cfg: c, store: st, auth: newAuth(), secure: p.Scheme == "https", public: p, access: a, auditFile: af, dummyHash: string(h), connections: map[string]*agentConnection{}, routes: map[string]uint64{}, live: map[string]liveRequest{}, pools: map[string]pooledTransport{}, reports: map[string]probeReport{}, metricsToken: os.Getenv("TUNNEL_METRICS_TOKEN"), diagnosticCache: map[string]diagnosticRecord{}}
	for _, svc := range st.snapshot().Services {
		if err := s.validateDomain(User{Role: "admin"}, svc.CustomDomain); err != nil {
			af.Close()
			st.Close()
			return nil, err
		}
	}

	history, err := openHistory(c.DataDir)
	if err != nil {
		af.Close()
		st.Close()
		return nil, fmt.Errorf("open monitoring history: %w", err)
	}
	s.history = history
	ready := false
	defer func() {
		if !ready {
			history.close()
		}
	}()
	if err = s.rebuildAuditIndex(); err != nil {
		s.stats.auditIndexErrors.Add(1)
		slog.Error("audit index rebuild failed", "error", err)
	}
	if err = s.loadHealthReports(); err != nil {
		af.Close()
		st.Close()
		return nil, err
	}
	s.tunnel = remotedialer.New(func(r *http.Request) (string, bool, error) {
		id, ok := s.authenticatedTunnel(r)
		return id, ok, nil
	}, func(w http.ResponseWriter, r *http.Request, code int, err error) {
		fail(w, code, "tunnel authentication or handshake failed")
	})
	if len(s.metricsToken) > 0 && len(s.metricsToken) < 32 {
		af.Close()
		_ = st.Close()
		return nil, fmt.Errorf("TUNNEL_METRICS_TOKEN must contain at least 32 bytes")
	}
	if err := s.configureOIDC(); err != nil {
		af.Close()
		_ = st.Close()
		return nil, err
	}
	s.startBackground()
	ready = true
	return s, nil
}
func (s *Server) Close() {
	s.backgroundCancel()
	s.backgroundWG.Wait()
	s.cancelRequests(func(liveRequest) bool { return true })
	s.poolMu.Lock()
	for id, p := range s.pools {
		p.transport.CloseIdleConnections()
		delete(s.pools, id)
	}
	s.poolMu.Unlock()
	s.tunnelMu.Lock()
	s.closing = true
	for _, connection := range s.connections {
		connection.closing.Store(true)
		connection.cancel()
	}
	s.tunnelMu.Unlock()
	s.tunnelWG.Wait()
	s.history.close()
	s.auditMu.Lock()
	defer s.auditMu.Unlock()
	s.auditFile.Close()
	if err := s.store.Close(); err != nil {
		slog.Error("state store close failed", "error", err)
	}
}
func (s *Server) audit(event, user, site, resource string, fields ...map[string]any) {
	org, owned := auditOrganization(s.store.snapshot(), user, site, resource)
	s.auditMu.Lock()
	defer s.auditMu.Unlock()
	info, statErr := s.auditFile.Stat()
	rotating := statErr == nil && info.Size() >= int64(s.cfg.AuditMaxMB)<<20
	if err := s.rotateAudit(); err != nil {
		s.logRotationError(err)
		rotating = true
	}
	if rotating {
		if err := s.rebuildAuditIndex(); err != nil {
			s.stats.auditIndexErrors.Add(1)
			slog.Error("audit index rebuild failed", "error", err)
		}
	}
	entry := map[string]any{"time": time.Now().UTC(), "event": event, "user": user, "siteID": site, "resourceID": resource}
	if owned {
		entry["organizationID"] = org
	}
	for _, values := range fields {
		for key, value := range values {
			entry[key] = value
		}
	}
	err := json.NewEncoder(s.auditFile).Encode(entry)
	if err != nil {
		s.history.auditDirty.Store(true)
		s.stats.auditIndexErrors.Add(1)
		slog.Error("audit write failed", "error", err)
		return
	}
	raw, _ := json.Marshal(entry)
	if err = s.history.indexAudit(raw); err != nil {
		s.history.auditDirty.Store(true)
		s.stats.auditIndexErrors.Add(1)
		slog.Error("audit index write failed", "error", err)
	}
}
func (s *Server) Run(ctx context.Context) error {
	h := &http.Server{Addr: s.cfg.Listen, Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 90 * time.Second, MaxHeaderBytes: 64 << 10}
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			s.tunnelMu.Lock()
			for _, connection := range s.connections {
				connection.closing.Store(true)
				connection.cancel()
			}
			s.tunnelMu.Unlock()
			stop, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_ = h.Shutdown(stop)
		case <-done:
		}
	}()
	slog.Info("control plane ready", "listen", s.cfg.Listen, "console", s.cfg.PublicURL, "access", s.cfg.AccessURL)
	err := h.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/agents", s.require(false, s.listAgents))
	mux.HandleFunc("GET /api/sites/{id}/agents", s.require(false, s.listAgents))
	mux.HandleFunc("POST /api/sites/{id}/agents", s.require(true, s.createAgent))
	mux.HandleFunc("PATCH /api/sites/{id}/agents/{agentID}", s.require(true, s.manageAgent))
	mux.HandleFunc("DELETE /api/sites/{id}/agents/{agentID}", s.require(true, s.manageAgent))
	mux.HandleFunc("POST /api/sites/{id}/agents/{agentID}/rotate", s.require(true, s.manageAgent))
	mux.HandleFunc("GET /api/services/{id}/history", s.require(false, s.serviceHistory))
	mux.HandleFunc("POST /api/services/bulk", s.require(true, s.bulkCreate))
	mux.HandleFunc("POST /api/services/bulk-delete", s.require(true, s.bulkServices))
	mux.HandleFunc("POST /api/services/bulk-move", s.require(true, s.bulkServices))
	mux.HandleFunc("GET /api/group-members", s.require(false, s.groupMembers))
	mux.HandleFunc("GET /api/organizations", s.require(false, s.listOrganizations))
	mux.HandleFunc("POST /api/organizations", s.require(true, s.manageOrganization))
	mux.HandleFunc("PATCH /api/organizations/{id}", s.require(true, s.manageOrganization))
	mux.HandleFunc("DELETE /api/organizations/{id}", s.require(true, s.manageOrganization))
	mux.HandleFunc("GET /api/roles", s.require(false, s.listRoles))
	mux.HandleFunc("POST /api/roles", s.require(true, s.manageRole))
	mux.HandleFunc("PATCH /api/roles/{id}", s.require(true, s.manageRole))
	mux.HandleFunc("DELETE /api/roles/{id}", s.require(true, s.manageRole))
	mux.HandleFunc("PATCH /api/users/{id}/role", s.require(true, s.assignRole))
	mux.HandleFunc("POST /api/administrators", s.require(true, s.manageAdministrator))
	mux.HandleFunc("PATCH /api/administrators/{id}", s.require(true, s.manageAdministrator))
	mux.HandleFunc("DELETE /api/administrators/{id}", s.require(true, s.manageAdministrator))
	mux.HandleFunc("GET /api/invitations", s.require(true, s.listInvites))
	mux.HandleFunc("POST /api/invitations", s.require(true, s.createInvite))
	mux.HandleFunc("DELETE /api/invitations/{id}", s.require(true, s.deleteInvite))
	mux.HandleFunc("POST /api/invitations/inspect", s.inspectInvite)
	mux.HandleFunc("POST /api/invitations/accept", s.acceptInvite)
	mux.HandleFunc("GET /api/groups", s.require(true, s.listGroups))
	mux.HandleFunc("GET /api/me/groups", s.require(false, s.listGroups))
	mux.HandleFunc("POST /api/groups", s.require(true, s.saveGroup))
	mux.HandleFunc("PATCH /api/groups/{id}", s.require(true, s.saveGroup))
	mux.HandleFunc("DELETE /api/groups/{id}", s.require(true, s.deleteGroup))
	mux.HandleFunc("GET /api/me/sessions", s.require(false, s.listSessions))
	mux.HandleFunc("GET /api/sessions", s.require(true, s.listSessions))
	mux.HandleFunc("DELETE /api/me/sessions/{id}", s.require(false, s.deleteSession))
	mux.HandleFunc("DELETE /api/sessions/{id}", s.require(true, s.deleteSession))
	mux.HandleFunc("POST /auth/oidc/backchannel-logout", s.backchannelLogout)
	mux.HandleFunc("GET /api/auth/options", s.authOptions)
	mux.HandleFunc("GET /auth/oidc/start", s.beginOIDC)
	mux.HandleFunc("GET /auth/oidc/callback", s.callbackOIDC)
	mux.HandleFunc("PATCH /api/users/{id}/identity", s.require(true, s.updateIdentity))
	mux.HandleFunc("PATCH /api/users/{id}/disabled", s.require(true, s.setDisabled))
	mux.HandleFunc("POST /api/login", s.login)
	mux.HandleFunc("POST /api/logout", s.logout)
	mux.HandleFunc("POST /api/logout/identity", s.require(false, s.logoutIdentity))
	mux.HandleFunc("POST /api/me/password", s.require(false, s.changePassword))
	mux.HandleFunc("PATCH /api/sites/{id}", s.require(true, s.updateSite))
	mux.HandleFunc("PATCH /api/services/{id}", s.require(true, s.updateService))
	mux.HandleFunc("PATCH /api/users/{id}", s.require(true, s.updateUser))
	mux.HandleFunc("POST /api/users/{id}/password", s.require(true, s.resetPassword))
	mux.HandleFunc("GET /api/status", s.require(false, s.status))
	mux.HandleFunc("GET /api/audit", s.require(true, s.listAudit))
	mux.HandleFunc("GET /api/metrics", s.require(true, s.metrics))
	mux.HandleFunc("GET /api/me", s.require(false, s.profile))
	mux.HandleFunc("GET /api/sites", s.require(false, s.listSites))
	mux.HandleFunc("POST /api/sites", s.require(true, s.createSite))
	mux.HandleFunc("DELETE /api/sites/{id}", s.require(true, s.deleteSite))
	mux.HandleFunc("POST /api/sites/{id}/rotate", s.require(true, s.rotateSite))
	mux.HandleFunc("GET /api/services", s.require(false, s.listServices))
	mux.HandleFunc("POST /api/services", s.require(true, s.createService))
	mux.HandleFunc("DELETE /api/services/{id}", s.require(true, s.deleteService))
	mux.HandleFunc("POST /api/services/{id}/launch", s.require(false, s.launch))
	mux.HandleFunc("POST /api/services/{id}/probe", s.require(false, s.probe))
	mux.HandleFunc("GET /api/users", s.require(true, s.listUsers))
	mux.HandleFunc("POST /api/users", s.require(true, s.createUser))
	mux.HandleFunc("DELETE /api/users/{id}", s.require(true, s.deleteUser))
	files := web.Files()
	static := http.FileServer(http.FS(files))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" && r.Method != "HEAD" {
			http.Error(w, "method not allowed", 405)
			return
		}
		if _, err := fs.Stat(files, "index.html"); err != nil {
			http.Error(w, "frontend not built: run make build", 503)
			return
		}
		static.ServeHTTP(w, r)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.EqualFold(r.Host, s.public.Host) {
			s.gateway(w, r)
			return
		}
		if r.URL.Path == "/tunnel/diagnostics" {
			s.receiveDiagnostic(w, r)
			return
		}
		if r.URL.Path == "/tunnel/connect" {
			s.connect(w, r)
			return
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'")
		if r.URL.Path == "/metrics" {
			if s.metricsToken == "" {
				http.NotFound(w, r)
				return
			}
			if !constantEqual(r.Header.Get("Authorization"), "Bearer "+s.metricsToken) {
				fail(w, 401, "metrics token required")
				return
			}
			s.metrics(w, r, User{})
			return
		}
		if r.URL.Path == "/healthz" {
			jsonOut(w, 200, map[string]string{"status": "ok"})
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			if r.Method != "GET" && r.Method != "HEAD" && r.Header.Get("Origin") != s.cfg.PublicURL {
				fail(w, 403, "invalid request origin")
				return
			}
		}
		mux.ServeHTTP(w, r)
	})
}
func (s *Server) require(admin bool, next func(http.ResponseWriter, *http.Request, User)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// All administrator mutations authenticate inside the same exclusive
		// boundary as role/session revocation, including create operations.
		if admin && r.Method != http.MethodGet && r.Method != http.MethodHead {
			s.accessMu.Lock()
			defer s.accessMu.Unlock()
			r = r.WithContext(context.WithValue(r.Context(), mutationKey{}, true))
		}
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			s.accessMu.RLock()
			defer s.accessMu.RUnlock()
		}
		u, _, ok := s.currentUser(r)
		if !ok {
			fail(w, 401, "sign in required")
			return
		}
		if admin && !hasPermission(u, permissionForRequest(r)) {
			fail(w, 403, "administrator required")
			return
		}
		next(w, r, u)
	}
}
func (s *Server) connect(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" || r.Header.Get("Origin") != "" {
		fail(w, 403, "agent connections only")
		return
	}
	siteID, ok := s.authenticatedAgent(r)
	if !ok {
		fail(w, 401, "invalid site/instance credentials")
		return
	}
	instance := r.Header.Get("X-Tunnel-Instance")
	key := agentKey(siteID, instance)
	site, _ := s.store.site(siteID)
	remote, _, _ := net.SplitHostPort(r.RemoteAddr)
	version := r.Header.Get("X-Tunnel-Version")
	if len(version) > 128 {
		version = version[:128]
	}
	s.tunnelMu.Lock()
	if s.closing {
		s.tunnelMu.Unlock()
		fail(w, 503, "server shutting down")
		return
	}
	if _, exists := s.connections[key]; exists {
		s.tunnelMu.Unlock()
		fail(w, 409, "Agent instance already connected")
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	connection := &agentConnection{key: key, siteID: siteID, instanceID: instance, organizationID: site.OrganizationID, remoteIP: remote, version: version, connectedAt: time.Now().UTC(), cancel: cancel}
	s.connections[key] = connection
	s.tunnelWG.Add(1)
	s.tunnelMu.Unlock()
	defer s.tunnelWG.Done()
	defer func() {
		connection.closing.Store(true)
		cancel()
		s.tunnelMu.Lock()
		delete(s.connections, key)
		s.tunnelMu.Unlock()
		s.dropSiteTransports(siteID)
		s.audit("agent_disconnected", "", siteID, "", map[string]any{"instanceID": instance, "organizationID": site.OrganizationID})
	}()
	s.audit("agent_connected", "", siteID, "", map[string]any{"instanceID": instance, "organizationID": site.OrganizationID})
	s.tunnel.ServeHTTP(&tunnelWriter{ResponseWriter: w, ctx: ctx}, r.WithContext(ctx))
}
func (s *Server) disconnect(id string) {
	s.dropSiteTransports(id)
	s.tunnelMu.Lock()
	for _, c := range s.connections {
		if c.siteID == id {
			c.closing.Store(true)
			c.cancel()
		}
	}
	delete(s.routes, id)
	s.tunnelMu.Unlock()
}

func (s *Server) listSites(w http.ResponseWriter, r *http.Request, u User) {
	out := []map[string]any{}
	state := s.store.snapshot()
	for _, site := range state.Sites {
		visible := canManage(u, site.OrganizationID, "sites:manage") || canManage(u, site.OrganizationID, "applications:manage") || canManage(u, site.OrganizationID, "members:manage") || canManage(u, site.OrganizationID, "groups:manage") || canAccess(u, site.ID)
		for _, svc := range state.Services {
			if svc.SiteID == site.ID && canAccessService(u, svc) {
				visible = true
				break
			}
		}
		if visible {
			out = append(out, map[string]any{"id": site.ID, "name": site.Name, "createdAt": site.CreatedAt, "organizationID": site.OrganizationID, "legacyDisabled": site.LegacyDisabled, "agentCount": len(s.connectedAgents(site.ID)), "online": s.siteOnline(site.ID)})
		}
	}
	jsonOut(w, 200, out)
}
func (s *Server) createSite(w http.ResponseWriter, r *http.Request, u User) {
	var b struct {
		OrganizationID string `json:"organizationID"`
		Name           string `json:"name"`
	}
	if !decode(w, r, &b) {
		return
	}
	b.Name = strings.TrimSpace(b.Name)
	if b.Name == "" || len(b.Name) > 100 {
		fail(w, 400, "name must be 1–100 bytes")
		return
	}
	org, err := selectedOrganization(s.store.snapshot(), u, b.OrganizationID)
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	token := randomID()
	site := Site{OrganizationID: org, ID: randomID()[:16], Name: b.Name, TokenHash: digest(token), CreatedAt: time.Now().UTC()}
	if err := s.store.update(func(st *State) error { st.Sites = append(st.Sites, site); return nil }); err != nil {
		fail(w, 500, "could not save site")
		return
	}
	site.TokenHash = ""
	s.audit("site_created", u.Username, site.ID, "")
	jsonOut(w, 201, map[string]any{"site": site, "token": token})
}
func (s *Server) deleteSite(w http.ResponseWriter, r *http.Request, u User) {
	unlock := s.lockMutation(r)
	defer unlock()
	oldServices := s.store.snapshot().Services
	id := r.PathValue("id")
	removed, _ := s.store.site(id)
	err := s.store.update(func(st *State) error {
		for i, v := range st.Sites {
			if v.ID == id && canManage(u, v.OrganizationID, "sites:manage") {
				st.Sites = append(st.Sites[:i], st.Sites[i+1:]...)
				services := []Service{}
				for _, svc := range st.Services {
					if svc.SiteID != id {
						services = append(services, svc)
					}
				}
				st.Services = services
				for i := range st.Users {
					sites := []string{}
					for _, v := range st.Users[i].SiteIDs {
						if v != id {
							sites = append(sites, v)
						}
					}
					st.Users[i].SiteIDs = sites
					apps := []string{}
					for _, granted := range st.Users[i].ServiceIDs {
						for _, svc := range services {
							if svc.ID == granted {
								apps = append(apps, granted)
								break
							}
						}
					}
					st.Users[i].ServiceIDs = apps
				}
				pruneGrantReferences(st)
				return nil
			}
		}
		return fmt.Errorf("site not found")
	})
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	for _, svc := range oldServices {
		if svc.SiteID == id {
			s.revokeService(svc.ID)
		}
	}
	s.disconnect(id)
	s.audit("site_deleted", u.Username, id, "", map[string]any{"organizationID": removed.OrganizationID})
	jsonOut(w, 200, map[string]bool{"ok": true})
}
func (s *Server) rotateSite(w http.ResponseWriter, r *http.Request, u User) {
	unlock := s.lockMutation(r)
	defer unlock()
	id := r.PathValue("id")
	token := randomID()
	err := s.store.update(func(st *State) error {
		for i, v := range st.Sites {
			if v.ID == id && canManage(u, v.OrganizationID, "sites:manage") {
				st.Sites[i].TokenHash = digest(token)
				for j := range st.Agents {
					if st.Agents[j].SiteID == id {
						st.Agents[j].TokenHash = digest(randomID())
						st.Agents[j].Disabled = true
						st.Agents[j].RequiresRotation = true
					}
				}
				return nil
			}
		}
		return fmt.Errorf("site not found")
	})
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	s.disconnect(id)
	s.audit("site_token_rotated", u.Username, id, "")
	jsonOut(w, 200, map[string]string{"token": token})
}
func (s *Server) listServices(w http.ResponseWriter, r *http.Request, u User) {
	out := []struct {
		Service
		Accessible bool `json:"accessible"`
	}{}
	for _, v := range s.store.snapshot().Services {
		if canAccessService(u, v) || canManage(u, v.OrganizationID, "applications:manage") || canManage(u, v.OrganizationID, "members:manage") || canManage(u, v.OrganizationID, "groups:manage") {
			out = append(out, struct {
				Service
				Accessible bool `json:"accessible"`
			}{v, canAccessService(u, v)})
		}
	}
	jsonOut(w, 200, out)
}
func (s *Server) createService(w http.ResponseWriter, r *http.Request, u User) {
	var b struct {
		Name                 string `json:"name"`
		SiteID               string `json:"siteID"`
		URL                  string `json:"url"`
		CustomDomain         string `json:"customDomain"`
		HealthPath           string `json:"healthPath"`
		HealthMethod         string `json:"healthMethod"`
		HealthExpectedStatus int    `json:"healthExpectedStatus"`
	}
	if !decode(w, r, &b) {
		return
	}
	target, err := config.TargetURL(b.URL)
	if err != nil {
		fail(w, 400, "target must be an HTTP/HTTPS origin or plain mount path without credentials, query or fragment")
		return
	}
	if strings.TrimSpace(b.Name) == "" || len(b.Name) > 100 {
		fail(w, 400, "name must be 1–100 bytes")
		return
	}
	b.CustomDomain = strings.ToLower(strings.TrimSpace(b.CustomDomain))
	if err := s.validateDomain(u, b.CustomDomain); err != nil {
		fail(w, 400, err.Error())
		return
	}
	svc := Service{HealthPath: b.HealthPath, HealthMethod: b.HealthMethod, HealthExpectedStatus: b.HealthExpectedStatus, CustomDomain: b.CustomDomain, ID: randomID()[:16], Name: b.Name, SiteID: b.SiteID, URL: target.String()}
	err = s.store.update(func(st *State) error {
		for _, site := range st.Sites {
			if site.ID == b.SiteID && canManage(u, site.OrganizationID, "applications:manage") {
				svc.OrganizationID = site.OrganizationID
				st.Services = append(st.Services, svc)
				return nil
			}
		}
		return fmt.Errorf("site not found")
	})
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	s.audit("service_created", u.Username, svc.SiteID, svc.ID)
	jsonOut(w, 201, svc)
}
func (s *Server) deleteService(w http.ResponseWriter, r *http.Request, u User) {
	unlock := s.lockMutation(r)
	defer unlock()
	id := r.PathValue("id")
	removed, _ := s.store.service(id)
	err := s.store.update(func(st *State) error {
		for i, v := range st.Services {
			if v.ID == id && canManage(u, v.OrganizationID, "applications:manage") {
				st.Services = append(st.Services[:i], st.Services[i+1:]...)
				for j := range st.Users {
					apps := []string{}
					for _, granted := range st.Users[j].ServiceIDs {
						if granted != id {
							apps = append(apps, granted)
						}
					}
					st.Users[j].ServiceIDs = apps
				}
				pruneGrantReferences(st)
				return nil
			}
		}
		return fmt.Errorf("service not found")
	})
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	s.revokeService(id)
	s.audit("service_deleted", u.Username, "", id, map[string]any{"organizationID": removed.OrganizationID})
	jsonOut(w, 200, map[string]bool{"ok": true})
}
func (s *Server) listUsers(w http.ResponseWriter, r *http.Request, u User) {
	users := []userView{}
	st := s.store.snapshot()
	for _, member := range st.Users {
		if u.Role == "admin" || (member.Role != "admin" && canManage(u, member.OrganizationID, "members:manage")) {
			member.Permissions = rolePermissions(st, member)
			users = append(users, publicUser(member))
		}
	}
	jsonOut(w, 200, users)
}
func (s *Server) createUser(w http.ResponseWriter, r *http.Request, u User) {
	var b struct {
		OrganizationID string   `json:"organizationID"`
		Username       string   `json:"username"`
		Password       string   `json:"password"`
		OIDCSubject    string   `json:"oidcSubject"`
		SiteIDs        []string `json:"siteIDs"`
		ServiceIDs     []string `json:"serviceIDs"`
	}
	if !decode(w, r, &b) {
		return
	}
	b.Username = strings.TrimSpace(b.Username)
	if b.Username == "" || len(b.Username) > 100 {
		fail(w, 400, "username must be 1–100 bytes")
		return
	}
	var hash string
	var identity *state.Identity
	if b.OIDCSubject != "" {
		if s.oidc == nil || b.Password != "" {
			fail(w, 400, "OIDC-only members require configured OIDC and no password")
			return
		}
		identity = &state.Identity{Issuer: s.cfg.OIDC.Issuer, Subject: b.OIDCSubject}
		if err := state.ValidateIdentity(*identity); err != nil {
			fail(w, 400, err.Error())
			return
		}
	} else {
		if len(b.Password) < 12 || len(b.Password) > 72 {
			fail(w, 400, "password must be 12–72 bytes")
			return
		}
		h, err := bcrypt.GenerateFromPassword([]byte(b.Password), bcrypt.DefaultCost)
		if err != nil {
			fail(w, 400, "invalid password")
			return
		}
		hash = string(h)
	}
	if b.SiteIDs == nil {
		b.SiteIDs = []string{}
	}
	if b.ServiceIDs == nil {
		b.ServiceIDs = []string{}
	}
	org, orgErr := selectedOrganization(s.store.snapshot(), u, b.OrganizationID)
	if orgErr != nil {
		fail(w, 400, orgErr.Error())
		return
	}
	user := User{OrganizationID: org, ID: randomID()[:16], Username: b.Username, PasswordHash: hash, Identity: identity, Role: "viewer", SiteIDs: b.SiteIDs, ServiceIDs: b.ServiceIDs}
	err := s.store.update(func(st *State) error {
		for _, v := range st.Users {
			if v.Username == user.Username {
				return fmt.Errorf("username exists")
			}
		}
		if err := validateGrants(st, b.SiteIDs, b.ServiceIDs); err != nil {
			return err
		}
		st.Users = append(st.Users, user)
		return nil
	})
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	s.audit("user_created", u.Username, "", user.ID)
	jsonOut(w, 201, publicUser(user))
}
func (s *Server) deleteUser(w http.ResponseWriter, r *http.Request, u User) {
	unlock := s.lockMutation(r)
	defer unlock()
	id := r.PathValue("id")
	removed, _ := s.store.user(id)
	if id == u.ID {
		fail(w, 400, "cannot delete yourself")
		return
	}
	err := s.store.update(func(st *State) error {
		for i, v := range st.Users {
			if v.ID == id && v.Role != "admin" && canManageMember(u, v) {
				st.Users = append(st.Users[:i], st.Users[i+1:]...)
				pruneGrantReferences(st)
				return nil
			}
		}
		return fmt.Errorf("viewer not found")
	})
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	s.revokeUser(id)
	s.audit("user_deleted", u.Username, "", id, map[string]any{"organizationID": removed.OrganizationID})
	jsonOut(w, 200, map[string]bool{"ok": true})
}

// remotedialer blocks reading its WebSocket and does not close it on context
// cancellation. Capture the hijacked socket so revocation closes the wire too.
type tunnelWriter struct {
	http.ResponseWriter
	ctx context.Context
}

func (w *tunnelWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("hijacking unavailable")
	}
	c, rw, err := h.Hijack()
	if err != nil {
		return nil, nil, err
	}
	go func() { <-w.ctx.Done(); _ = c.Close() }()
	return c, rw, nil
}
