package state

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

func directorySQL(q DirectoryQuery, after int) (string, []any) {
	table := q.Kind
	if table == "invitations" {
		table = "invites"
	}
	clauses := []string{"position>?"}
	args := []any{after}
	f := q.Filter
	org := "organization_id"
	switch q.Kind {
	case "groups", "roles", "invitations":
		org = "coalesce(json_extract(payload,'$.organizationID'),'')"
	case "organizations":
		org = "id"
	}
	if !f.Global {
		clauses = append(clauses, org+"=?")
		args = append(args, f.OrganizationID)
	}
	in := func(column string, ids []string) string {
		raw, _ := json.Marshal(append([]string{}, ids...))
		args = append(args, string(raw))
		return column + " IN (SELECT value FROM json_each(?))"
	}
	if q.Kind == "users" && f.ExcludeAdmins {
		clauses = append(clauses, "role!='admin'")
	}
	if !f.Global && !f.All {
		switch q.Kind {
		case "services":
			clauses = append(clauses, "("+in("site_id", f.SiteIDs)+" OR "+in("id", f.ServiceIDs)+")")
		case "sites":
			sites := in("id", f.SiteIDs)
			services := "1=1"
			if !f.AllServices {
				services = in("svc.id", f.ServiceIDs)
			}
			clauses = append(clauses, "("+sites+" OR EXISTS (SELECT 1 FROM services svc WHERE svc.site_id=sites.id AND "+services+"))")
		case "groups":
			clauses = append(clauses, "id IN (SELECT group_id FROM group_members WHERE user_id=?)")
			args = append(args, f.MemberID)
		}
	}
	columns := "id,payload"
	switch q.Kind {
	case "users":
		columns = "id,username,password_hash,role,disabled,oidc_issuer,oidc_subject,organization_id,role_id"
	case "sites":
		columns = "id,name,token_hash,created_at,organization_id,legacy_disabled"
	case "services":
		columns = "id,name,site_id,url,organization_id,custom_domain,health_path,health_method,health_expected_status,connection_mode"
	}
	sql := "SELECT position," + columns + " FROM " + table + " WHERE " + strings.Join(clauses, " AND ") + " ORDER BY position"
	if q.Limit > 0 {
		sql += " LIMIT ?"
		args = append(args, q.Limit+1)
	}
	return sql, args
}
func (b *sqliteBackend) directory(q DirectoryQuery, after int) (State, int, bool, error) {
	sql, args := directorySQL(q, after)
	rows, err := b.db.Query(sql, args...)
	if err != nil {
		return State{}, 0, false, err
	}
	var out State
	last, count, more := -1, 0, false
	for rows.Next() {
		if q.Limit > 0 && count == q.Limit {
			more = true
			break
		}
		var pos int
		var value any
		switch q.Kind {
		case "users":
			var v User
			var issuer, subject string
			err = rows.Scan(&pos, &v.ID, &v.Username, &v.PasswordHash, &v.Role, &v.Disabled, &issuer, &subject, &v.OrganizationID, &v.RoleID)
			if issuer != "" || subject != "" {
				v.Identity = &Identity{issuer, subject}
			}
			v.SiteIDs = []string{}
			v.ServiceIDs = []string{}
			value = v
		case "sites":
			var v Site
			var created string
			err = rows.Scan(&pos, &v.ID, &v.Name, &v.TokenHash, &created, &v.OrganizationID, &v.LegacyDisabled)
			if err == nil {
				v.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
			}
			value = v
		case "services":
			var v Service
			err = rows.Scan(&pos, &v.ID, &v.Name, &v.SiteID, &v.URL, &v.OrganizationID, &v.CustomDomain, &v.HealthPath, &v.HealthMethod, &v.HealthExpectedStatus, &v.ConnectionMode)
			value = v
		default:
			var id, raw string
			err = rows.Scan(&pos, &id, &raw)
			if err == nil {
				value, err = directoryPayload(q.Kind, id, raw)
			}
			if g, ok := value.(Group); ok && !directoryMatch(State{}, g, q.Filter) {
				err = fmt.Errorf("invalid group directory projection")
			}
		}
		if err != nil {
			break
		}
		appendDirectory(&out, value)
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
		return State{}, 0, false, err
	}
	// Release the single connection before loading page-local grants and roles.
	if q.Kind == "users" {
		if err = b.directoryUserGrants(&out); err != nil {
			return State{}, 0, false, err
		}
	}
	return out, last, more, nil
}
func directoryPayload(kind, id, raw string) (any, error) {
	var value any
	var actualID string
	var err error
	switch kind {
	case "roles":
		var v Role
		err = decodeStrict([]byte(raw), &v)
		actualID = v.ID
		value = v
	case "groups":
		var v Group
		err = decodeStrict([]byte(raw), &v)
		actualID = v.ID
		value = v
	case "invitations":
		var v Invite
		err = decodeStrict([]byte(raw), &v)
		actualID = v.ID
		value = v
	case "organizations":
		var v Organization
		err = decodeStrict([]byte(raw), &v)
		actualID = v.ID
		value = v
	default:
		return nil, ErrDirectoryQuery
	}
	if err != nil || actualID != id {
		return nil, fmt.Errorf("invalid directory payload")
	}
	return value, nil
}
func (b *sqliteBackend) directoryUserGrants(out *State) error {
	if len(out.Users) == 0 {
		return nil
	}
	ids := []string{}
	roles := []string{}
	index := map[string]int{}
	for i, u := range out.Users {
		ids = append(ids, u.ID)
		index[u.ID] = i
		if u.RoleID != "" {
			roles = append(roles, u.RoleID)
		}
	}
	raw, _ := json.Marshal(ids)
	for _, grant := range []struct{ table, column string }{{"user_sites", "site_id"}, {"user_services", "service_id"}} {
		rows, err := b.db.Query("SELECT user_id,"+grant.column+" FROM "+grant.table+" WHERE user_id IN (SELECT value FROM json_each(?)) ORDER BY user_id,position", string(raw))
		if err != nil {
			return err
		}
		for rows.Next() {
			var userID, id string
			if err = rows.Scan(&userID, &id); err != nil {
				break
			}
			u := &out.Users[index[userID]]
			if grant.table == "user_sites" {
				u.SiteIDs = append(u.SiteIDs, id)
			} else {
				u.ServiceIDs = append(u.ServiceIDs, id)
			}
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
	}
	if len(roles) == 0 {
		return nil
	}
	raw, _ = json.Marshal(roles)
	rows, err := b.db.Query("SELECT id,payload FROM roles WHERE id IN (SELECT value FROM json_each(?)) ORDER BY position", string(raw))
	if err != nil {
		return err
	}
	owners := map[string]string{}
	for rows.Next() {
		var id, payload string
		if err = rows.Scan(&id, &payload); err != nil {
			break
		}
		var value any
		value, err = directoryPayload("roles", id, payload)
		if err != nil {
			break
		}
		role := value.(Role)
		out.Roles = append(out.Roles, role)
		owners[id] = role.OrganizationID
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
	for _, u := range out.Users {
		if u.RoleID != "" {
			org, ok := owners[u.RoleID]
			if !ok || org != u.OrganizationID {
				return fmt.Errorf("invalid user role projection")
			}
		}
	}
	return nil
}
