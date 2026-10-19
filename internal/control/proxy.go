package control

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"
)

func (s *Server) serviceURL(id string) *url.URL {
	u := *s.access
	u.Host = id + "." + s.access.Host
	return &u
}
func (s *Server) launch(w http.ResponseWriter, r *http.Request, u User) {
	svc, ok := s.store.service(r.PathValue("id"))
	if !ok || !canAccess(u, svc.SiteID) {
		fail(w, 404, "service not found")
		return
	}
	if !s.tunnel.HasSession(svc.SiteID) {
		fail(w, 503, "site is offline")
		return
	}
	_, key, _ := s.currentUser(r)
	token := randomID()
	s.auth.mu.Lock()
	s.auth.clean(time.Now())
	s.auth.tickets[digest(token)] = grant{key, svc.ID, time.Now().Add(30 * time.Second)}
	s.auth.mu.Unlock()
	dest := s.serviceURL(svc.ID)
	dest.Path = "/_tunnel/entry"
	dest.RawQuery = url.Values{"ticket": []string{token}}.Encode()
	s.audit("service_launch", u.Username, svc.SiteID, svc.ID)
	jsonOut(w, 200, map[string]string{"url": dest.String()})
}
func (s *Server) validGrant(g grant, serviceID string) (User, bool) {
	if g.ServiceID != serviceID || time.Now().After(g.Expires) {
		return User{}, false
	}
	s.auth.mu.Lock()
	sess, ok := s.auth.sessions[g.SessionKey]
	s.auth.mu.Unlock()
	if !ok || time.Now().After(sess.Expires) {
		return User{}, false
	}
	u, ok := s.store.user(sess.UserID)
	return u, ok
}
func (s *Server) gateway(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	suffix := "." + s.access.Host
	host := strings.ToLower(r.Host)
	if !strings.HasSuffix(host, strings.ToLower(suffix)) {
		http.NotFound(w, r)
		return
	}
	id := strings.TrimSuffix(host, strings.ToLower(suffix))
	if strings.Contains(id, ".") {
		http.NotFound(w, r)
		return
	}
	svc, ok := s.store.service(id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if r.URL.Path == "/_tunnel/entry" {
		if r.Method != "GET" {
			fail(w, 405, "GET required")
			return
		}
		key := digest(r.URL.Query().Get("ticket"))
		s.auth.mu.Lock()
		g, found := s.auth.tickets[key]
		delete(s.auth.tickets, key)
		s.auth.mu.Unlock()
		u, valid := s.validGrant(g, id)
		if !found || !valid || !canAccess(u, svc.SiteID) {
			fail(w, 401, "access ticket expired or invalid; reopen from console")
			return
		}
		token := randomID()
		g.Expires = time.Now().Add(time.Hour)
		s.auth.mu.Lock()
		s.auth.clean(time.Now())
		s.auth.access[digest(token)] = g
		s.auth.mu.Unlock()
		http.SetCookie(w, &http.Cookie{Name: accessCookie, Value: token, Path: "/", HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteLaxMode, MaxAge: 3600})
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	c, err := r.Cookie(accessCookie)
	if err != nil {
		fail(w, 401, "open this application from the control plane")
		return
	}
	s.auth.mu.Lock()
	g, ok := s.auth.access[digest(c.Value)]
	s.auth.mu.Unlock()
	u, valid := s.validGrant(g, id)
	if !ok || !valid || !canAccess(u, svc.SiteID) {
		fail(w, 401, "access expired; reopen from console")
		return
	}
	if !s.tunnel.HasSession(svc.SiteID) {
		fail(w, 503, "site is offline")
		return
	}
	target, _ := url.Parse(svc.URL)
	transport := s.transport(svc.SiteID)
	defer transport.CloseIdleConnections()
	public := s.serviceURL(id)
	proxy := &httputil.ReverseProxy{
		Transport: transport, FlushInterval: -1,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Header.Del("X-Forwarded-For")
			pr.Out.Header.Del("X-Forwarded-Host")
			pr.Out.Header.Del("X-Forwarded-Proto")
			pr.Out.Header.Del("Forwarded")
			pr.SetXForwarded()
			pr.Out.Header.Set("X-Forwarded-Host", public.Host)
			pr.Out.Header.Set("X-Forwarded-Proto", public.Scheme)
			// Translate same-application origins for upstream CSRF and WebSocket checks.
			if pr.Out.Header.Get("Origin") == public.String() {
				pr.Out.Header.Set("Origin", target.String())
			}
			pr.Out.Header.Del("Cookie")
			for _, c := range pr.In.Cookies() {
				if c.Name != accessCookie && c.Name != consoleCookie {
					pr.Out.AddCookie(c)
				}
			}
		},
		ModifyResponse: func(resp *http.Response) error {
			if location := resp.Header.Get("Location"); location != "" {
				dest, err := url.Parse(location)
				if err == nil && strings.EqualFold(dest.Host, target.Host) && (dest.Scheme == "" || dest.Scheme == target.Scheme) {
					dest.Scheme = public.Scheme
					dest.Host = public.Host
					resp.Header.Set("Location", dest.String())
				}
			}
			cookies := resp.Cookies()
			resp.Header.Del("Set-Cookie")
			for _, c := range cookies {
				if c.Name == accessCookie || c.Name == consoleCookie {
					continue
				}
				c.Domain = ""
				if s.secure {
					c.Secure = true
				}
				resp.Header.Add("Set-Cookie", c.String())
			}
			if resp.Header.Get("Access-Control-Allow-Origin") == target.String() {
				resp.Header.Set("Access-Control-Allow-Origin", public.String())
			}
			resp.Header.Set("Referrer-Policy", "no-referrer")
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			s.audit("proxy_error", u.Username, svc.SiteID, svc.ID)
			fail(w, 502, "upstream unavailable; check agent ACL, DNS, port and TLS trust")
		},
	}
	s.audit("proxy_request", u.Username, svc.SiteID, svc.ID)
	proxy.ServeHTTP(w, r)
}
func (s *Server) transport(siteID string) *http.Transport {
	remote := s.tunnel.Dialer(siteID)
	return &http.Transport{Proxy: nil, DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		return remote(ctx, network, addr)
	}, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 30 * time.Second, IdleConnTimeout: 30 * time.Second, MaxIdleConnsPerHost: 4, ForceAttemptHTTP2: false}
}
func (s *Server) probe(w http.ResponseWriter, r *http.Request, u User) {
	svc, ok := s.store.service(r.PathValue("id"))
	if !ok || !canAccess(u, svc.SiteID) {
		fail(w, 404, "service not found")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()
	t := s.transport(svc.SiteID)
	defer t.CloseIdleConnections()
	client := &http.Client{Transport: t, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(ctx, "HEAD", svc.URL, nil)
	if err != nil {
		fail(w, 400, "invalid target")
		return
	}
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		s.audit("probe_failed", u.Username, svc.SiteID, svc.ID)
		jsonOut(w, 200, map[string]any{"reachable": false, "message": "Dial/HTTP failed: check site, ACL, DNS and TLS"})
		return
	}
	resp.Body.Close()
	s.audit("probe", u.Username, svc.SiteID, svc.ID)
	jsonOut(w, 200, map[string]any{"reachable": true, "status": resp.StatusCode, "latencyMs": time.Since(start).Milliseconds()})
}
