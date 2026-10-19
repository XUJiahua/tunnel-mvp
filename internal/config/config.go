package config

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
)

type Server struct {
	Listen        string `json:"listen"`
	PublicURL     string `json:"publicURL"`
	AccessURL     string `json:"accessURL"`
	DataDir       string `json:"dataDir"`
	AdminUsername string `json:"adminUsername"`
}
type Rule struct {
	CIDR  string `json:"cidr"`
	Host  string `json:"host,omitempty"`
	Ports []int  `json:"ports"`
}
type Agent struct {
	ServerURL string `json:"serverURL"`
	SiteID    string `json:"siteID"`
	Allow     []Rule `json:"allow"`
}

func Load(path string, v any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	d := json.NewDecoder(f)
	d.DisallowUnknownFields()
	return d.Decode(v)
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
