package state

import "fmt"

var TenantPermissions = []string{"roles:manage", "sites:manage", "applications:manage", "members:manage", "groups:manage", "audit:read", "applications:access"}

// The legacy/default organization is represented by an empty owner ID. Keeping
// that representation preserves canonical old backup checksums and grants.
func validateOrganizations(s *State) error {
	orgs := map[string]bool{"": true}
	for _, o := range s.Organizations {
		if !resourceID.MatchString(o.ID) || o.ID == "default" || orgs[o.ID] || !validName(o.Name) {
			return fmt.Errorf("invalid or duplicate organization")
		}
		orgs[o.ID] = true
	}
	roles := map[string]bool{}
	for i := range s.Roles {
		r := &s.Roles[i]
		if !resourceID.MatchString(r.ID) || roles[r.ID] || !orgs[r.OrganizationID] || !validName(r.Name) {
			return fmt.Errorf("invalid or duplicate organization role")
		}
		roles[r.ID] = true
		r.Permissions = uniqueGrants(r.Permissions)
		for _, p := range r.Permissions {
			known := false
			for _, allowed := range TenantPermissions {
				if p == allowed {
					known = true
				}
			}
			if !known {
				return fmt.Errorf("unknown tenant permission %q", p)
			}
		}
	}
	for _, u := range s.Users {
		if !orgs[u.OrganizationID] || (u.Role == "admin" && (u.OrganizationID != "" || u.RoleID != "")) {
			return fmt.Errorf("invalid user organization or global administrator role")
		}
		if u.RoleID != "" {
			found := false
			for _, r := range s.Roles {
				if r.ID == u.RoleID && r.OrganizationID == u.OrganizationID {
					found = true
				}
			}
			if !found {
				return fmt.Errorf("invalid user role reference")
			}
		}
	}
	for _, v := range s.Sites {
		if !orgs[v.OrganizationID] {
			return fmt.Errorf("unknown site organization")
		}
	}
	for _, v := range s.Groups {
		if !orgs[v.OrganizationID] {
			return fmt.Errorf("unknown group organization")
		}
	}
	for _, v := range s.Invites {
		if !orgs[v.OrganizationID] {
			return fmt.Errorf("unknown invitation organization")
		}
	}
	return nil
}
func validateOrganizationReferences(s *State) error {
	sites, apps, users, groups := map[string]string{}, map[string]string{}, map[string]string{}, map[string]string{}
	for _, v := range s.Sites {
		sites[v.ID] = v.OrganizationID
	}
	for _, v := range s.Users {
		users[v.ID] = v.OrganizationID
	}
	for _, v := range s.Groups {
		groups[v.ID] = v.OrganizationID
	}
	for _, v := range s.Services {
		if v.OrganizationID != sites[v.SiteID] {
			return fmt.Errorf("cross-organization application site")
		}
		apps[v.ID] = v.OrganizationID
	}
	grants := func(org string, ss, aa []string) error {
		for _, id := range ss {
			if sites[id] != org {
				return fmt.Errorf("cross-organization site grant")
			}
		}
		for _, id := range aa {
			if apps[id] != org {
				return fmt.Errorf("cross-organization application grant")
			}
		}
		return nil
	}
	for _, u := range s.Users {
		if u.Role != "admin" {
			if err := grants(u.OrganizationID, u.SiteIDs, u.ServiceIDs); err != nil {
				return err
			}
		}
	}
	for _, v := range s.Groups {
		if err := grants(v.OrganizationID, v.SiteIDs, v.ServiceIDs); err != nil {
			return err
		}
		for _, id := range v.MemberIDs {
			if users[id] != v.OrganizationID {
				return fmt.Errorf("cross-organization group member")
			}
		}
	}
	for _, v := range s.Invites {
		if err := grants(v.OrganizationID, v.SiteIDs, v.ServiceIDs); err != nil {
			return err
		}
		for _, id := range v.GroupIDs {
			if groups[id] != v.OrganizationID {
				return fmt.Errorf("cross-organization invitation group")
			}
		}
	}
	return nil
}
