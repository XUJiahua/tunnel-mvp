package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/XUJiahua/tunnel-mvp/internal/config"
	"github.com/XUJiahua/tunnel-mvp/internal/state"
)

func fetchPolicy(ctx context.Context, c config.Agent, token string) (state.AgentPolicy, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", strings.TrimSuffix(c.ServerURL, "/")+"/tunnel/policy", nil)
	if err != nil {
		return state.AgentPolicy{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Tunnel-Site", c.SiteID)
	req.Header.Set("X-Tunnel-Instance", c.InstanceID)
	client := http.Client{Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Do(req)
	if err != nil {
		return state.AgentPolicy{}, err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return state.AgentPolicy{}, fmt.Errorf("Agent policy endpoint returned %d", res.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(res.Body, 65537))
	if err != nil || len(raw) > 65536 {
		return state.AgentPolicy{}, fmt.Errorf("Agent policy response exceeds limit or cannot be read")
	}
	var policy state.AgentPolicy
	d := json.NewDecoder(strings.NewReader(string(raw)))
	d.DisallowUnknownFields()
	if err = d.Decode(&policy); err != nil {
		return policy, err
	}
	if d.Decode(new(any)) != io.EOF {
		return policy, fmt.Errorf("trailing policy content")
	}
	return policy, state.ValidateAgentPolicy(&policy)
}
func runRemotePolicy(ctx context.Context, c config.Agent, token string, interval, maxAge time.Duration) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if len(token) < 32 {
		return fmt.Errorf("TUNNEL_AGENT_TOKEN must contain the instance token")
	}
	origin, _ := config.Origin(c.ServerURL)
	if origin.Scheme != "https" && origin.Hostname() != "localhost" && origin.Hostname() != "127.0.0.1" && origin.Hostname() != "::1" {
		return fmt.Errorf("remote policy requires HTTPS except loopback")
	}
	if _, err := NewPolicy(c.Allow); err != nil {
		return err
	}
	var cancel context.CancelFunc
	var done chan error
	version := ""
	lastGood := time.Time{}
	stop := func() {
		if cancel != nil {
			cancel()
			<-done
			cancel = nil
			done = nil
		}
	}
	defer stop()
	for ctx.Err() == nil {
		policy, err := fetchPolicy(ctx, c, token)
		if err == nil {
			lastGood = time.Now()
			if cancel == nil || version != policy.Version {
				stop()
				remote := &Policy{resolver: net.DefaultResolver}
				if len(policy.Allow) > 0 {
					remote, err = NewPolicy(policy.Allow)
					if err != nil {
						return err
					}
				}
				version = policy.Version
				child, childCancel := context.WithCancel(ctx)
				cancel = childCancel
				done = make(chan error, 1)
				go func(p *Policy, v string, ch chan error) { ch <- runConfigured(child, c, token, p, v) }(remote, version, done)
				slog.Info("remote Agent policy applied", "site_id", c.SiteID, "instance_id", c.InstanceID, "version", version, "rule_count", len(policy.Allow))
			}
		} else {
			slog.Warn("Agent policy fetch failed", "instance_id", c.InstanceID, "error", err)
			if cancel != nil && time.Since(lastGood) >= maxAge {
				stop()
				slog.Warn("Agent policy expired; tunnel closed", "instance_id", c.InstanceID)
			}
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case err := <-done:
			timer.Stop()
			cancel = nil
			done = nil
			if ctx.Err() != nil {
				return nil
			}
			return err
		case <-timer.C:
		}
	}
	return nil
}
