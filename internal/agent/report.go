package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/XUJiahua/tunnel-mvp/internal/config"
	"github.com/XUJiahua/tunnel-mvp/internal/diagnostics"
)

func newReporter(root context.Context, c config.Agent, token string) (func(*diagnostics.Failure), func()) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	client := &http.Client{Transport: transport, Timeout: 2 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return func(failure *diagnostics.Failure) {
		ctx, cancel := context.WithTimeout(root, 2*time.Second)
		defer cancel()
		body, _ := json.Marshal(failure)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.ServerURL+"/tunnel/diagnostics", bytes.NewReader(body))
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("X-Tunnel-Site", c.SiteID)
		resp, err := client.Do(req)
		if err != nil {
			slog.Warn("agent diagnostic upload failed", "site_id", c.SiteID, "agent_request_id", failure.AgentRequestID, "error", err)
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			slog.Warn("agent diagnostic upload rejected; use Agent logs for full details", "site_id", c.SiteID, "agent_request_id", failure.AgentRequestID, "http_status", resp.StatusCode)
		}
	}, transport.CloseIdleConnections
}
