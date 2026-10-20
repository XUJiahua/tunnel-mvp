package config

import (
	"fmt"
	"net"
	"net/url"

	"github.com/XUJiahua/tunnel-mvp/internal/state"
)

type Cluster struct {
	Enabled      bool   `json:"enabled" mapstructure:"enabled"`
	NodeID       string `json:"nodeID" mapstructure:"nodeID"`
	Listen       string `json:"listen" mapstructure:"listen"`
	AdvertiseURL string `json:"advertiseURL" mapstructure:"advertiseURL"`
	CAFile       string `json:"caFile" mapstructure:"caFile"`
	CertFile     string `json:"certFile" mapstructure:"certFile"`
	KeyFile      string `json:"keyFile" mapstructure:"keyFile"`
}

func (c *Cluster) validate(storage string) error {
	if !c.Enabled {
		if c.NodeID != "" || c.Listen != "" || c.AdvertiseURL != "" || c.CAFile != "" || c.CertFile != "" || c.KeyFile != "" {
			return fmt.Errorf("cluster fields require cluster.enabled")
		}
		return nil
	}
	if storage != state.Postgres {
		return fmt.Errorf("cluster requires PostgreSQL")
	}
	if !state.ValidClusterNodeID(c.NodeID) || c.CAFile == "" || c.CertFile == "" || c.KeyFile == "" {
		return fmt.Errorf("cluster requires a valid nodeID, caFile, certFile and keyFile")
	}
	if _, _, err := net.SplitHostPort(c.Listen); err != nil {
		return fmt.Errorf("cluster.listen requires an explicit host:port")
	}
	u, err := url.Parse(c.AdvertiseURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Path != "" {
		return fmt.Errorf("cluster.advertiseURL must be an HTTPS origin without a path")
	}
	return nil
}
