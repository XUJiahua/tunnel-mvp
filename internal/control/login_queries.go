package control

import "github.com/XUJiahua/tunnel-mvp/internal/state"

func (s *Store) userByUsername(name string) (User, bool) {
	if reader, ok := s.Repository.(state.LoginReader); ok {
		u, found, err := reader.UserByUsername(name)
		return u, found && err == nil
	}
	for _, u := range s.snapshot().Users {
		if u.Username == name {
			return u, true
		}
	}
	return User{}, false
}
func (s *Store) identityUsers(issuer, subject string) []User {
	if reader, ok := s.Repository.(state.LoginReader); ok {
		u, found, err := reader.UserByIdentity(state.Identity{Issuer: issuer, Subject: subject})
		if err == nil && found {
			return []User{u}
		}
		return nil
	}
	for _, u := range s.snapshot().Users {
		if u.Identity != nil && u.Identity.Issuer == issuer && u.Identity.Subject == subject {
			return []User{u}
		}
	}
	return nil
}
