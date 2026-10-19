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
	"github.com/gorilla/websocket"
	"github.com/rancher/remotedialer"
)

type rule struct {
	prefix netip.Prefix
	host   string
	ports  map[string]bool
}
type Policy struct {
	rules    []rule
	resolver *net.Resolver
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
func (p *Policy) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if network != "tcp" && network != "tcp4" && network != "tcp6" {
		return nil, errors.New("only TCP allowed")
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	ips, err := p.resolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	// Resolve in the site and pin the checked IP in the actual dial: no second DNS lookup.
	var last error = errors.New("target denied by agent ACL")
	for _, ip := range ips {
		if !p.allowed(host, port, ip) {
			continue
		}
		c, e := (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if e == nil {
			return c, nil
		}
		last = e
	}
	return nil, last
}
func Run(ctx context.Context, c config.Agent, token string) error {
	if len(token) < 32 {
		return errors.New("TUNNEL_AGENT_TOKEN must contain the site token")
	}
	if c.SiteID == "" {
		return errors.New("siteID required")
	}
	p, err := NewPolicy(c.Allow)
	if err != nil {
		return err
	}
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
	headers := http.Header{"Authorization": []string{"Bearer " + token}, "X-Tunnel-Site": []string{c.SiteID}}
	dialer := &websocket.Dialer{Proxy: http.ProxyFromEnvironment, HandshakeTimeout: 15 * time.Second}
	delay := time.Second
	for ctx.Err() == nil {
		var connected atomic.Bool
		err = remotedialer.ConnectToProxyWithDialer(ctx, u.String(), headers, func(proto, address string) bool { return proto == "tcp" || proto == "tcp4" || proto == "tcp6" }, dialer, p.DialContext, func(_ context.Context, _ *remotedialer.Session) error {
			connected.Store(true)
			slog.Info("site tunnel connected", "site", c.SiteID)
			return nil
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
