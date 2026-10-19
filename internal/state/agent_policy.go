package state

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/netip"
	"strings"
)

type NetworkRule struct {
	CIDR  string `json:"cidr" mapstructure:"cidr"`
	Host  string `json:"host,omitempty" mapstructure:"host"`
	Ports []int  `json:"ports" mapstructure:"ports"`
}
type AgentPolicy struct {
	Version string        `json:"version"`
	Allow   []NetworkRule `json:"allow"`
}

func PolicyVersion(rules []NetworkRule) string {
	b, _ := json.Marshal(rules)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func ValidateAgentPolicy(p *AgentPolicy) error {
	if p == nil {
		return nil
	}
	if p.Allow == nil || len(p.Allow) > 64 || p.Version != PolicyVersion(p.Allow) {
		return fmt.Errorf("invalid Agent policy version or rule count")
	}
	for _, r := range p.Allow {
		if _, e := netip.ParsePrefix(r.CIDR); e != nil {
			return fmt.Errorf("invalid policy CIDR")
		}
		if len(r.Host) > 253 || strings.TrimSpace(r.Host) != r.Host || strings.ContainsAny(r.Host, "/:\\\x00\r\n\t ") {
			return fmt.Errorf("invalid policy hostname")
		}
		if len(r.Ports) == 0 || len(r.Ports) > 64 {
			return fmt.Errorf("policy rules require 1–64 ports")
		}
		for _, port := range r.Ports {
			if port < 1 || port > 65535 {
				return fmt.Errorf("invalid policy port")
			}
		}
	}
	return nil
}
func EmptyAgentPolicy() AgentPolicy {
	rules := []NetworkRule{}
	return AgentPolicy{Version: PolicyVersion(rules), Allow: rules}
}
