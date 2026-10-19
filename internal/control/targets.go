package control

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"

	"github.com/XUJiahua/tunnel-mvp/internal/state"
)

func (s *Server) validateDomain(actor User, domain string) error {
	if domain == "" {
		return nil
	}
	if actor.Role != "admin" {
		return fmt.Errorf("custom domains require a global administrator")
	}
	if !state.ValidCustomDomain(domain) {
		return fmt.Errorf("custom domain must be a lowercase DNS hostname without port")
	}
	related := func(a, b string) bool { return a == b || strings.HasSuffix(a, "."+b) || strings.HasSuffix(b, "."+a) }
	dev := s.public.Scheme == "http" && (s.public.Hostname() == "localhost" || net.ParseIP(s.public.Hostname()).IsLoopback())
	if related(domain, strings.ToLower(s.access.Hostname())) || !dev && related(domain, strings.ToLower(s.public.Hostname())) || domain == strings.ToLower(s.public.Hostname()) {
		return fmt.Errorf("custom domain overlaps the console or application namespace")
	}
	return nil
}
func customHost(domain string, access *url.URL) string {
	if access.Port() != "" {
		return net.JoinHostPort(domain, access.Port())
	}
	return domain
}
func (s *Server) serviceForHost(host string) (Service, bool) {
	host = strings.ToLower(host)
	suffix := "." + strings.ToLower(s.access.Host)
	id := ""
	if strings.HasSuffix(host, suffix) {
		id = strings.TrimSuffix(host, suffix)
		if strings.Contains(id, ".") {
			id = ""
		}
	}
	for _, svc := range s.store.snapshot().Services {
		if svc.ID == id || svc.CustomDomain != "" && host == customHost(svc.CustomDomain, s.access) {
			return svc, true
		}
	}
	return Service{}, false
}
func mountPath(raw string) string {
	u, _ := url.Parse(raw)
	if u.Path == "" {
		return "/"
	}
	return u.Path
}
func inMount(u *url.URL, base string) bool {
	if base == "/" {
		return true
	}
	if !strings.HasPrefix(u.Path, base) || !strings.HasPrefix(path.Clean(u.Path), strings.TrimSuffix(base, "/")+"/") && path.Clean(u.Path) != strings.TrimSuffix(base, "/") {
		return false
	}
	lower := strings.ToLower(u.Path)
	return !strings.ContainsAny(u.Path, "\\\x00") && !strings.Contains(lower, "%2e") && !strings.Contains(lower, "%2f") && !strings.Contains(lower, "%5c") && !strings.Contains(lower, "%25")
}
func (s *Server) mountGuard(w http.ResponseWriter, r *http.Request, svc Service) bool {
	base := mountPath(svc.URL)
	if base == "/" {
		return true
	}
	if (r.Method == "GET" || r.Method == "HEAD") && (r.URL.Path == "/" || r.URL.Path == strings.TrimSuffix(base, "/")) {
		dest := &url.URL{Path: base, RawQuery: r.URL.RawQuery}
		http.Redirect(w, r, dest.String(), http.StatusTemporaryRedirect)
		return false
	}
	if !inMount(r.URL, base) {
		http.NotFound(w, r)
		return false
	}
	return true
}
