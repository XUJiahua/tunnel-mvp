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
	ID           string    `json:"id"`
	Disabled     bool      `json:"disabled,omitempty"`
	Identity     *Identity `json:"identity,omitempty"`
	Username     string    `json:"username"`
	PasswordHash string    `json:"passwordHash,omitempty"`
	Role         string    `json:"role"`
	SiteIDs      []string  `json:"siteIDs"`
	ServiceIDs   []string  `json:"serviceIDs"`
}
type Site struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	TokenHash string    `json:"tokenHash,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
}
type Service struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	SiteID string `json:"siteID"`
	URL    string `json:"url"`
}
type State struct {
	Users    []User    `json:"users"`
	Sites    []Site    `json:"sites"`
	Services []Service `json:"services"`
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
	if s.Users == nil {
		s.Users = []User{}
	}
	if s.Sites == nil {
		s.Sites = []Site{}
	}
	if s.Services == nil {
		s.Services = []Service{}
	}
	sites, services, users, names := map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, v := range s.Sites {
		token, err := hex.DecodeString(v.TokenHash)
		if !resourceID.MatchString(v.ID) || sites[v.ID] || !validName(v.Name) || err != nil || len(token) != sha256.Size || v.CreatedAt.IsZero() {
			return fmt.Errorf("invalid or duplicate site %q", v.ID)
		}
		sites[v.ID] = true
	}
	for _, v := range s.Services {
		target, err := url.Parse(v.URL)
		if !resourceID.MatchString(v.ID) || services[v.ID] || !validName(v.Name) || !sites[v.SiteID] || err != nil || target.Hostname() == "" || (target.Scheme != "http" && target.Scheme != "https") || target.User != nil || target.RawQuery != "" || target.ForceQuery || target.Fragment != "" || (target.Path != "" && target.Path != "/") {
			return fmt.Errorf("invalid application or site reference %q", v.ID)
		}
		services[v.ID] = true
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
	return nil
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
