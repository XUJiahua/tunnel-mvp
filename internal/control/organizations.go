package control

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/XUJiahua/tunnel-mvp/internal/state"
)

func permissionForRequest(r *http.Request) string {
	p := r.URL.Path
	switch {
	case strings.HasPrefix(p, "/api/users"):
		if strings.HasSuffix(p, "/role") {
			return "roles:manage"
		}
		return "members:manage"
	case strings.HasPrefix(p, "/api/roles"):
		return "roles:manage"
	case strings.HasPrefix(p, "/api/groups"), strings.HasPrefix(p, "/api/invitations"):
		return "groups:manage"
	case strings.HasPrefix(p, "/api/sites"):
		return "sites:manage"
	case strings.HasPrefix(p, "/api/services"):
		return "applications:manage"
	case p == "/api/audit":
		return "audit:read"
	default:
		return ""
	}
}
func hasPermission(u User, permission string) bool {
	return !u.Disabled && (u.Role == "admin" || (permission != "" && containsID(u.Permissions, permission)))
}
func canManage(u User, org, permission string) bool {
	return hasPermission(u, permission) && (u.Role == "admin" || u.OrganizationID == org)
}

func canManageMember(actor, target User) bool {
	return actor.Role == "admin" || (target.Role != "admin" && target.RoleID == "" && canManage(actor, target.OrganizationID, "members:manage"))
}
func (s *Server) groupMembers(w http.ResponseWriter, r *http.Request, u User) {
	if !hasPermission(u, "groups:manage") {
		fail(w, 403, "group management permission required")
		return
	}
	out := []map[string]string{}
	filter := directoryFilter(u)
	filter.ExcludeAdmins = true
	st, ok := s.directory(w, r, "users", filter)
	if !ok {
		return
	}
	for _, m := range st.Users {
		if m.Role != "admin" && (u.Role == "admin" || m.OrganizationID == u.OrganizationID) {
			out = append(out, map[string]string{"id": m.ID, "username": m.Username, "organizationID": m.OrganizationID})
		}
	}
	jsonOut(w, 200, out)
}
func rolePermissions(st State, u User) []string {
	for _, r := range st.Roles {
		if r.ID == u.RoleID && r.OrganizationID == u.OrganizationID {
			return append([]string{}, r.Permissions...)
		}
	}
	return []string{}
}

// Missing ownership is intentionally not equivalent to the default organization:
// old/global audit events remain visible only to global administrators.
func auditOrganization(st State, username, site, resource string) (string, bool) {
	org, found, conflict := "", false, false
	add := func(owner string) {
		if found && owner != org {
			conflict = true
		}
		org, found = owner, true
	}
	if site != "" {
		for _, v := range st.Sites {
			if v.ID == site {
				add(v.OrganizationID)
			}
		}
	}
	if resource != "" {
		for _, v := range st.Services {
			if v.ID == resource {
				add(v.OrganizationID)
			}
		}
		for _, v := range st.Users {
			if v.ID == resource && v.Role != "admin" {
				add(v.OrganizationID)
			}
		}
		for _, v := range st.Groups {
			if v.ID == resource {
				add(v.OrganizationID)
			}
		}
		for _, v := range st.Invites {
			if v.ID == resource {
				add(v.OrganizationID)
			}
		}
	}
	if !found {
		for _, v := range st.Users {
			if v.Username == username && v.Role != "admin" {
				add(v.OrganizationID)
			}
		}
	}
	return org, found && !conflict
}
func organizationExists(st State, id string) bool {
	if id == "" {
		return true
	}
	for _, o := range st.Organizations {
		if o.ID == id {
			return true
		}
	}
	return false
}
func selectedOrganization(st State, u User, requested string) (string, error) {
	if requested == "default" {
		requested = ""
	} else if requested == "" && u.Role != "admin" {
		requested = u.OrganizationID
	}
	if !organizationExists(st, requested) || (u.Role != "admin" && requested != u.OrganizationID) {
		return "", fmt.Errorf("organization not found")
	}
	return requested, nil
}
func (s *Server) listOrganizations(w http.ResponseWriter, r *http.Request, u User) {
	st, ok := s.directory(w, r, "organizations", directoryFilter(u))
	if !ok {
		return
	}
	out := []state.Organization{}
	if r.URL.Query().Get("cursor") == "" && (u.Role == "admin" || u.OrganizationID == "") {
		out = append(out, state.Organization{ID: "default", Name: "默认组织"})
	}
	for _, o := range st.Organizations {
		if u.Role == "admin" || o.ID == u.OrganizationID {
			out = append(out, o)
		}
	}
	jsonOut(w, 200, out)
}
func (s *Server) manageOrganization(w http.ResponseWriter, r *http.Request, u User) {
	var body struct {
		Name string `json:"name"`
	}
	if r.Method != "DELETE" && !decode(w, r, &body) {
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	id := r.PathValue("id")
	create := r.Method == "POST"
	if create {
		id = randomID()[:16]
	}
	out := state.Organization{ID: id, Name: body.Name}
	err := s.updateAuthorized(r, u, func(st *State) error {
		if create {
			st.Organizations = append(st.Organizations, out)
			return nil
		}
		for i, o := range st.Organizations {
			if o.ID != id {
				continue
			}
			if r.Method == "DELETE" {
				for _, v := range st.Users {
					if v.OrganizationID == id {
						return fmt.Errorf("organization is not empty")
					}
				}
				for _, v := range st.Sites {
					if v.OrganizationID == id {
						return fmt.Errorf("organization is not empty")
					}
				}
				for _, v := range st.Groups {
					if v.OrganizationID == id {
						return fmt.Errorf("organization is not empty")
					}
				}
				for _, v := range st.Invites {
					if v.OrganizationID == id {
						return fmt.Errorf("organization is not empty")
					}
				}
				for _, v := range st.Roles {
					if v.OrganizationID == id {
						return fmt.Errorf("organization is not empty")
					}
				}
				st.Organizations = append(st.Organizations[:i], st.Organizations[i+1:]...)
			} else {
				st.Organizations[i].Name = body.Name
				out = st.Organizations[i]
			}
			return nil
		}
		return fmt.Errorf("organization not found; default organization is permanent")
	})
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	s.audit("organization_"+strings.ToLower(r.Method), u.Username, "", id, map[string]any{"organizationID": id})
	code := 200
	if create {
		code = 201
	}
	jsonOut(w, code, out)
}

// Delegation is bounded by the actor's current authority, not only organization.
func delegatedPermissions(actor User, permissions []string) bool {
	if actor.Role == "admin" {
		return true
	}
	for _, p := range permissions {
		if !containsID(actor.Permissions, p) {
			return false
		}
	}
	return true
}
func manageableRole(actor User, role state.Role) bool {
	return canManage(actor, role.OrganizationID, "roles:manage") && delegatedPermissions(actor, role.Permissions)
}
func manageableRoleMember(st State, actor, target User) bool {
	if target.Role == "admin" {
		return false
	}
	if !canManage(actor, target.OrganizationID, "roles:manage") {
		return false
	}
	return delegatedPermissions(actor, rolePermissions(st, target))
}
func (s *Server) roleMembers(w http.ResponseWriter, r *http.Request, u User) {
	if !hasPermission(u, "roles:manage") {
		fail(w, 403, "role management permission required")
		return
	}
	filter := directoryFilter(u)
	filter.ExcludeAdmins = true
	st, ok := s.directory(w, r, "users", filter)
	if !ok {
		return
	}
	out := []map[string]any{}
	for _, m := range st.Users {
		if m.Role != "admin" && (u.Role == "admin" || m.OrganizationID == u.OrganizationID) {
			out = append(out, map[string]any{"id": m.ID, "username": m.Username, "organizationID": m.OrganizationID, "role": "viewer", "roleID": m.RoleID, "manageable": manageableRoleMember(st, u, m)})
		}
	}
	jsonOut(w, 200, out)
}
func (s *Server) listRoles(w http.ResponseWriter, r *http.Request, u User) {
	out := []map[string]any{}
	st, ok := s.directory(w, r, "roles", directoryFilter(u))
	if !ok {
		return
	}
	for _, role := range st.Roles {
		if u.Role == "admin" || role.OrganizationID == u.OrganizationID {
			out = append(out, map[string]any{"id": role.ID, "name": role.Name, "organizationID": role.OrganizationID, "permissions": role.Permissions, "manageable": manageableRole(u, role)})
		}
	}
	permissions := []string{}
	for _, p := range state.TenantPermissions {
		if hasPermission(u, p) {
			permissions = append(permissions, p)
		}
	}
	jsonOut(w, 200, map[string]any{"roles": out, "permissions": permissions})
}
func (s *Server) manageRole(w http.ResponseWriter, r *http.Request, u User) {
	var body struct {
		Name           string   `json:"name"`
		OrganizationID string   `json:"organizationID"`
		Permissions    []string `json:"permissions"`
	}
	if r.Method != "DELETE" && !decode(w, r, &body) {
		return
	}
	if !delegatedPermissions(u, body.Permissions) {
		fail(w, 403, "cannot delegate permissions outside your current authority")
		return
	}
	id := r.PathValue("id")
	create := r.Method == "POST"
	if create {
		id = randomID()[:16]
	}
	org, err := selectedOrganization(s.store.snapshot(), u, body.OrganizationID)
	if err != nil {
		fail(w, 404, "organization not found")
		return
	}
	out := state.Role{ID: id, Name: strings.TrimSpace(body.Name), OrganizationID: org, Permissions: body.Permissions}
	affected := []string{}
	err = s.updateAuthorized(r, u, func(st *State) error {
		if create {
			if !canManage(u, org, "roles:manage") {
				return fmt.Errorf("role management permission required")
			}
			st.Roles = append(st.Roles, out)
			return nil
		}
		for i, role := range st.Roles {
			if role.ID != id {
				continue
			}
			if !manageableRole(u, role) {
				return fmt.Errorf("role not manageable within current authority")
			}
			if body.OrganizationID != "" && org != role.OrganizationID {
				return fmt.Errorf("role organization cannot change")
			}
			out.OrganizationID = role.OrganizationID
			for j := range st.Users {
				if st.Users[j].RoleID == id {
					affected = append(affected, st.Users[j].ID)
					if r.Method == "DELETE" {
						st.Users[j].RoleID = ""
					}
				}
			}
			if r.Method == "DELETE" {
				out = role
				st.Roles = append(st.Roles[:i], st.Roles[i+1:]...)
			} else {
				st.Roles[i] = out
			}
			return nil
		}
		return fmt.Errorf("role not found")
	})
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	for _, id := range affected {
		s.revokeUser(id)
	}
	s.audit("role_"+strings.ToLower(r.Method), u.Username, "", id, map[string]any{"organizationID": out.OrganizationID})
	code := 200
	if create {
		code = 201
	}
	jsonOut(w, code, out)
}
func (s *Server) assignRole(w http.ResponseWriter, r *http.Request, u User) {
	var body struct {
		RoleID string `json:"roleID"`
	}
	if !decode(w, r, &body) {
		return
	}
	var out User
	err := s.updateAuthorized(r, u, func(st *State) error {
		for i := range st.Users {
			m := &st.Users[i]
			if m.ID != r.PathValue("id") {
				continue
			}
			if !manageableRoleMember(*st, u, *m) {
				return fmt.Errorf("member role exceeds your organization or current authority")
			}
			if body.RoleID != "" {
				found := false
				for _, role := range st.Roles {
					if role.ID == body.RoleID && role.OrganizationID == m.OrganizationID && manageableRole(u, role) {
						found = true
						break
					}
				}
				if !found {
					return fmt.Errorf("role not assignable within current authority")
				}
			}
			m.RoleID = body.RoleID
			out = *m
			return nil
		}
		return fmt.Errorf("member not found")
	})
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	s.revokeUser(out.ID)
	out.Permissions = rolePermissions(s.store.snapshot(), out)
	s.audit("role_assigned", u.Username, "", out.ID, map[string]any{"organizationID": out.OrganizationID})
	jsonOut(w, 200, publicUser(out))
}
