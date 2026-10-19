package config

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/pflag"
	"github.com/spf13/viper"
)

type binding struct{ key, flag, env string }

var commonBindings = []binding{{"logLevel", "log-level", "TUNNEL_LOG_LEVEL"}, {"logFormat", "log-format", "TUNNEL_LOG_FORMAT"}}
var serverBindings = []binding{{"probeIntervalSeconds", "probe-interval", "TUNNEL_PROBE_INTERVAL_SECONDS"}, {"auditMaxMB", "audit-max-mb", "TUNNEL_AUDIT_MAX_MB"}, {"auditBackups", "audit-backups", "TUNNEL_AUDIT_BACKUPS"}, {"listen", "listen", "TUNNEL_LISTEN"}, {"publicURL", "public-url", "TUNNEL_PUBLIC_URL"}, {"accessURL", "access-url", "TUNNEL_ACCESS_URL"}, {"dataDir", "data-dir", "TUNNEL_DATA_DIR"}, {"adminUsername", "admin-username", "TUNNEL_ADMIN_USERNAME"}}
var agentBindings = []binding{{"serverURL", "server-url", "TUNNEL_SERVER_URL"}, {"siteID", "site-id", "TUNNEL_SITE_ID"}, {"allow", "allow", "TUNNEL_ALLOW"}}

func reader(path string, flags *pflag.FlagSet, bindings []binding) (*viper.Viper, error) {
	v := viper.New()
	v.SetDefault("logLevel", "info")
	v.SetDefault("logFormat", "text")
	for _, b := range append(append([]binding{}, commonBindings...), bindings...) {
		if flags != nil {
			if flag := flags.Lookup(b.flag); flag != nil {
				if err := v.BindPFlag(b.key, flag); err != nil {
					return nil, err
				}
			}
		}
		if err := v.BindEnv(b.key, b.env); err != nil {
			return nil, err
		}
	}
	if path == "" {
		path = os.Getenv("TUNNEL_CONFIG")
	}
	if path != "" {
		v.SetConfigFile(path)
		if err := v.ReadInConfig(); err != nil {
			return nil, fmt.Errorf("read config %q: %w", path, err)
		}
	}
	return v, nil
}

// ServerFrom uses explicit flags > environment > JSON/YAML file > defaults.
func ServerFrom(path string, flags *pflag.FlagSet) (Server, error) {
	var c Server
	v, err := reader(path, flags, serverBindings)
	if err != nil {
		return c, err
	}
	v.SetDefault("probeIntervalSeconds", 60)
	for k, value := range map[string]string{"listen": ":8080", "publicURL": "http://localhost:8080", "accessURL": "http://apps.localhost:8080", "dataDir": "data", "adminUsername": "admin"} {
		v.SetDefault(k, value)
	}
	if err = v.UnmarshalExact(&c); err != nil {
		return c, fmt.Errorf("invalid server config: %w", err)
	}
	return c, c.Validate()
}
func AgentFrom(path string, flags *pflag.FlagSet) (Agent, error) {
	var c Agent
	v, err := reader(path, flags, agentBindings)
	if err != nil {
		return c, err
	}
	v.SetDefault("serverURL", "http://localhost:8080")
	// Environment/CLI ACL overrides are JSON arrays, not comma-separated rules.
	if raw, ok := v.Get("allow").(string); ok && raw != "" {
		var rules any
		if err = json.Unmarshal([]byte(raw), &rules); err != nil {
			return c, fmt.Errorf("allow must be a JSON array: %w", err)
		}
		v.Set("allow", rules)
	}
	if err = v.UnmarshalExact(&c); err != nil {
		return c, fmt.Errorf("invalid agent config: %w", err)
	}
	return c, c.Validate()
}
