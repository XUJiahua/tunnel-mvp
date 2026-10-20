package state

import "fmt"

// LoginReader is separate from UserReader so old repository adapters remain
// compatible. A failed point read must never fall back to a stale snapshot.
type LoginReader interface {
	UserByUsername(string) (User, bool, error)
	UserByIdentity(Identity) (User, bool, error)
}

func (s *Store) UserByUsername(username string) (User, bool, error) {
	return s.loginUser(username, nil)
}
func (s *Store) UserByIdentity(identity Identity) (User, bool, error) {
	return s.loginUser("", &identity)
}
func (s *Store) loginUser(username string, identity *Identity) (User, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return User{}, false, fmt.Errorf("state store is closed")
	}
	if identity == nil && username == "" || identity != nil && (identity.Issuer == "" || identity.Subject == "") {
		return User{}, false, nil
	}
	if b, ok := s.disk.(*sqliteBackend); ok {
		var u User
		var found bool
		var err error
		if identity == nil {
			u, found, err = b.readUser("username=?", username)
		} else {
			u, found, err = b.readUser("oidc_issuer=? AND oidc_subject=? AND oidc_issuer!=''", identity.Issuer, identity.Subject)
		}
		if err != nil {
			s.queryReadFailures.Add(1)
		}
		return u, found, err
	}
	for _, u := range s.state.Users {
		if identity == nil && u.Username == username || identity != nil && u.Identity != nil && *u.Identity == *identity {
			return Clone(State{Users: []User{u}}).Users[0], true, nil
		}
	}
	return User{}, false, nil
}
