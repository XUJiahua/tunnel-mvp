package config

import (
	"fmt"
	"net"
	"net/url"
	"strings"

	"github.com/XUJiahua/tunnel-mvp/internal/state"
)

type Logging struct {
	LogLevel  string `json:"logLevel,omitempty" mapstructure:"logLevel"`
	LogFormat string `json:"logFormat,omitempty" mapstructure:"logFormat"`
}

func (l *Logging) Validate() error {
	if l.LogLevel == "" {
		l.LogLevel = "info"
	}
	if l.LogFormat == "" {
		l.LogFormat = "text"
	}
	switch l.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("logLevel must be debug, info, warn or error")
	}
	if l.LogFormat != "text" && l.LogFormat != "json" {
		return fmt.Errorf("logFormat must be text or json")
	}
	return nil
}

type KeycloakSync struct {
	Enabled         bool   `json:"enabled" mapstructure:"enabled"`
	ClientID        string `json:"clientID" mapstructure:"clientID"`
	SubjectIsUserID bool   `json:"subjectIsUserID" mapstructure:"subjectIsUserID"`
	PollSeconds     int    `json:"pollSeconds" mapstructure:"pollSeconds"`
	MaxStaleSeconds int    `json:"maxStaleSeconds" mapstructure:"maxStaleSeconds"`
}
type OIDC struct {
	KeycloakSync   KeycloakSync `json:"keycloakSync,omitempty" mapstructure:"keycloakSync"`
	RequiredACR    []string     `json:"requiredACR,omitempty" mapstructure:"requiredACR"`
	RequiredAMR    []string     `json:"requiredAMR,omitempty" mapstructure:"requiredAMR"`
	Issuer         string       `json:"issuer" mapstructure:"issuer"`
	ClientID       string       `json:"clientID" mapstructure:"clientID"`
	SessionMinutes int          `json:"sessionMinutes" mapstructure:"sessionMinutes"`
}
type Server struct {
	OIDC                 OIDC `json:"oidc" mapstructure:"oidc"`
	Logging              `mapstructure:",squash"`
	Listen               string `json:"listen" mapstructure:"listen"`
	PublicURL            string `json:"publicURL" mapstructure:"publicURL"`
	AccessURL            string `json:"accessURL" mapstructure:"accessURL"`
	Storage              string `json:"storage" mapstructure:"storage"`
	DataDir              string `json:"dataDir" mapstructure:"dataDir"`
	ProbeIntervalSeconds int    `json:"probeIntervalSeconds" mapstructure:"probeIntervalSeconds"`
	AuditMaxMB           int    `json:"auditMaxMB" mapstructure:"auditMaxMB"`
	AuditBackups         int    `json:"auditBackups" mapstructure:"auditBackups"`
	AdminUsername        string `json:"adminUsername" mapstructure:"adminUsername"`
}
type Rule = state.NetworkRule

type Agent struct {
	RemotePolicy   bool   `json:"remotePolicy,omitempty" mapstructure:"remotePolicy"`
	InstanceID     string `json:"instanceID,omitempty" mapstructure:"instanceID"`
	RuntimeVersion string `json:"-" mapstructure:"-"`
	Logging        `mapstructure:",squash"`
	ServerURL      string `json:"serverURL" mapstructure:"serverURL"`
	SiteID         string `json:"siteID" mapstructure:"siteID"`
	Allow          []Rule `json:"allow" mapstructure:"allow"`
}

func Origin(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, fmt.Errorf("expected an http(s) origin: %q", raw)
	}
	u.Path = ""
	u.Host = strings.ToLower(u.Host)
	if (u.Scheme == "https" && u.Port() == "443") || (u.Scheme == "http" && u.Port() == "80") {
		u.Host = u.Hostname()
		if strings.Contains(u.Host, ":") {
			u.Host = "[" + u.Host + "]"
		}
	}
	return u, nil
}
func (s *Server) Validate() error {
	if s.OIDC.Issuer != "" || s.OIDC.ClientID != "" {
		for _, value := range append(append([]string{}, s.OIDC.RequiredACR...), s.OIDC.RequiredAMR...) {
			if strings.TrimSpace(value) == "" || len(value) > 255 {
				return fmt.Errorf("OIDC required ACR/AMR values must be nonempty and at most 255 bytes")
			}
		}
		if err := state.ValidateIdentity(state.Identity{Issuer: s.OIDC.Issuer, Subject: "config"}); err != nil || strings.TrimSpace(s.OIDC.ClientID) == "" || len(s.OIDC.ClientID) > 255 {
			return fmt.Errorf("OIDC requires a valid issuer and clientID")
		}
		if s.OIDC.SessionMinutes == 0 {
			s.OIDC.SessionMinutes = 60
		}
		if s.OIDC.SessionMinutes < 1 || s.OIDC.SessionMinutes > 720 {
			return fmt.Errorf("oidc.sessionMinutes must be 1–720")
		}
	} else if s.OIDC.SessionMinutes != 0 || len(s.OIDC.RequiredACR) > 0 || len(s.OIDC.RequiredAMR) > 0 {
		return fmt.Errorf("OIDC sessionMinutes requires issuer and clientID")
	}

	if k := &s.OIDC.KeycloakSync; k.Enabled {
		if s.OIDC.Issuer == "" || strings.TrimSpace(k.ClientID) == "" || len(k.ClientID) > 255 || !k.SubjectIsUserID {
			return fmt.Errorf("keycloakSync requires OIDC, a service clientID and explicit subjectIsUserID=true")
		}
		if k.PollSeconds == 0 {
			k.PollSeconds = 15
		}
		if k.MaxStaleSeconds == 0 {
			k.MaxStaleSeconds = 60
		}
		if k.PollSeconds < 1 || k.PollSeconds > 300 || k.MaxStaleSeconds < 2*k.PollSeconds+5 || k.MaxStaleSeconds > 3600 {
			return fmt.Errorf("keycloakSync requires pollSeconds 1–300 and maxStaleSeconds >= 2*pollSeconds+5, <=3600")
		}
	}
	if s.Storage == "" {
		s.Storage = "json"
	}
	if s.Storage != "json" && s.Storage != "sqlite" && s.Storage != "postgres" {
		return fmt.Errorf("storage must be json, sqlite or postgres")
	}
	if s.ProbeIntervalSeconds < 0 || (s.ProbeIntervalSeconds > 0 && s.ProbeIntervalSeconds < 10) || s.ProbeIntervalSeconds > 86400 {
		return fmt.Errorf("probeIntervalSeconds must be 0 (disabled) or 10–86400")
	}
	if s.AuditMaxMB == 0 {
		s.AuditMaxMB = 20
	}
	if s.AuditBackups == 0 {
		s.AuditBackups = 5
	}
	if s.AuditMaxMB < 1 || s.AuditMaxMB > 1024 || s.AuditBackups < 1 || s.AuditBackups > 20 {
		return fmt.Errorf("auditMaxMB must be 1–1024; auditBackups must be 1–20")
	}
	if err := s.Logging.Validate(); err != nil {
		return err
	}
	p, err := Origin(s.PublicURL)
	if err != nil {
		return err
	}
	a, err := Origin(s.AccessURL)
	if err != nil {
		return err
	}
	if strings.EqualFold(p.Hostname(), a.Hostname()) || strings.HasSuffix(strings.ToLower(p.Hostname()), "."+strings.ToLower(a.Hostname())) {
		return fmt.Errorf("console must not be under access domain")
	}
	if p.Scheme != a.Scheme {
		return fmt.Errorf("console and access must use the same scheme")
	}
	if s.OIDC.Issuer != "" && p.Scheme != "https" && p.Hostname() != "localhost" && !net.ParseIP(p.Hostname()).IsLoopback() {
		return fmt.Errorf("OIDC publicURL requires HTTPS (HTTP allowed only on loopback)")
	}
	s.PublicURL = p.String()
	s.AccessURL = a.String()
	if s.Listen == "" {
		s.Listen = ":8080"
	}
	if s.DataDir == "" {
		s.DataDir = "data"
	}
	if s.AdminUsername == "" {
		s.AdminUsername = "admin"
	}
	return nil
}

func (a *Agent) Validate() error {
	if err := a.Logging.Validate(); err != nil {
		return err
	}
	origin, err := Origin(a.ServerURL)
	if err != nil {
		return err
	}
	a.ServerURL = origin.String()
	if a.RemotePolicy && a.InstanceID == "" {
		return fmt.Errorf("remotePolicy requires an independently registered instanceID")
	}
	if a.InstanceID != "" && !state.ValidAgentID(a.InstanceID) {
		return fmt.Errorf("invalid registered Agent instance ID")
	}
	if a.SiteID == "" {
		return fmt.Errorf("siteID is required (config, TUNNEL_SITE_ID or --site-id)")
	}
	if len(a.Allow) == 0 {
		return fmt.Errorf("Agent allow list must not be empty")
	}
	return nil
}

// TargetURL preserves a validated application path prefix; control/Agent origins remain path-free.
func TargetURL(raw string) (*url.URL, error) { return state.ApplicationURL(raw) }
