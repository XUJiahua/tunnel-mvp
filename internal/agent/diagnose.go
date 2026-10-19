package agent

import (
	"context"
	"crypto/tls"
	"net/http"
	"time"

	"github.com/XUJiahua/tunnel-mvp/internal/config"
	"github.com/XUJiahua/tunnel-mvp/internal/diagnostics"
)

type Diagnosis struct {
	Reachable bool                 `json:"reachable"`
	Target    string               `json:"target"`
	Status    int                  `json:"status,omitempty"`
	LatencyMs int64                `json:"latencyMs"`
	Failure   *diagnostics.Failure `json:"diagnostic,omitempty"`
}

// Diagnose checks the actual Agent DNS/ACL/TCP path without needing a tunnel or token.
func Diagnose(ctx context.Context, c config.Agent, target string) (Diagnosis, error) {
	origin, err := config.Origin(target)
	if err != nil {
		return Diagnosis{}, err
	}
	policy, err := NewPolicy(c.Allow)
	if err != nil {
		return Diagnosis{}, err
	}
	policy.siteID = c.SiteID
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	transport := &http.Transport{Proxy: nil, DialContext: policy.DialContext, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 10 * time.Second}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, _ := http.NewRequestWithContext(ctx, http.MethodHead, origin.String(), nil)
	start := time.Now()
	response, err := client.Do(req)
	out := Diagnosis{Target: origin.String(), LatencyMs: time.Since(start).Milliseconds()}
	if err != nil {
		out.Failure = diagnostics.Describe(err)
		return out, nil
	}
	defer response.Body.Close()
	out.Reachable = true
	out.Status = response.StatusCode
	return out, nil
}
