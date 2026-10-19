package control

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"github.com/XUJiahua/tunnel-mvp/internal/state"
	"golang.org/x/crypto/bcrypt"
)

type User = state.User
type Site = state.Site
type Service = state.Service
type State = state.State
type Store struct{ state.Repository }

func randomID() string {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
func digest(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
func OpenStore(dir, username, password string) (*Store, error) {
	return openStore(dir, state.JSON, username, password)
}
func openStore(dir, kind, username, password string) (*Store, error) {
	repo, err := state.Open(dir, kind, func() (State, error) {
		if len(password) < 12 || len(password) > 72 {
			return State{}, fmt.Errorf("first start requires TUNNEL_ADMIN_PASSWORD (12–72 bytes)")
		}
		h, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
		if err != nil {
			return State{}, err
		}
		return State{Users: []User{{ID: randomID()[:16], Username: username, PasswordHash: string(h), Role: "admin", SiteIDs: []string{}, ServiceIDs: []string{}}}}, nil
	})
	if err != nil {
		return nil, err
	}
	return &Store{repo}, nil
}
func (s *Store) snapshot() State                    { return s.Snapshot() }
func (s *Store) update(fn func(*State) error) error { return s.Update(fn) }
func (s *Store) user(id string) (User, bool) {
	for _, u := range s.snapshot().Users {
		if u.ID == id {
			return u, true
		}
	}
	return User{}, false
}
func (s *Store) site(id string) (Site, bool) {
	for _, v := range s.snapshot().Sites {
		if v.ID == id {
			return v, true
		}
	}
	return Site{}, false
}
func (s *Store) service(id string) (Service, bool) {
	for _, v := range s.snapshot().Services {
		if v.ID == id {
			return v, true
		}
	}
	return Service{}, false
}
func canAccess(u User, siteID string) bool {
	if u.Role == "admin" {
		return true
	}
	for _, id := range u.SiteIDs {
		if id == siteID {
			return true
		}
	}
	return false
}

// Site grants cover all current and future applications in that site. Explicit
// service grants are additive and never grant access to sibling applications.
func canAccessService(u User, svc Service) bool {
	if canAccess(u, svc.SiteID) {
		return true
	}
	for _, id := range u.ServiceIDs {
		if id == svc.ID {
			return true
		}
	}
	return false
}
