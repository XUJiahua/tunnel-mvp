// Package state owns persistent configuration. Sessions, tunnels and audit logs
// have separate lifecycles and are deliberately not part of a state snapshot.
package state

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

type Identity struct {
	Issuer  string `json:"issuer"`
	Subject string `json:"subject"`
}

type User struct {
	AuthRevision   *uint64   `json:"-"`
	OrganizationID string    `json:"organizationID,omitempty"`
	RoleID         string    `json:"roleID,omitempty"`
	Permissions    []string  `json:"-"`
	ID             string    `json:"id"`
	Disabled       bool      `json:"disabled,omitempty"`
	Identity       *Identity `json:"identity,omitempty"`
	Username       string    `json:"username"`
	PasswordHash   string    `json:"passwordHash,omitempty"`
	Role           string    `json:"role"`
	SiteIDs        []string  `json:"siteIDs"`
	ServiceIDs     []string  `json:"serviceIDs"`
}
type Site struct {
	LegacyDisabled bool      `json:"legacyDisabled,omitempty"`
	OrganizationID string    `json:"organizationID,omitempty"`
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	TokenHash      string    `json:"tokenHash,omitempty"`
	CreatedAt      time.Time `json:"createdAt"`
}
type Service struct {
	ConnectionMode       string `json:"connectionMode,omitempty"`
	HealthPath           string `json:"healthPath,omitempty"`
	HealthMethod         string `json:"healthMethod,omitempty"`
	HealthExpectedStatus int    `json:"healthExpectedStatus,omitempty"`
	CustomDomain         string `json:"customDomain,omitempty"`
	OrganizationID       string `json:"organizationID,omitempty"`
	ID                   string `json:"id"`
	Name                 string `json:"name"`
	SiteID               string `json:"siteID"`
	URL                  string `json:"url"`
}
type Group struct {
	OrganizationID string   `json:"organizationID,omitempty"`
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	MemberIDs      []string `json:"memberIDs"`
	SiteIDs        []string `json:"siteIDs"`
	ServiceIDs     []string `json:"serviceIDs"`
}
type Invite struct {
	OrganizationID string    `json:"organizationID,omitempty"`
	ID             string    `json:"id"`
	Username       string    `json:"username"`
	TokenHash      string    `json:"tokenHash"`
	CreatedAt      time.Time `json:"createdAt"`
	ExpiresAt      time.Time `json:"expiresAt"`
	SiteIDs        []string  `json:"siteIDs"`
	ServiceIDs     []string  `json:"serviceIDs"`
	GroupIDs       []string  `json:"groupIDs"`
}
type Organization struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}
type Role struct {
	ID             string   `json:"id"`
	OrganizationID string   `json:"organizationID,omitempty"`
	Name           string   `json:"name"`
	Permissions    []string `json:"permissions"`
}
type State struct {
	Agents        []AgentRegistration `json:"agents,omitempty"`
	Organizations []Organization      `json:"organizations,omitempty"`
	Roles         []Role              `json:"roles,omitempty"`
	Groups        []Group             `json:"groups,omitempty"`
	Invites       []Invite            `json:"invites,omitempty"`
	Users         []User              `json:"users"`
	Sites         []Site              `json:"sites"`
	Services      []Service           `json:"services"`
}

func Clone(s State) State {
	// All fields are JSON-compatible; the copy prevents callbacks/callers from
	// retaining mutable references into the committed in-memory state.
	b, _ := json.Marshal(s)
	var out State
	_ = json.Unmarshal(b, &out)
	return out
}

var resourceID = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

func validName(s string) bool { return strings.TrimSpace(s) != "" && len(s) <= 100 }
func uniqueGrants(ids []string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, id := range ids {
		if !seen[id] {
			out = append(out, id)
			seen[id] = true
		}
	}
	return out
}

// Validate normalizes empty/duplicate grant lists and rejects malformed state
// before any write, import or migration can change persistent data.
func Validate(s *State) error {
	if err := validateOrganizations(s); err != nil {
		return err
	}
	if s.Users == nil {
		s.Users = []User{}
	}
	if s.Sites == nil {
		s.Sites = []Site{}
	}
	if s.Services == nil {
		s.Services = []Service{}
	}
	domains := map[string]bool{}
	sites, services, users, names := map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, v := range s.Sites {
		token, err := hex.DecodeString(v.TokenHash)
		if !resourceID.MatchString(v.ID) || sites[v.ID] || !validName(v.Name) || err != nil || len(token) != sha256.Size || v.CreatedAt.IsZero() {
			return fmt.Errorf("invalid or duplicate site %q", v.ID)
		}
		sites[v.ID] = true
	}
	for _, v := range s.Services {
		if v.ConnectionMode != "" && v.ConnectionMode != "session" {
			return fmt.Errorf("connectionMode must be empty or session")
		}
		_, err := ApplicationURL(v.URL)
		if !resourceID.MatchString(v.ID) || services[v.ID] || !validName(v.Name) || !sites[v.SiteID] || err != nil || ValidateHealth(v) != nil || v.CustomDomain != "" && (!ValidCustomDomain(v.CustomDomain) || domains[v.CustomDomain]) {
			return fmt.Errorf("invalid application or site reference %q", v.ID)
		}
		services[v.ID] = true
		if v.CustomDomain != "" {
			domains[v.CustomDomain] = true
		}
	}
	admins := 0
	identities := map[Identity]bool{}
	for i := range s.Users {
		v := &s.Users[i]
		if !resourceID.MatchString(v.ID) || users[v.ID] || !validName(v.Username) || names[v.Username] || (v.Role != "admin" && v.Role != "viewer") {
			return fmt.Errorf("invalid or duplicate user %q", v.ID)
		}
		if v.Identity != nil {
			identity := *v.Identity
			if err := ValidateIdentity(identity); err != nil || identities[identity] {
				return fmt.Errorf("invalid or duplicate OIDC identity for user %q", v.ID)
			}
			identities[identity] = true
		}
		if v.PasswordHash == "" && v.Identity == nil {
			return fmt.Errorf("user %q has no login method", v.ID)
		}
		if v.PasswordHash != "" {
			if _, err := bcrypt.Cost([]byte(v.PasswordHash)); err != nil {
				return fmt.Errorf("invalid password hash for user %q", v.ID)
			}
		}
		if v.Role == "admin" && !v.Disabled && v.PasswordHash != "" {
			admins++
		}
		users[v.ID] = true
		names[v.Username] = true
		v.SiteIDs = uniqueGrants(v.SiteIDs)
		v.ServiceIDs = uniqueGrants(v.ServiceIDs)
		for _, id := range v.SiteIDs {
			if !sites[id] {
				return fmt.Errorf("unknown site grant %q for user %q", id, v.ID)
			}
		}
		for _, id := range v.ServiceIDs {
			if !services[id] {
				return fmt.Errorf("unknown application grant %q for user %q", id, v.ID)
			}
		}
	}
	if admins == 0 {
		return fmt.Errorf("state must contain at least one enabled local-password administrator")
	}
	groupIDs := map[string]bool{}
	for i := range s.Groups {
		v := &s.Groups[i]
		if !resourceID.MatchString(v.ID) || groupIDs[v.ID] || !validName(v.Name) {
			return fmt.Errorf("invalid or duplicate group %q", v.ID)
		}
		groupIDs[v.ID] = true
		v.MemberIDs = uniqueGrants(v.MemberIDs)
		v.SiteIDs = uniqueGrants(v.SiteIDs)
		v.ServiceIDs = uniqueGrants(v.ServiceIDs)
		for _, id := range v.MemberIDs {
			if !users[id] {
				return fmt.Errorf("unknown group member %q", id)
			}
		}
		for _, id := range v.SiteIDs {
			if !sites[id] {
				return fmt.Errorf("unknown group site %q", id)
			}
		}
		for _, id := range v.ServiceIDs {
			if !services[id] {
				return fmt.Errorf("unknown group application %q", id)
			}
		}
	}
	inviteIDs, inviteTokens := map[string]bool{}, map[string]bool{}
	for i := range s.Invites {
		v := &s.Invites[i]
		hash, err := hex.DecodeString(v.TokenHash)
		if !resourceID.MatchString(v.ID) || inviteIDs[v.ID] || inviteTokens[v.TokenHash] || !validName(v.Username) || names[v.Username] || err != nil || len(hash) != sha256.Size || v.CreatedAt.IsZero() || !v.ExpiresAt.After(v.CreatedAt) || v.ExpiresAt.Sub(v.CreatedAt) > 7*24*time.Hour {
			return fmt.Errorf("invalid or duplicate invitation %q", v.ID)
		}
		inviteIDs[v.ID], inviteTokens[v.TokenHash], names[v.Username] = true, true, true
		v.SiteIDs = uniqueGrants(v.SiteIDs)
		v.ServiceIDs = uniqueGrants(v.ServiceIDs)
		v.GroupIDs = uniqueGrants(v.GroupIDs)
		for _, id := range v.SiteIDs {
			if !sites[id] {
				return fmt.Errorf("unknown invitation site %q", id)
			}
		}
		for _, id := range v.ServiceIDs {
			if !services[id] {
				return fmt.Errorf("unknown invitation application %q", id)
			}
		}
		for _, id := range v.GroupIDs {
			if !groupIDs[id] {
				return fmt.Errorf("unknown invitation group %q", id)
			}
		}
	}
	if err := validateAgents(s); err != nil {
		return err
	}
	return validateOrganizationReferences(s)
}

// Issuers are exact identifiers, not normalized origins. Subjects are opaque and
// case-sensitive. Never trim/alias them or substitute an email/display name.
func ValidateIdentity(v Identity) error {
	u, err := url.Parse(v.Issuer)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || (u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "localhost" || net.ParseIP(u.Hostname()).IsLoopback()))) || len(v.Issuer) > 2048 || v.Subject == "" || len(v.Subject) > 255 || strings.TrimSpace(v.Subject) != v.Subject {
		return fmt.Errorf("valid HTTPS issuer and nonempty subject required (HTTP allowed only on loopback)")
	}
	return nil
}
