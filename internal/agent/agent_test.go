package agent

import (
	"context"
	"fmt"
	"github.com/XUJiahua/tunnel-mvp/internal/diagnostics"
	"net"
	"net/netip"
	"strconv"
	"testing"
	"time"

	"github.com/XUJiahua/tunnel-mvp/internal/config"
)

func TestPolicyPinsResolvedIPsAndPorts(t *testing.T) {
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			c, e := l.Accept()
			if e != nil {
				return
			}
			c.Close()
		}
	}()
	port := l.Addr().(*net.TCPAddr).Port
	p, err := NewPolicy([]config.Rule{{CIDR: "127.0.0.0/8", Host: "localhost", Ports: []int{port}}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	c, err := p.DialContext(ctx, "tcp", net.JoinHostPort("localhost", strconv.Itoa(port)))
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	for _, target := range []string{l.Addr().String(), "localhost:1", "169.254.169.254:80"} {
		if c, e := p.DialContext(ctx, "tcp", target); e == nil {
			c.Close()
			t.Fatalf("unexpected access to %s", target)
		}
	}
	if _, e := p.DialContext(ctx, "udp", l.Addr().String()); e == nil {
		t.Fatal("UDP allowed")
	}
	for _, ip := range []string{"169.254.169.254", "0.0.0.0", "224.0.0.1", "fe80::1"} {
		if p.allowed("localhost", strconv.Itoa(port), netip.MustParseAddr(ip)) {
			t.Fatalf("unsafe IP allowed: %s", ip)
		}
	}
}

func TestPolicyRejectsEmptyAndInvalidRules(t *testing.T) {
	for _, rules := range [][]config.Rule{nil, {{CIDR: "invalid", Ports: []int{80}}}, {{CIDR: "0.0.0.0/0"}}, {{CIDR: "0.0.0.0/0", Ports: []int{70000}}}} {
		if _, err := NewPolicy(rules); err == nil {
			t.Fatalf("accepted invalid rules: %+v", rules)
		}
	}
}

func TestDiagnosticStages(t *testing.T) {
	p, err := NewPolicy([]config.Rule{{CIDR: "127.0.0.0/8", Ports: []int{3000}}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.DialContext(context.Background(), "tcp", "127.0.0.1:3001")
	d := diagnostics.Describe(err)
	if d.Code != "acl_denied" || d.AgentRequestID == "" || len(d.Attempts) != 1 || d.Attempts[0].Allowed || d.ResolvedIPs[0] != "127.0.0.1" {
		t.Fatalf("missing ACL detail: %+v", d)
	}
	p.resolver = &net.Resolver{PreferGo: true, Dial: func(context.Context, string, string) (net.Conn, error) {
		return nil, fmt.Errorf("controlled DNS lookup failure")
	}}
	_, err = p.DialContext(context.Background(), "tcp", "does-not-exist.invalid:3000")
	d = diagnostics.Describe(err)
	if d.Code != "dns_failed" || d.Stage != "dns" || d.AgentRequestID == "" {
		t.Fatalf("missing DNS detail: %+v", d)
	}
}
