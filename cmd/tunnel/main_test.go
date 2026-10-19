package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func execute(args ...string) (string, error) {
	cmd := newRootCommand()
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetErr(&output)
	cmd.SetArgs(legacyArgs(args))
	err := cmd.Execute()
	return output.String(), err
}
func TestCobraCommandsAndConfiguration(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"server", "--help"}, {"agent", "diagnose", "--help"}, {"completion", "bash"}, {"version"}, {"--version"}} {
		out, err := execute(args...)
		if err != nil || len(out) == 0 {
			t.Fatalf("%v: %q %v", args, out, err)
		}
	}
	for _, args := range [][]string{{"unknown"}, {"server", "extra"}, {"agent", "diagnose"}, {"server", "--unknown"}} {
		if _, err := execute(args...); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "server.yaml")
	if err := os.WriteFile(path, []byte("listen: 127.0.0.1:8001\npublicURL: http://localhost:8001\naccessURL: http://apps.localhost:8001\nlogLevel: warn\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TUNNEL_LISTEN", "127.0.0.1:8002")
	t.Setenv("TUNNEL_ADMIN_PASSWORD", "secret-not-printed")
	t.Setenv("TUNNEL_AGENT_TOKEN", "secret-token-not-printed")
	for _, flag := range []string{"-config", "--config", "-c"} {
		out, err := execute("server", flag, path, "--listen", "127.0.0.1:8003", "--log-level", "debug", "--check-config")
		if err != nil {
			t.Fatal(err)
		}
		var cfg map[string]any
		if json.Unmarshal([]byte(out), &cfg) != nil || cfg["listen"] != "127.0.0.1:8003" || cfg["logLevel"] != "debug" {
			t.Fatal(out)
		}
		if strings.Contains(out, "secret") {
			t.Fatal("credential printed")
		}
	}
	out, err := execute("server", "--config", path, "--check-config")
	if err != nil || !strings.Contains(out, "127.0.0.1:8002") || !strings.Contains(out, "warn") {
		t.Fatalf("environment/file precedence: %s %v", out, err)
	}
	t.Setenv("TUNNEL_CONFIG", path)
	out, err = execute("server", "--check-config")
	if err != nil || !strings.Contains(out, "localhost:8001") {
		t.Fatalf("TUNNEL_CONFIG ignored: %s %v", out, err)
	}
}
func TestAgentCLIFlagsAndStrictConfig(t *testing.T) {
	out, err := execute("agent", "--site-id", "local-site", "--allow", `[{"cidr":"127.0.0.0/8","ports":[3000]}]`, "--check-config")
	if err != nil || !strings.Contains(out, "local-site") {
		t.Fatalf("%s %v", out, err)
	}
	path := filepath.Join(t.TempDir(), "agent.json")
	_ = os.WriteFile(path, []byte(`{"siteID":"test","allow":[{"cidr":"127.0.0.0/8","ports":[3000],"typo":true}]}`), 0600)
	if _, err = execute("agent", "-c", path, "--check-config"); err == nil {
		t.Fatal("unknown ACL field accepted")
	}
	if _, err = execute("agent", "--site-id", "test", "--allow", "[]", "--check-config"); err == nil {
		t.Fatal("empty ACL accepted")
	}
	if _, err = execute("server", "--log-level", "trace", "--check-config"); err == nil {
		t.Fatal("invalid log level accepted")
	}
}

func TestStandaloneAgentDiagnosisWithoutToken(t *testing.T) {
	t.Setenv("TUNNEL_AGENT_TOKEN", "")
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	defer app.Close()
	target, _ := url.Parse(app.URL)
	port, _ := strconv.Atoi(target.Port())
	rules := fmt.Sprintf(`[{"cidr":"127.0.0.0/8","ports":[%d]}]`, port)
	out, err := execute("agent", "diagnose", "--site-id", "local", "--allow", rules, "--target", app.URL)
	if err != nil || !strings.Contains(out, `"reachable": true`) {
		t.Fatalf("local diagnosis failed: %s %v", out, err)
	}
	out, err = execute("agent", "diagnose", "--site-id", "local", "--allow", `[{"cidr":"127.0.0.0/8","ports":[1]}]`, "--target", app.URL)
	if err == nil || !strings.Contains(out, `"acl_denied"`) || !strings.Contains(out, `"agentRequestID"`) {
		t.Fatalf("diagnostic exit/output: %s %v", out, err)
	}
}

func TestOperationsConfiguration(t *testing.T) {
	t.Setenv("TUNNEL_PROBE_INTERVAL_SECONDS", "120")
	out, err := execute("server", "--probe-interval", "0", "--audit-max-mb", "8", "--audit-backups", "3", "--check-config")
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if json.Unmarshal([]byte(out), &cfg) != nil || cfg["probeIntervalSeconds"] != float64(0) || cfg["auditMaxMB"] != float64(8) || cfg["auditBackups"] != float64(3) {
		t.Fatalf("operations flags not applied: %s", out)
	}
	for _, args := range [][]string{{"server", "--probe-interval", "1", "--check-config"}, {"server", "--audit-backups", "21", "--check-config"}, {"server", "--audit-max-mb", "-1", "--check-config"}} {
		if _, err := execute(args...); err == nil {
			t.Fatalf("invalid configuration accepted: %v", args)
		}
	}
	t.Setenv("TUNNEL_CONFIG", "")
	_, err = execute("agent", "--site-id", "test", "--allow", `[{"cidr":"127.0.0.0/8","ports":[80]}]`, "--watch-config")
	if err == nil || !strings.Contains(err.Error(), "requires --config") {
		t.Fatalf("watcher should require config: %v", err)
	}
}
