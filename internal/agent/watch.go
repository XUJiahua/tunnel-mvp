package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/XUJiahua/tunnel-mvp/internal/config"
)

// RunWatched validates the complete effective config before replacing the Agent.
// Restarting only the tunnel (not the process) also terminates connections that
// were admitted under an older, potentially broader ACL.
func RunWatched(ctx context.Context, initial config.Agent, token string, load func() (config.Agent, error)) error {
	return runWatched(ctx, initial, token, load, 2*time.Second)
}
func runWatched(ctx context.Context, initial config.Agent, token string, load func() (config.Agent, error), interval time.Duration) error {
	current := initial
	if err := current.Validate(); err != nil {
		return err
	}
	if _, err := NewPolicy(current.Allow); err != nil {
		return err
	}
	if len(token) < 32 {
		return fmt.Errorf("TUNNEL_AGENT_TOKEN must contain the site token")
	}
	cancel, done := startAgent(ctx, current, token)
	defer func() { cancel() }()
	tick := time.NewTicker(interval)
	defer tick.Stop()
	lastError := ""
	for {
		select {
		case err := <-done:
			return err
		case <-ctx.Done():
			cancel()
			return <-done
		case <-tick.C:
			next, err := load()
			if err == nil {
				err = next.Validate()
			}
			if err == nil {
				_, err = NewPolicy(next.Allow)
			}
			if err == nil && (next.SiteID != current.SiteID || next.ServerURL != current.ServerURL) {
				err = fmt.Errorf("siteID/serverURL changes require a process restart")
			}
			if err != nil {
				if err.Error() != lastError {
					slog.Warn("agent config reload rejected; keeping current policy", "error", err)
					lastError = err.Error()
				}
				continue
			}
			lastError = ""
			a, _ := json.Marshal(current.Allow)
			b, _ := json.Marshal(next.Allow)
			if string(a) == string(b) {
				continue
			}
			cancel()
			if err := <-done; err != nil {
				return err
			}
			if ctx.Err() != nil {
				return nil
			}
			current = next
			slog.Info("agent ACL reloaded; replacing tunnel", "site_id", current.SiteID)
			cancel, done = startAgent(ctx, current, token)
		}
	}
}

func startAgent(ctx context.Context, c config.Agent, token string) (context.CancelFunc, <-chan error) {
	child, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- Run(child, c, token) }()
	return cancel, done
}
