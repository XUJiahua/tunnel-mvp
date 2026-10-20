package state

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"time"
)

type sqlRow struct {
	id     string
	values []any
}
type sqlTable struct {
	name string
	rows []sqlRow
}

func stateRows(s State) ([]sqlTable, error) {
	tables := []sqlTable{{name: "users"}, {name: "sites"}, {name: "services"}, {name: "organizations"}, {name: "roles"}, {name: "groups"}, {name: "invites"}, {name: "agents"}}
	for i, v := range s.Users {
		issuer, subject := "", ""
		if v.Identity != nil {
			issuer, subject = v.Identity.Issuer, v.Identity.Subject
		}
		tables[0].rows = append(tables[0].rows, sqlRow{v.ID, []any{v.ID, i, v.Username, v.PasswordHash, v.Role, v.Disabled, issuer, subject, v.OrganizationID, v.RoleID}})
	}
	for i, v := range s.Sites {
		tables[1].rows = append(tables[1].rows, sqlRow{v.ID, []any{v.ID, i, v.Name, v.TokenHash, v.CreatedAt.Format(time.RFC3339Nano), v.OrganizationID, v.LegacyDisabled}})
	}
	for i, v := range s.Services {
		tables[2].rows = append(tables[2].rows, sqlRow{v.ID, []any{v.ID, i, v.Name, v.SiteID, v.URL, v.OrganizationID, v.CustomDomain, v.HealthPath, v.HealthMethod, v.HealthExpectedStatus, v.ConnectionMode}})
	}
	payload := func(table int, id string, position int, value any) error {
		raw, e := json.Marshal(value)
		if e != nil {
			return e
		}
		tables[table].rows = append(tables[table].rows, sqlRow{id, []any{id, position, string(raw)}})
		return nil
	}
	for i, v := range s.Organizations {
		if e := payload(3, v.ID, i, v); e != nil {
			return nil, e
		}
	}
	for i, v := range s.Roles {
		if e := payload(4, v.ID, i, v); e != nil {
			return nil, e
		}
	}
	for i, v := range s.Groups {
		if e := payload(5, v.ID, i, v); e != nil {
			return nil, e
		}
	}
	for i, v := range s.Invites {
		if e := payload(6, v.ID, i, v); e != nil {
			return nil, e
		}
	}
	for i, v := range s.Agents {
		if e := payload(7, v.ID, i, v); e != nil {
			return nil, e
		}
	}
	return tables, nil
}
func rowsByID(rows []sqlRow) map[string][]any {
	out := map[string][]any{}
	for _, row := range rows {
		out[row.id] = row.values
	}
	return out
}

type userGrants struct{ siteIDs, serviceIDs []string }

func grantRows(users []User) map[string]userGrants {
	out := map[string]userGrants{}
	for _, u := range users {
		out[u.ID] = userGrants{siteIDs: append([]string{}, u.SiteIDs...), serviceIDs: append([]string{}, u.ServiceIDs...)}
	}
	return out
}
func (b *sqliteBackend) cache(s State) error {
	rows, e := stateRows(s)
	if e != nil {
		return e
	}
	b.rows = rows
	b.grants = grantRows(s.Users)
	b.members = membershipRows(s.Groups)
	return nil
}
func placeholders(n int) string {
	value := "?"
	for i := 1; i < n; i++ {
		value += ",?"
	}
	return value
}
func (b *sqliteBackend) save(next State) error {
	before := b.rows
	if before == nil {
		before, _ = stateRows(State{})
	}
	after, e := stateRows(next)
	if e != nil {
		return e
	}
	tx, e := b.db.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	// Defer, never disable, foreign-key enforcement. Changed parent rows may be
	// replaced while unchanged grant rows retain their stable references.
	if _, e = tx.Exec("PRAGMA defer_foreign_keys=ON"); e != nil {
		return e
	}
	changed := make([]map[string]bool, len(after))
	for i, table := range after {
		oldRows, newRows := rowsByID(before[i].rows), rowsByID(table.rows)
		changed[i] = map[string]bool{}
		for id, old := range oldRows {
			fresh, exists := newRows[id]
			if !exists || !slices.Equal(old, fresh) {
				changed[i][id] = true
				if _, e = tx.Exec("DELETE FROM "+table.name+" WHERE id=?", id); e != nil {
					return e
				}
			}
		}
		for id, fresh := range newRows {
			if old, exists := oldRows[id]; !exists || !slices.Equal(old, fresh) {
				changed[i][id] = true
			}
		}
	}
	// Delete all changed unique values before reinsertion. Username/issuer-sub
	// swaps and position reorders therefore remain one valid atomic transaction.
	for i, table := range after {
		for _, row := range table.rows {
			if !changed[i][row.id] {
				continue
			}
			if _, e = tx.Exec("INSERT INTO "+table.name+" VALUES("+placeholders(len(row.values))+")", row.values...); e != nil {
				return e
			}
		}
	}
	grants := grantRows(next.Users)
	if e = saveGrantDelta(tx, "user_sites", "site_id", b.grants, grants, func(u userGrants) []string { return u.siteIDs }); e != nil {
		return e
	}
	if e = saveGrantDelta(tx, "user_services", "service_id", b.grants, grants, func(u userGrants) []string { return u.serviceIDs }); e != nil {
		return e
	}
	members := membershipRows(next.Groups)
	if e = saveMembershipDelta(tx, b.members, members); e != nil {
		return e
	}
	if e = tx.Commit(); e != nil {
		return e
	}
	b.rows = after
	b.grants = grants
	b.members = members
	return nil
}
func saveGrantDelta(tx *sql.Tx, table, column string, before, after map[string]userGrants, ids func(userGrants) []string) error {
	old, newUsers := before, after
	for id, user := range old {
		fresh, exists := newUsers[id]
		if !exists || !slices.Equal(ids(user), ids(fresh)) {
			if _, e := tx.Exec("DELETE FROM "+table+" WHERE user_id=?", id); e != nil {
				return e
			}
		}
	}
	for userID, user := range after {
		prior, exists := old[userID]
		if exists && slices.Equal(ids(prior), ids(user)) {
			continue
		}
		for i, id := range ids(user) {
			if _, e := tx.Exec(fmt.Sprintf("INSERT INTO %s(user_id,%s,position) VALUES(?,?,?)", table, column), userID, id, i); e != nil {
				return e
			}
		}
	}
	return nil
}
