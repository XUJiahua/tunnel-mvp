package control

import (
	"github.com/XUJiahua/tunnel-mvp/internal/state"
	"net/http"
	"strings"
	"testing"
)

func TestDelegatedRoleAuthorityCeilingAndScope(t *testing.T) {
	f := setup(t)
	create := func(path string, body any) []byte {
		t.Helper()
		r, b := f.request(t, "POST", "/api"+path, body, f.cookie, "")
		if r.StatusCode != 201 {
			t.Fatalf("create: %d %s", r.StatusCode, b)
		}
		return b
	}
	a := mustJSON[state.Organization](t, create("/organizations", map[string]string{"name": "A"}))
	b := mustJSON[state.Organization](t, create("/organizations", map[string]string{"name": "B"}))
	role := func(org, name string, permissions []string) state.Role {
		return mustJSON[state.Role](t, create("/roles", map[string]any{"name": name, "organizationID": org, "permissions": permissions}))
	}
	managerRole := role(a.ID, "bounded delegator", []string{"roles:manage", "audit:read"})
	highRole := role(a.ID, "higher role", []string{"roles:manage", "sites:manage"})
	foreignRole := role(b.ID, "foreign", []string{"audit:read"})
	member := func(name, org string) User {
		return mustJSON[User](t, create("/users", map[string]string{"username": name, "organizationID": org, "password": testPassword}))
	}
	manager := member("delegator-a", a.ID)
	target := member("target-a", a.ID)
	higher := member("higher-a", a.ID)
	foreign := member("member-b", b.ID)
	assign := func(user User, role state.Role) {
		r, b := f.request(t, "PATCH", "/api/users/"+user.ID+"/role", map[string]string{"roleID": role.ID}, f.cookie, "")
		if r.StatusCode != 200 {
			t.Fatal(string(b))
		}
	}
	assign(manager, managerRole)
	assign(higher, highRole)
	login := func(u User) *http.Cookie {
		r, b := f.request(t, "POST", "/api/login", map[string]string{"username": u.Username, "password": testPassword}, nil, "")
		if r.StatusCode != 200 {
			t.Fatal(string(b))
		}
		return r.Cookies()[0]
	}
	cookie := login(manager)
	rootID := f.s.store.snapshot().Users[0].ID
	for _, tc := range []struct {
		method, path string
		body         any
	}{{"POST", "/api/roles", map[string]any{"name": "escape", "permissions": []string{"sites:manage"}}}, {"POST", "/api/roles", map[string]any{"name": "foreign", "organizationID": b.ID}}, {"PATCH", "/api/roles/" + highRole.ID, map[string]any{"name": "demoted", "permissions": []string{}}}, {"DELETE", "/api/roles/" + highRole.ID, nil}, {"DELETE", "/api/roles/" + foreignRole.ID, nil}, {"PATCH", "/api/users/" + higher.ID + "/role", map[string]string{"roleID": ""}}, {"PATCH", "/api/users/" + target.ID + "/role", map[string]string{"roleID": highRole.ID}}, {"PATCH", "/api/users/" + foreign.ID + "/role", map[string]string{"roleID": ""}}, {"PATCH", "/api/users/" + target.ID + "/role", map[string]string{"roleID": foreignRole.ID}}, {"PATCH", "/api/users/" + rootID + "/role", map[string]string{"roleID": managerRole.ID}}, {"POST", "/api/organizations", map[string]string{"name": "forbidden"}}} {
		r, b := f.request(t, tc.method, tc.path, tc.body, cookie, "")
		if r.StatusCode < 400 {
			t.Fatalf("delegation escape %s %s: %s", tc.method, tc.path, b)
		}
	}
	r, body := f.request(t, "POST", "/api/roles", map[string]any{"name": "auditor", "permissions": []string{"audit:read"}}, cookie, "")
	if r.StatusCode != 201 {
		t.Fatalf("bounded delegation failed: %s", body)
	}
	child := mustJSON[state.Role](t, body)
	if child.OrganizationID != a.ID {
		t.Fatal("omitted owner did not use actor organization")
	}
	r, _ = f.request(t, "PATCH", "/api/users/"+target.ID+"/role", map[string]string{"roleID": child.ID}, cookie, "")
	if r.StatusCode != 200 {
		t.Fatal("bounded assignment failed")
	}
	targetCookie := login(target)
	r, body = f.request(t, "GET", "/api/role-members", nil, cookie, "")
	if r.StatusCode != 200 || strings.Contains(string(body), foreign.ID) || strings.Contains(string(body), "passwordHash") || strings.Contains(string(body), "admin") {
		t.Fatal("role directory leaked data")
	}
	r, body = f.request(t, "GET", "/api/roles", nil, cookie, "")
	if r.StatusCode != 200 || strings.Contains(string(body), foreignRole.ID) {
		t.Fatal("role listing crossed organization")
	}
	r, _ = f.request(t, "PATCH", "/api/roles/"+child.ID, map[string]any{"name": "auditor changed", "permissions": []string{}}, cookie, "")
	if r.StatusCode != 200 {
		t.Fatal("bounded role update failed")
	}
	r, _ = f.request(t, "GET", "/api/me", nil, targetCookie, "")
	if r.StatusCode != 401 {
		t.Fatal("role edit retained old member session")
	}
	// Removing one's own delegation capability is allowed and immediately revokes the actor.
	r, _ = f.request(t, "PATCH", "/api/roles/"+managerRole.ID, map[string]any{"name": "no delegation", "permissions": []string{"audit:read"}}, cookie, "")
	if r.StatusCode != 200 {
		t.Fatal("self demotion failed")
	}
	r, _ = f.request(t, "POST", "/api/roles", map[string]string{"name": "stale authority"}, cookie, "")
	if r.StatusCode != 401 {
		t.Fatal("stale delegator session retained authority")
	}
}
