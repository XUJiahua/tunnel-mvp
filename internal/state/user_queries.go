package state

import (
	"database/sql"
	"errors"
	"fmt"
)

// UserReader returns an authorization projection: one user, its role and only
// groups containing that user. It is not a complete State for Update/Validate.
type UserReader interface {
	UserContext(string) (State, bool, error)
}

// QueryReadFailures counts failed database projections, excluding not-found and
// calls rejected after Close. It has process lifetime and is safe during reads.
func (s *Store) QueryReadFailures() uint64 { return s.queryReadFailures.Load() }

func (s *Store) UserContext(id string) (State, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return State{}, false, fmt.Errorf("state store is closed")
	}
	if id == "" {
		return State{}, false, nil
	}
	if b, ok := s.disk.(*sqliteBackend); ok {
		st, found, err := b.userContext(id)
		if err != nil {
			s.queryReadFailures.Add(1)
		}
		return st, found, err
	}
	for _, u := range s.state.Users {
		if u.ID == id {
			out := State{Users: []User{u}}
			for _, r := range s.state.Roles {
				if r.ID == u.RoleID && r.OrganizationID == u.OrganizationID {
					out.Roles = append(out.Roles, r)
				}
			}
			for _, g := range s.state.Groups {
				for _, member := range g.MemberIDs {
					if member == id {
						out.Groups = append(out.Groups, g)
						break
					}
				}
			}
			return Clone(out), true, nil
		}
	}
	return State{}, false, nil
}
func (b *sqliteBackend) userContext(id string) (State, bool, error) {
	var u User
	var issuer, subject string
	err := b.db.QueryRow("SELECT id,username,password_hash,role,disabled,oidc_issuer,oidc_subject,organization_id,role_id FROM users WHERE id=?", id).Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Role, &u.Disabled, &issuer, &subject, &u.OrganizationID, &u.RoleID)
	if errors.Is(err, sql.ErrNoRows) {
		return State{}, false, nil
	}
	if err != nil {
		return State{}, false, err
	}
	if issuer != "" || subject != "" {
		u.Identity = &Identity{issuer, subject}
	}
	u.SiteIDs, err = b.userGrantIDs("user_sites", "site_id", id)
	if err != nil {
		return State{}, false, err
	}
	u.ServiceIDs, err = b.userGrantIDs("user_services", "service_id", id)
	if err != nil {
		return State{}, false, err
	}
	st := State{Users: []User{u}}
	if u.RoleID != "" {
		var raw string
		err = b.db.QueryRow("SELECT payload FROM roles WHERE id=?", u.RoleID).Scan(&raw)
		if err != nil {
			return State{}, false, err
		}
		var role Role
		if err = decodeStrict([]byte(raw), &role); err != nil || role.ID != u.RoleID || role.OrganizationID != u.OrganizationID {
			return State{}, false, fmt.Errorf("invalid user role projection")
		}
		st.Roles = []Role{role}
	}
	rows, err := b.db.Query("SELECT id,payload FROM groups WHERE EXISTS (SELECT 1 FROM json_each(groups.payload,'$.memberIDs') AS member WHERE member.value=?) ORDER BY position", id)
	if err != nil {
		return State{}, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var groupID, raw string
		if err = rows.Scan(&groupID, &raw); err != nil {
			return State{}, false, err
		}
		var g Group
		if err = decodeStrict([]byte(raw), &g); err != nil || g.ID != groupID || g.OrganizationID != u.OrganizationID {
			return State{}, false, fmt.Errorf("invalid user group projection")
		}
		st.Groups = append(st.Groups, g)
	}
	if err = rows.Err(); err != nil {
		return State{}, false, err
	}
	return st, true, nil
}

// Table/column identifiers are compile-time constants supplied only above.
func (b *sqliteBackend) userGrantIDs(table, column, id string) ([]string, error) {
	out := []string{}
	rows, err := b.db.Query("SELECT "+column+" FROM "+table+" WHERE user_id=? ORDER BY position", id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var grant string
		if err = rows.Scan(&grant); err != nil {
			return nil, err
		}
		out = append(out, grant)
	}
	return out, rows.Err()
}
