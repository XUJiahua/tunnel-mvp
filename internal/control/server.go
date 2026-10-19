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
	"github.com/XUJiahua/tunnel-mvp/internal/web"
	"github.com/rancher/remotedialer"
	"golang.org/x/crypto/bcrypt"
)

type Server struct {
	cfg            config.Server
	store          *Store
	tunnel         *remotedialer.Server
	auth           *authState
	secure         bool
	dummyHash      string
	public, access *url.URL
	auditMu        sync.Mutex
	auditFile      *os.File
	tunnelMu       sync.Mutex
	connections    map[string]context.CancelFunc
}

func New(c config.Server, password string) (*Server, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	st, err := OpenStore(c.DataDir, c.AdminUsername, password)
	if err != nil {
		return nil, err
	}
	af, err := os.OpenFile(filepath.Join(c.DataDir, "audit.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	p, _ := url.Parse(c.PublicURL)
	a, _ := url.Parse(c.AccessURL)
	h, _ := bcrypt.GenerateFromPassword([]byte(randomID()), bcrypt.DefaultCost)
	s := &Server{cfg: c, store: st, auth: newAuth(), secure: p.Scheme == "https", public: p, access: a, auditFile: af, dummyHash: string(h), connections: map[string]context.CancelFunc{}}
	s.tunnel = remotedialer.New(func(r *http.Request) (string, bool, error) {
		id := r.Header.Get("X-Tunnel-Site")
		site, ok := s.store.site(id)
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		return id, ok && constantEqual(site.TokenHash, digest(token)), nil
	}, func(w http.ResponseWriter, r *http.Request, code int, err error) {
		fail(w, code, "tunnel authentication or handshake failed")
	})
	return s, nil
}
func (s *Server) Close() {
	s.tunnelMu.Lock()
	for _, cancel := range s.connections {
		cancel()
	}
	s.tunnelMu.Unlock()
	s.auditFile.Close()
}
func (s *Server) audit(event, user, site, resource string) {
	s.auditMu.Lock()
	defer s.auditMu.Unlock()
	err := json.NewEncoder(s.auditFile).Encode(map[string]any{"time": time.Now().UTC(), "event": event, "user": user, "siteID": site, "resourceID": resource})
	if err != nil {
		slog.Error("audit write failed", "error", err)
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
			for _, cancel := range s.connections {
				cancel()
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
	mux.HandleFunc("POST /api/login", s.login)
	mux.HandleFunc("POST /api/logout", func(w http.ResponseWriter, r *http.Request) {
		_, key, _ := s.currentUser(r)
		s.auth.mu.Lock()
		delete(s.auth.sessions, key)
		s.auth.mu.Unlock()
		http.SetCookie(w, &http.Cookie{Name: consoleCookie, Path: "/", MaxAge: -1, HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteLaxMode})
		jsonOut(w, 200, map[string]bool{"ok": true})
	})
	mux.HandleFunc("GET /api/me", s.require(false, func(w http.ResponseWriter, r *http.Request, u User) { u.PasswordHash = ""; jsonOut(w, 200, u) }))
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
		if r.URL.Path == "/tunnel/connect" {
			s.connect(w, r)
			return
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'")
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
		u, _, ok := s.currentUser(r)
		if !ok {
			fail(w, 401, "sign in required")
			return
		}
		if admin && u.Role != "admin" {
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
	id := r.Header.Get("X-Tunnel-Site")
	site, ok := s.store.site(id)
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || !constantEqual(site.TokenHash, digest(token)) {
		fail(w, 401, "invalid site credentials")
		return
	}
	s.tunnelMu.Lock()
	if _, exists := s.connections[id]; exists {
		s.tunnelMu.Unlock()
		fail(w, 409, "site already connected")
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	s.connections[id] = cancel
	s.tunnelMu.Unlock()
	defer func() {
		cancel()
		s.tunnelMu.Lock()
		delete(s.connections, id)
		s.tunnelMu.Unlock()
		s.audit("agent_disconnected", "", id, "")
	}()
	s.audit("agent_connected", "", id, "")
	s.tunnel.ServeHTTP(&tunnelWriter{ResponseWriter: w, ctx: ctx}, r.WithContext(ctx))
}
func (s *Server) disconnect(id string) {
	s.tunnelMu.Lock()
	if cancel := s.connections[id]; cancel != nil {
		cancel()
	}
	s.tunnelMu.Unlock()
}
func (s *Server) listSites(w http.ResponseWriter, r *http.Request, u User) {
	out := []map[string]any{}
	for _, site := range s.store.snapshot().Sites {
		if canAccess(u, site.ID) {
			out = append(out, map[string]any{"id": site.ID, "name": site.Name, "createdAt": site.CreatedAt, "online": s.tunnel.HasSession(site.ID)})
		}
	}
	jsonOut(w, 200, out)
}
func (s *Server) createSite(w http.ResponseWriter, r *http.Request, u User) {
	var b struct {
		Name string `json:"name"`
	}
	if !decode(w, r, &b) {
		return
	}
	b.Name = strings.TrimSpace(b.Name)
	if b.Name == "" || len(b.Name) > 100 {
		fail(w, 400, "name must be 1–100 bytes")
		return
	}
	token := randomID()
	site := Site{ID: randomID()[:16], Name: b.Name, TokenHash: digest(token), CreatedAt: time.Now().UTC()}
	if err := s.store.update(func(st *State) error { st.Sites = append(st.Sites, site); return nil }); err != nil {
		fail(w, 500, "could not save site")
		return
	}
	site.TokenHash = ""
	s.audit("site_created", u.Username, site.ID, "")
	jsonOut(w, 201, map[string]any{"site": site, "token": token})
}
func (s *Server) deleteSite(w http.ResponseWriter, r *http.Request, u User) {
	id := r.PathValue("id")
	err := s.store.update(func(st *State) error {
		for i, v := range st.Sites {
			if v.ID == id {
				st.Sites = append(st.Sites[:i], st.Sites[i+1:]...)
				services := []Service{}
				for _, svc := range st.Services {
					if svc.SiteID != id {
						services = append(services, svc)
					}
				}
				st.Services = services
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
	s.audit("site_deleted", u.Username, id, "")
	jsonOut(w, 200, map[string]bool{"ok": true})
}
func (s *Server) rotateSite(w http.ResponseWriter, r *http.Request, u User) {
	id := r.PathValue("id")
	token := randomID()
	err := s.store.update(func(st *State) error {
		for i, v := range st.Sites {
			if v.ID == id {
				st.Sites[i].TokenHash = digest(token)
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
	out := []Service{}
	for _, v := range s.store.snapshot().Services {
		if canAccess(u, v.SiteID) {
			out = append(out, v)
		}
	}
	jsonOut(w, 200, out)
}
func (s *Server) createService(w http.ResponseWriter, r *http.Request, u User) {
	var b struct {
		Name   string `json:"name"`
		SiteID string `json:"siteID"`
		URL    string `json:"url"`
	}
	if !decode(w, r, &b) {
		return
	}
	target, err := config.Origin(b.URL)
	if err != nil {
		fail(w, 400, "target must be an http(s) origin without path or credentials")
		return
	}
	if strings.TrimSpace(b.Name) == "" || len(b.Name) > 100 {
		fail(w, 400, "name must be 1–100 bytes")
		return
	}
	svc := Service{ID: randomID()[:16], Name: b.Name, SiteID: b.SiteID, URL: target.String()}
	err = s.store.update(func(st *State) error {
		for _, site := range st.Sites {
			if site.ID == b.SiteID {
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
	id := r.PathValue("id")
	err := s.store.update(func(st *State) error {
		for i, v := range st.Services {
			if v.ID == id {
				st.Services = append(st.Services[:i], st.Services[i+1:]...)
				return nil
			}
		}
		return fmt.Errorf("service not found")
	})
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	s.audit("service_deleted", u.Username, "", id)
	jsonOut(w, 200, map[string]bool{"ok": true})
}
func (s *Server) listUsers(w http.ResponseWriter, r *http.Request, u User) {
	users := s.store.snapshot().Users
	for i := range users {
		users[i].PasswordHash = ""
	}
	jsonOut(w, 200, users)
}
func (s *Server) createUser(w http.ResponseWriter, r *http.Request, u User) {
	var b struct {
		Username string   `json:"username"`
		Password string   `json:"password"`
		SiteIDs  []string `json:"siteIDs"`
	}
	if !decode(w, r, &b) {
		return
	}
	b.Username = strings.TrimSpace(b.Username)
	if b.Username == "" || len(b.Username) > 100 || len(b.Password) < 12 || len(b.Password) > 72 {
		fail(w, 400, "username required; password must be 12–72 bytes")
		return
	}
	h, err := bcrypt.GenerateFromPassword([]byte(b.Password), bcrypt.DefaultCost)
	if err != nil {
		fail(w, 400, "invalid password")
		return
	}
	user := User{ID: randomID()[:16], Username: b.Username, PasswordHash: string(h), Role: "viewer", SiteIDs: b.SiteIDs}
	err = s.store.update(func(st *State) error {
		for _, v := range st.Users {
			if v.Username == user.Username {
				return fmt.Errorf("username exists")
			}
		}
		for _, id := range b.SiteIDs {
			found := false
			for _, site := range st.Sites {
				if site.ID == id {
					found = true
				}
			}
			if !found {
				return fmt.Errorf("unknown site grant")
			}
		}
		st.Users = append(st.Users, user)
		return nil
	})
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	user.PasswordHash = ""
	s.audit("user_created", u.Username, "", user.ID)
	jsonOut(w, 201, user)
}
func (s *Server) deleteUser(w http.ResponseWriter, r *http.Request, u User) {
	id := r.PathValue("id")
	if id == u.ID {
		fail(w, 400, "cannot delete yourself")
		return
	}
	err := s.store.update(func(st *State) error {
		for i, v := range st.Users {
			if v.ID == id && v.Role != "admin" {
				st.Users = append(st.Users[:i], st.Users[i+1:]...)
				return nil
			}
		}
		return fmt.Errorf("viewer not found")
	})
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	s.audit("user_deleted", u.Username, "", id)
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
