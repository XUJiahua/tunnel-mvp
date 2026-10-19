package state

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"
)

type AgentRegistration struct {
	RequiresRotation bool      `json:"requiresRotation,omitempty"`
	ID               string    `json:"id"`
	SiteID           string    `json:"siteID"`
	Name             string    `json:"name"`
	TokenHash        string    `json:"tokenHash"`
	Disabled         bool      `json:"disabled,omitempty"`
	CreatedAt        time.Time `json:"createdAt"`
}

func ValidAgentID(id string) bool { return id != "legacy" && resourceID.MatchString(id) }
func validateAgents(s *State) error {
	counts := map[string]int{}
	sites, ids, tokens := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, site := range s.Sites {
		sites[site.ID] = true
		tokens[site.TokenHash] = true
	}
	for _, a := range s.Agents {
		hash, err := hex.DecodeString(a.TokenHash)
		if !ValidAgentID(a.ID) || ids[a.ID] || !sites[a.SiteID] || !validName(a.Name) || err != nil || len(hash) != sha256.Size || tokens[a.TokenHash] || a.CreatedAt.IsZero() || (a.RequiresRotation && !a.Disabled) {
			return fmt.Errorf("invalid agent registration or site reference")
		}
		counts[a.SiteID]++
		if counts[a.SiteID] > 16 {
			return fmt.Errorf("at most 16 registered Agents per site")
		}
		ids[a.ID], tokens[a.TokenHash] = true, true
	}
	return nil
}
