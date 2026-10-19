package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync/atomic"
	"time"

	"github.com/XUJiahua/tunnel-mvp/internal/config"
	"github.com/XUJiahua/tunnel-mvp/internal/diagnostics"
	"github.com/gorilla/websocket"
)

type rule struct {
	prefix netip.Prefix
	host   string
	ports  map[string]bool
}
type Policy struct {
	rules    []rule
	resolver *net.Resolver
	siteID   string
	report   func(*diagnostics.Failure)
}

func NewPolicy(rules []config.Rule) (*Policy, error) {
	if len(rules) == 0 {
		return nil, errors.New("agent allow list must not be empty")
	}
	p := &Policy{resolver: net.DefaultResolver}
	for _, r := range rules {
		prefix, err := netip.ParsePrefix(r.CIDR)
		if err != nil {
			return nil, fmt.Errorf("invalid CIDR: %w", err)
		}
		if len(r.Ports) == 0 {
			return nil, errors.New("each rule needs explicit ports")
		}
		ports := map[string]bool{}
		for _, port := range r.Ports {
			if port < 1 || port > 65535 {
				return nil, errors.New("invalid port")
			}
			ports[fmt.Sprint(port)] = true
		}
		p.rules = append(p.rules, rule{prefix.Masked(), strings.ToLower(r.Host), ports})
	}
	return p, nil
}
func (p *Policy) allowed(host, port string, ip netip.Addr) bool {
	ip = ip.Unmap()
	if ip.IsUnspecified() || ip.IsMulticast() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return false
	}
	for _, r := range p.rules {
		if r.ports[port] && (r.host == "" || r.host == strings.ToLower(host)) && r.prefix.Contains(ip) {
			return true
		}
	}
	return false
}
func (p *Policy) DialContext(ctx context.Context, network, address string) (conn net.Conn, err error) {
	start := time.Now()
	id := diagnostics.ID()
	failure := func(code, stage, message string) *diagnostics.Failure {
		f := diagnostics.New(code, stage, message)
		f.AgentRequestID = id
		f.Target = address
		return f
	}
	defer func() {
		if err != nil {
			d := diagnostics.Describe(err)
			slog.Warn("agent target connection failed", "site_id", p.siteID, "agent_request_id", id, "target", address, "code", d.Code, "stage", d.Stage, "resolved_ips", d.ResolvedIPs, "attempts", d.Attempts, "error", d.Message, "hint", d.Hint, "latency_ms", time.Since(start).Milliseconds())
			if p.report != nil {
				p.report(d)
			}
		}
	}()
	if network != "tcp" && network != "tcp4" && network != "tcp6" {
		return nil, failure("invalid_target", "configuration", "only TCP connections are allowed")
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, failure("invalid_target", "configuration", err.Error())
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	ips, err := p.resolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, failure("dns_failed", "dns", err.Error())
	}
	resolved := []string{}
	for i, ip := range ips {
		ips[i] = ip.Unmap()
		resolved = append(resolved, ips[i].String())
	}
	slog.Debug("agent target DNS resolved", "site_id", p.siteID, "agent_request_id", id, "target", address, "resolved_ips", resolved)
	attempts := []diagnostics.Attempt{}
	var last error
	for _, ip := range ips {
		allowed := p.allowed(host, port, ip)
		attempt := diagnostics.Attempt{IP: ip.String(), Allowed: allowed}
		if !allowed {
			attempt.Error = "denied by Agent CIDR/host/port ACL"
			attempts = append(attempts, attempt)
			continue
		}
		conn, e := (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if e == nil {
			slog.Info("agent target connected", "site_id", p.siteID, "agent_request_id", id, "target", address, "ip", ip.String(), "latency_ms", time.Since(start).Milliseconds())
			return conn, nil
		}
		attempt.Error = e.Error()
		attempts = append(attempts, attempt)
		last = e
	}
	d := failure("acl_denied", "acl", "none of the resolved target IPs matched the Agent CIDR/host/port allow list")
	if last != nil {
		d = diagnostics.Describe(last)
		d.Stage = "tcp"
		d.AgentRequestID = id
		d.Target = address
	}
	d.ResolvedIPs = resolved
	d.Attempts = attempts
	return nil, d
}
func Run(ctx context.Context, c config.Agent, token string) error {
	if len(token) < 32 {
		return errors.New("TUNNEL_AGENT_TOKEN must contain the site token")
	}
	if c.SiteID == "" {
		return errors.New("siteID required")
	}
	if err := c.Validate(); err != nil {
		return err
	}
	p, err := NewPolicy(c.Allow)
	if err != nil {
		return err
	}
	p.siteID = c.SiteID
	report, closeReporter := newReporter(ctx, c, token)
	defer closeReporter()
	p.report = report
	slog.Info("agent configuration loaded", "site_id", c.SiteID, "instance_id", c.InstanceID, "server", c.ServerURL, "allow", c.Allow)
	u, err := config.Origin(c.ServerURL)
	if err != nil {
		return err
	}
	if u.Scheme == "http" && u.Hostname() != "localhost" && u.Hostname() != "127.0.0.1" && u.Hostname() != "::1" {
		return errors.New("agent requires HTTPS except on loopback")
	}
	if u.Scheme == "https" {
		u.Scheme = "wss"
	} else {
		u.Scheme = "ws"
	}
	u.Path = "/tunnel/connect"
	headers := http.Header{"Authorization": []string{"Bearer " + token}, "X-Tunnel-Site": []string{c.SiteID}, "X-Tunnel-Instance": []string{c.InstanceID}, "X-Tunnel-Version": []string{c.RuntimeVersion}}
	dialer := &websocket.Dialer{Proxy: http.ProxyFromEnvironment, HandshakeTimeout: 15 * time.Second}
	delay := time.Second
	for ctx.Err() == nil {
		var connected atomic.Bool
		err = connectTunnel(ctx, u.String(), headers, dialer, p.DialContext, func() {
			connected.Store(true)
			slog.Info("site tunnel connected", "site", c.SiteID)
		})
		if ctx.Err() != nil {
			return nil
		}
		if connected.Load() {
			delay = time.Second
		}
		slog.Warn("tunnel disconnected; reconnecting", "error", err, "delay", delay)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(delay + time.Duration(rand.IntN(500))*time.Millisecond):
		}
		delay = min(delay*2, 30*time.Second)
	}
	return nil
}
