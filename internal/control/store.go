package control

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

type User struct {
	ID           string   `json:"id"`
	Username     string   `json:"username"`
	PasswordHash string   `json:"passwordHash,omitempty"`
	Role         string   `json:"role"`
	SiteIDs      []string `json:"siteIDs"`
	ServiceIDs   []string `json:"serviceIDs"`
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
type Store struct {
	mu    sync.RWMutex
	path  string
	state State
}

func randomID() string {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
func digest(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
func OpenStore(dir, username, password string) (*Store, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	s := &Store{path: filepath.Join(dir, "state.json"), state: State{Users: []User{}, Sites: []Site{}, Services: []Service{}}}
	b, err := os.ReadFile(s.path)
	if err == nil {
		if err = json.Unmarshal(b, &s.state); err != nil {
			return nil, err
		}
		return s, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if len(password) < 12 || len(password) > 72 {
		return nil, fmt.Errorf("first start requires TUNNEL_ADMIN_PASSWORD (12–72 bytes)")
	}
	h, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}
	s.state.Users = append(s.state.Users, User{ID: randomID()[:16], Username: username, PasswordHash: string(h), Role: "admin", SiteIDs: []string{}})
	return s, s.save(s.state)
}
func (s *Store) save(v State) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(s.path), "state-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), s.path)
}
func (s *Store) snapshot() State {
	s.mu.RLock()
	defer s.mu.RUnlock()
	b, _ := json.Marshal(s.state)
	var out State
	_ = json.Unmarshal(b, &out)
	return out
}
func (s *Store) update(fn func(*State) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, _ := json.Marshal(s.state)
	var next State
	_ = json.Unmarshal(b, &next)
	if err := fn(&next); err != nil {
		return err
	}
	if err := s.save(next); err != nil {
		return err
	}
	s.state = next
	return nil
}
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
