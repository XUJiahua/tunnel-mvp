package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/lib/pq"
)

func pgOne(ctx context.Context, tx *sql.Tx, kind, where string, args ...any) (any, bool, error) {
	var id string
	var raw []byte
	err := tx.QueryRowContext(ctx, `SELECT id,payload FROM `+pgTable(kind)+` WHERE `+where, args...).Scan(&id, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	value, err := pgPayload(kind, id, raw)
	if err != nil {
		return nil, false, err
	}
	return value, true, nil
}
func (p *PostgresRepository) SiteByID(id string) (Site, bool, error) {
	var out Site
	var found bool
	err := p.read(context.Background(), func(ctx context.Context, tx *sql.Tx, _ pgMetadata) error {
		v, ok, err := pgOne(ctx, tx, "sites", "id=$1", id)
		if err == nil && ok {
			out = v.(Site)
			found = true
		}
		return err
	})
	if err != nil {
		return Site{}, false, err
	}
	return out, found, nil
}
func (p *PostgresRepository) ServiceByID(id string) (Service, bool, error) {
	return p.serviceByKey(id, false)
}
func (p *PostgresRepository) ServiceByDomain(domain string) (Service, bool, error) {
	return p.serviceByKey(domain, true)
}
func (p *PostgresRepository) serviceByKey(key string, domain bool) (Service, bool, error) {
	where := "id=$1"
	if domain {
		where = "custom_domain=$1"
	}
	var out Service
	var found bool
	err := p.read(context.Background(), func(ctx context.Context, tx *sql.Tx, _ pgMetadata) error {
		v, ok, err := pgOne(ctx, tx, "services", where, key)
		if err == nil && ok {
			out = v.(Service)
			found = true
		}
		return err
	})
	if err != nil {
		return Service{}, false, err
	}
	return out, found, nil
}
func (p *PostgresRepository) UserByUsername(username string) (User, bool, error) {
	return p.loginUser("username=$1", username)
}
func (p *PostgresRepository) UserByIdentity(id Identity) (User, bool, error) {
	return p.loginUser("oidc_issuer=$1 AND oidc_subject=$2", id.Issuer, id.Subject)
}
func (p *PostgresRepository) loginUser(where string, args ...any) (User, bool, error) {
	var out User
	var found bool
	err := p.read(context.Background(), func(ctx context.Context, tx *sql.Tx, _ pgMetadata) error {
		v, ok, err := pgOne(ctx, tx, "users", where, args...)
		if err == nil && ok {
			out = v.(User)
			found = true
		}
		return err
	})
	if err != nil {
		return User{}, false, err
	}
	return out, found, nil
}
func (p *PostgresRepository) UserContext(id string) (State, bool, error) {
	var out State
	var found bool
	err := p.read(context.Background(), func(ctx context.Context, tx *sql.Tx, _ pgMetadata) error {
		var err error
		out, found, err = pgUserContext(ctx, tx, id)
		return err
	})
	if err != nil {
		return State{}, false, err
	}
	return out, found, nil
}
func pgUserContext(ctx context.Context, tx *sql.Tx, id string) (State, bool, error) {
	var out State
	v, ok, err := pgOne(ctx, tx, "users", "id=$1", id)
	if err != nil || !ok {
		return State{}, false, err
	}
	u := v.(User)
	out.Users = []User{u}
	if u.RoleID != "" {
		v, ok, err = pgOne(ctx, tx, "roles", "id=$1", u.RoleID)
		if err != nil {
			return State{}, false, err
		}
		if !ok || v.(Role).OrganizationID != u.OrganizationID {
			return State{}, false, fmt.Errorf("invalid role projection")
		}
		out.Roles = []Role{v.(Role)}
	}
	rows, err := tx.QueryContext(ctx, `SELECT g.id,g.payload FROM tunnel_state.group_members m JOIN tunnel_state.groups g ON g.id=m.group_id WHERE m.user_id=$1 ORDER BY g.position`, id)
	if err != nil {
		return State{}, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var groupID string
		var raw []byte
		if err = rows.Scan(&groupID, &raw); err != nil {
			return State{}, false, err
		}
		v, err = pgPayload("groups", groupID, raw)
		if err != nil {
			return State{}, false, err
		}
		g := v.(Group)
		if g.OrganizationID != u.OrganizationID || !slices.Contains(g.MemberIDs, id) {
			return State{}, false, fmt.Errorf("invalid member projection")
		}
		out.Groups = append(out.Groups, g)
	}
	if err = rows.Err(); err != nil {
		return State{}, false, err
	}
	return out, true, nil
}
func pgDirectorySQL(q DirectoryQuery, after int) (string, []any) {
	args := []any{after}
	clauses := []string{"position>$1"}
	f := q.Filter
	bind := func(v any) string { args = append(args, v); return fmt.Sprintf("$%d", len(args)) }
	if q.Kind == "organizations" {
		clauses = append(clauses, "id<>''")
	}
	if !f.Global {
		column := "organization_id"
		if q.Kind == "organizations" {
			column = "id"
		}
		clauses = append(clauses, column+"="+bind(f.OrganizationID))
	}
	in := func(column string, ids []string) string {
		return column + "=ANY(" + bind(pq.Array(append([]string{}, ids...))) + "::text[])"
	}
	if q.Kind == "users" && f.ExcludeAdmins {
		clauses = append(clauses, "role<>'admin'")
	}
	if !f.Global && !f.All {
		switch q.Kind {
		case "services":
			clauses = append(clauses, "("+in("site_id", f.SiteIDs)+" OR "+in("id", f.ServiceIDs)+")")
		case "sites":
			sites := in("id", f.SiteIDs)
			services := "true"
			if !f.AllServices {
				services = in("svc.id", f.ServiceIDs)
			}
			clauses = append(clauses, "("+sites+" OR EXISTS(SELECT 1 FROM tunnel_state.services svc WHERE svc.site_id=sites.id AND "+services+"))")
		case "groups":
			clauses = append(clauses, "id IN (SELECT group_id FROM tunnel_state.group_members WHERE user_id="+bind(f.MemberID)+")")
		}
	}
	query := `SELECT id,position,payload FROM ` + pgTable(q.Kind) + ` WHERE ` + strings.Join(clauses, " AND ") + ` ORDER BY position`
	if q.Limit > 0 {
		query += " LIMIT " + bind(q.Limit+1)
	}
	return query, args
}
func (p *PostgresRepository) Directory(q DirectoryQuery) (DirectoryPage, error) {
	// Reject invalid identifiers before constructing any SQL.
	if _, err := directoryScope(q); err != nil {
		return DirectoryPage{}, err
	}
	var page DirectoryPage
	err := p.read(context.Background(), func(ctx context.Context, tx *sql.Tx, meta pgMetadata) error {
		if q.Filter.ExpectedRevision != nil && *q.Filter.ExpectedRevision != meta.Revision {
			return ErrDirectoryChanged
		}
		after, scope, err := directoryStart(q, meta.InstanceID, meta.Revision)
		if err != nil {
			return err
		}
		query, args := pgDirectorySQL(q, after)
		rows, err := tx.QueryContext(ctx, query, args...)
		if err != nil {
			return err
		}
		last, count, more := -1, 0, false
		for rows.Next() {
			if q.Limit > 0 && count == q.Limit {
				more = true
				break
			}
			var id string
			var pos int
			var raw []byte
			if err = rows.Scan(&id, &pos, &raw); err != nil {
				break
			}
			var v any
			v, err = pgPayload(q.Kind, id, raw)
			if err != nil {
				break
			}
			if g, ok := v.(Group); ok && !directoryMatch(State{}, g, q.Filter) {
				err = fmt.Errorf("invalid member directory")
				break
			}
			pgAppend(&page.State, v)
			last = pos
			count++
		}
		if err == nil {
			err = rows.Err()
		}
		closeErr := rows.Close()
		if err == nil {
			err = closeErr
		}
		if err != nil {
			return err
		}
		if q.Kind == "users" {
			ids := []string{}
			for _, u := range page.State.Users {
				if u.RoleID != "" {
					ids = append(ids, u.RoleID)
				}
			}
			if len(ids) > 0 {
				rows, err = tx.QueryContext(ctx, `SELECT id,payload FROM tunnel_state.roles WHERE id=ANY($1::text[]) ORDER BY position`, pq.Array(ids))
				if err != nil {
					return err
				}
				owners := map[string]string{}
				for rows.Next() {
					var id string
					var raw []byte
					if err = rows.Scan(&id, &raw); err != nil {
						break
					}
					v, decodeErr := pgPayload("roles", id, raw)
					if decodeErr != nil {
						err = decodeErr
						break
					}
					role := v.(Role)
					owners[id] = role.OrganizationID
					page.State.Roles = append(page.State.Roles, role)
				}
				if err == nil {
					err = rows.Err()
				}
				closeErr = rows.Close()
				if err == nil {
					err = closeErr
				}
				if err != nil {
					return err
				}
				for _, u := range page.State.Users {
					if u.RoleID != "" {
						org, ok := owners[u.RoleID]
						if !ok || org != u.OrganizationID {
							return fmt.Errorf("invalid directory role")
						}
					}
				}
			}
		}
		if more {
			page.NextCursor = directoryNext(meta.InstanceID, meta.Revision, scope, last)
		}
		return nil
	})
	if err != nil {
		return DirectoryPage{}, err
	}
	return page, nil
}
