package control

import (
	"context"
	"crypto/tls"
	"github.com/XUJiahua/tunnel-mvp/internal/diagnostics"
	"log/slog"
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
	if svc, ok := s.store.service(id); ok && svc.CustomDomain != "" {
		u.Host = customHost(svc.CustomDomain, s.access)
	}
	return &u
}
func (s *Server) launch(w http.ResponseWriter, r *http.Request, u User) {
	s.accessMu.RLock()
	defer s.accessMu.RUnlock()
	fresh, _, authenticated := s.currentUser(r)
	if !authenticated {
		fail(w, 401, "sign in required")
		return
	}
	u = fresh
	svc, ok := s.store.service(r.PathValue("id"))
	if !ok || !canAccessService(u, svc) {
		fail(w, 404, "service not found")
		return
	}
	if !s.siteOnline(svc.SiteID) {
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
	if !ok || time.Now().After(sess.Expires) || !s.identitySessionAllowed(sess) {
		return User{}, false
	}
	u, ok := s.store.user(sess.UserID)
	return u, ok && !u.Disabled
}
func (s *Server) gateway(w http.ResponseWriter, r *http.Request) {
	s.accessMu.RLock()
	locked := true
	defer func() {
		if locked {
			s.accessMu.RUnlock()
		}
	}()
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	svc, ok := s.serviceForHost(r.Host)
	if !ok {
		http.NotFound(w, r)
		return
	}
	id := svc.ID
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
		if !found || !valid || !canAccessService(u, svc) {
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
		http.Redirect(w, r, mountPath(svc.URL), http.StatusSeeOther)
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
	if !ok || !valid || !canAccessService(u, svc) {
		fail(w, 401, "access expired; reopen from console")
		return
	}
	if !s.siteOnline(svc.SiteID) {
		fail(w, 503, "site is offline")
		return
	}
	if !s.mountGuard(w, r, svc) {
		return
	}
	ctx, release := s.trackRequest(r.Context(), u, g)
	defer release()
	r = r.WithContext(ctx)
	target, _ := url.Parse(svc.URL)
	upstreamOrigin := *target
	upstreamOrigin.Path = ""
	upstreamOrigin.RawPath = ""
	transport := s.serviceTransport(svc, g.SessionKey)
	s.accessMu.RUnlock()
	locked = false
	s.stats.requests.Add(1)
	public := &url.URL{Scheme: s.access.Scheme, Host: r.Host}
	proxy := &httputil.ReverseProxy{
		Transport: transport, FlushInterval: -1,
		ErrorLog: slog.NewLogLogger(slog.Default().With("site_id", svc.SiteID, "service_id", svc.ID, "target", svc.URL).Handler(), slog.LevelWarn),
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(&upstreamOrigin)
			pr.Out.Header.Del("X-Forwarded-For")
			pr.Out.Header.Del("X-Forwarded-Host")
			pr.Out.Header.Del("X-Forwarded-Proto")
			pr.Out.Header.Del("Forwarded")
			pr.SetXForwarded()
			pr.Out.Header.Set("X-Forwarded-Host", public.Host)
			pr.Out.Header.Set("X-Forwarded-Proto", public.Scheme)
			// Translate same-application origins for upstream CSRF and WebSocket checks.
			if pr.Out.Header.Get("Origin") == public.String() {
				pr.Out.Header.Set("Origin", upstreamOrigin.String())
			}
			if ref, err := url.Parse(pr.Out.Header.Get("Referer")); err == nil && ref.Host == public.Host && ref.Scheme == public.Scheme {
				ref.Host = target.Host
				ref.Scheme = target.Scheme
				pr.Out.Header.Set("Referer", ref.String())
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
			if resp.Header.Get("Access-Control-Allow-Origin") == upstreamOrigin.String() {
				resp.Header.Set("Access-Control-Allow-Origin", public.String())
			}
			// Preserve same-application CSRF referers; never send them cross-origin.
			resp.Header.Set("Referrer-Policy", "same-origin")
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			s.stats.errors.Add(1)
			s.proxyFailure(w, u, svc, err)
		},
	}
	s.audit("proxy_request", u.Username, svc.SiteID, svc.ID)
	proxy.ServeHTTP(w, r)
}
func (s *Server) transport(siteID string, serviceIDs ...string) *http.Transport {
	serviceID := ""
	if len(serviceIDs) > 0 {
		serviceID = serviceIDs[0]
	}
	return &http.Transport{Proxy: nil, DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		return s.dialSite(ctx, siteID, serviceID, network, addr)
	}, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 30 * time.Second, IdleConnTimeout: 30 * time.Second, MaxIdleConnsPerHost: 4, ForceAttemptHTTP2: false}
}
func (s *Server) probe(w http.ResponseWriter, r *http.Request, u User) {
	s.accessMu.RLock()
	fresh, key, valid := s.currentUser(r)
	if !valid {
		s.accessMu.RUnlock()
		fail(w, 401, "sign in required")
		return
	}
	u = fresh
	svc, ok := s.store.service(r.PathValue("id"))
	if !ok || (!canAccessService(u, svc) && !canManage(u, svc.OrganizationID, "applications:manage")) {
		s.accessMu.RUnlock()
		fail(w, 404, "service not found")
		return
	}
	ctx, release := s.trackRequest(r.Context(), u, grant{SessionKey: key, ServiceID: svc.ID, Expires: time.Now().Add(14 * time.Second)})
	s.accessMu.RUnlock()
	defer release()
	report := s.checkService(ctx, svc)
	s.saveReport(svc, report)
	event := "probe"
	if !report.Reachable {
		event = "probe_failed"
	}
	s.recordDiagnostic(event, u, report)
	w.Header().Set("X-Tunnel-Request-ID", report.RequestID)
	jsonOut(w, 200, report)
}
func (s *Server) checkService(parent context.Context, svc Service) probeReport {
	report := newReport(svc)
	start := time.Now()
	defer func() { s.stats.probes.Add(1) }()
	if !s.siteOnline(svc.SiteID) {
		report.Failure = diagnostics.New("site_offline", "tunnel", "site Agent is not connected")
		return report
	}
	ctx, cancel := context.WithTimeout(parent, 14*time.Second)
	defer cancel()
	t := s.serviceTransport(svc)
	client := &http.Client{Transport: t, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(ctx, func() string {
		if svc.HealthMethod != "" {
			return svc.HealthMethod
		}
		return "HEAD"
	}(), probeURL(svc), nil)
	if err != nil {
		report.Failure = diagnostics.New("invalid_target", "configuration", err.Error())
		return report
	}
	resp, err := client.Do(req)
	report.LatencyMs = time.Since(start).Milliseconds()
	if err != nil {
		report.Failure = s.describeFailure(svc.SiteID, err)
		return report
	}
	defer resp.Body.Close()
	report.Reachable = true
	report.Status = resp.StatusCode
	report.Healthy = resp.StatusCode >= 200 && resp.StatusCode < 400
	if svc.HealthExpectedStatus != 0 {
		report.Healthy = resp.StatusCode == svc.HealthExpectedStatus
	}
	return report
}
