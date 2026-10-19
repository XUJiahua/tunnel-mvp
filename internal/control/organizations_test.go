package control

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/XUJiahua/tunnel-mvp/internal/state"
)

func TestOrganizationBoundariesAndRoleRevocation(t *testing.T) {
	f := setup(t)
	create := func(path string, body any, cookie *http.Cookie) []byte {
		t.Helper()
		res, b := f.request(t, "POST", "/api"+path, body, cookie, "")
		if res.StatusCode != 201 {
			t.Fatalf("create %s: %d %s", path, res.StatusCode, b)
		}
		return b
	}
	org := func(name string) string {
		return mustJSON[state.Organization](t, create("/organizations", map[string]string{"name": name}, f.cookie)).ID
	}
	a, b := org("Organization A"), org("Organization B")
	role := mustJSON[state.Role](t, create("/roles", map[string]any{"name": "Organization operator", "organizationID": a, "permissions": state.TenantPermissions}, f.cookie))
	roleB := mustJSON[state.Role](t, create("/roles", map[string]any{"name": "B operator", "organizationID": b, "permissions": state.TenantPermissions}, f.cookie))
	member := func(name, owner string) User {
		return mustJSON[User](t, create("/users", map[string]string{"username": name, "password": testPassword, "organizationID": owner}, f.cookie))
	}
	operator := member("operator-a", a)
	peer := member("peer-a", a)
	other := member("member-b", b)
	res, body := f.request(t, "PATCH", "/api/users/"+operator.ID+"/role", map[string]string{"roleID": role.ID}, f.cookie, "")
	if res.StatusCode != 200 {
		t.Fatal(string(body))
	}
	login := func(username string) *http.Cookie {
		t.Helper()
		res, body := f.request(t, "POST", "/api/login", map[string]string{"username": username, "password": testPassword}, nil, "")
		if res.StatusCode != 200 {
			t.Fatal(string(body))
		}
		return res.Cookies()[0]
	}
	ownerCookie := login(operator.Username)
	site := func(owner string) (string, string) {
		v := mustJSON[struct {
			Site  Site
			Token string
		}](t, create("/sites", map[string]string{"name": "Owned site", "organizationID": owner}, f.cookie))
		return v.Site.ID, v.Token
	}
	sa, token := site(a)
	sb, _ := site(b)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "organization A application") }))
	defer upstream.Close()
	siteA, _ := f.s.store.site(sa)
	siteB, _ := f.s.store.site(sb)
	app := f.service(t, siteA, "A application", upstream.URL)
	foreign := f.service(t, siteB, "B application", upstream.URL)
	f.agent(t, siteA, token, portOf(upstream.URL))
	access, host, _ := f.grant(t, app, ownerCookie)
	res, _ = f.request(t, "GET", "/", nil, access, host)
	if res.StatusCode != 200 {
		t.Fatal("organization application inaccessible")
	}
	group := func(owner string, members []string) Group {
		return mustJSON[Group](t, create("/groups", map[string]any{"name": "Owned group", "organizationID": owner, "memberIDs": members}, f.cookie))
	}
	ga, gb := group(a, []string{peer.ID}), group(b, []string{other.ID})
	inv := mustJSON[struct{ Invitation inviteView }](t, create("/invitations", map[string]string{"username": "invited-b", "organizationID": b}, f.cookie)).Invitation
	for _, path := range []string{"/api/sites", "/api/services", "/api/users", "/api/groups", "/api/invitations", "/api/roles", "/api/organizations", "/api/group-members", "/api/agents"} {
		res, body = f.request(t, "GET", path, nil, ownerCookie, "")
		if res.StatusCode != 200 || strings.Contains(string(body), b) || strings.Contains(string(body), other.ID) || strings.Contains(string(body), foreign.ID) {
			t.Fatalf("cross-org listing %s: %d %s", path, res.StatusCode, body)
		}
	}
	for _, c := range []struct {
		method, path string
		body         any
	}{

		{"POST", "/api/services/bulk-delete", map[string]any{"ids": []string{app.ID, foreign.ID}}},
		{"POST", "/api/services/bulk-move", map[string]any{"ids": []string{app.ID, foreign.ID}, "siteID": sa}},
		{"POST", "/api/services/bulk", map[string]any{"services": []map[string]string{{"name": "bad bulk", "siteID": sb, "url": upstream.URL}}}},
		{"PATCH", "/api/services/" + app.ID, map[string]string{"name": app.Name, "url": app.URL, "customDomain": "steal.customer.test"}},
		{"POST", "/api/sites/" + sb + "/agents", map[string]string{"name": "foreign"}},
		{"PATCH", "/api/sites/" + sb, map[string]string{"name": "stolen"}},
		{"DELETE", "/api/sites/" + sb, nil}, {"POST", "/api/sites/" + sb + "/rotate", nil},
		{"PATCH", "/api/services/" + foreign.ID, map[string]string{"name": "stolen", "url": upstream.URL}},
		{"DELETE", "/api/services/" + foreign.ID, nil}, {"POST", "/api/services/" + foreign.ID + "/probe", nil}, {"POST", "/api/services/" + foreign.ID + "/launch", nil},
		{"POST", "/api/services", map[string]string{"name": "stolen", "siteID": sb, "url": upstream.URL}},
		{"POST", "/api/sites", map[string]string{"name": "stolen", "organizationID": b}},
		{"POST", "/api/users", map[string]string{"username": "stolen", "password": testPassword, "organizationID": b}},
		{"PATCH", "/api/users/" + other.ID, map[string]any{"siteIDs": []string{sb}}},
		{"DELETE", "/api/users/" + other.ID, nil}, {"POST", "/api/users/" + other.ID + "/password", map[string]string{"password": testPassword}},
		{"PATCH", "/api/users/" + other.ID + "/disabled", map[string]bool{"disabled": true}},
		{"PATCH", "/api/users/" + other.ID + "/identity", map[string]string{"subject": ""}},
		{"PATCH", "/api/users/" + peer.ID, map[string]any{"serviceIDs": []string{foreign.ID}}},
		{"POST", "/api/groups", map[string]any{"name": "bad", "memberIDs": []string{other.ID}}},
		{"PATCH", "/api/groups/" + gb.ID, map[string]string{"name": "stolen"}}, {"DELETE", "/api/groups/" + gb.ID, nil},
		{"POST", "/api/invitations", map[string]any{"username": "bad-invite", "groupIDs": []string{gb.ID}}}, {"DELETE", "/api/invitations/" + inv.ID, nil},
		{"PATCH", "/api/users/" + peer.ID + "/role", map[string]string{"roleID": roleB.ID}},
		{"PATCH", "/api/users/" + operator.ID + "/disabled", map[string]bool{"disabled": true}},
		{"POST", "/api/roles", map[string]string{"name": "escape"}}, {"POST", "/api/organizations", map[string]string{"name": "escape"}},
		{"POST", "/api/administrators", map[string]string{"username": "escape", "password": testPassword, "currentPassword": testPassword}},
	} {
		res, body = f.request(t, c.method, c.path, c.body, ownerCookie, "")
		if res.StatusCode < 400 {
			t.Fatalf("cross-org mutation accepted %s %s: %s", c.method, c.path, body)
		}
	}
	res, _ = f.request(t, "GET", "/api/services/"+foreign.ID+"/history", nil, ownerCookie, "")
	if res.StatusCode != 404 {
		t.Fatal("monitoring history crossed organization boundary")
	}
	for _, path := range []string{"/api/metrics", "/api/sessions"} {
		res, _ = f.request(t, "GET", path, nil, ownerCookie, "")
		if res.StatusCode != 403 {
			t.Fatal("global data leaked", path)
		}
	}
	res, _ = f.request(t, "PATCH", "/api/users/"+peer.ID+"/role", map[string]string{"roleID": roleB.ID}, f.cookie, "")
	if res.StatusCode != 400 {
		t.Fatal("cross-org role binding accepted")
	}
	// Scope is attached to deletions even when a global administrator performs them.
	res, _ = f.request(t, "DELETE", "/api/groups/"+ga.ID, nil, f.cookie, "")
	if res.StatusCode != 200 {
		t.Fatal("root delete failed")
	}
	res, body = f.request(t, "GET", "/api/audit", nil, ownerCookie, "")
	if res.StatusCode != 200 || !strings.Contains(string(body), "group_deleted") || strings.Contains(string(body), b) || strings.Contains(string(body), "administrator_") {
		t.Fatal("audit ownership boundary", string(body))
	}
	// Management and application access are distinct. Editing a role revokes all
	// old devices and their access; a new session sees only the reduced capability.
	res, body = f.request(t, "PATCH", "/api/roles/"+role.ID, map[string]any{"name": "Application editor", "permissions": []string{"applications:manage"}}, f.cookie, "")
	if res.StatusCode != 200 {
		t.Fatal(string(body))
	}
	res, _ = f.request(t, "GET", "/api/me", nil, ownerCookie, "")
	if res.StatusCode != 401 {
		t.Fatal("role edit retained session")
	}
	res, _ = f.request(t, "GET", "/", nil, access, host)
	if res.StatusCode != 401 {
		t.Fatal("role edit retained application cookie")
	}
	ownerCookie = login(operator.Username)
	res, _ = f.request(t, "POST", "/api/services/"+app.ID+"/launch", nil, ownerCookie, "")
	if res.StatusCode != 404 {
		t.Fatal("management permission granted application access")
	}
	res, body = f.request(t, "POST", "/api/services/"+app.ID+"/probe", nil, ownerCookie, "")
	if res.StatusCode != 200 {
		t.Fatal("manager cannot probe own application", string(body))
	}
	res, _ = f.request(t, "DELETE", "/api/roles/"+role.ID, nil, f.cookie, "")
	if res.StatusCode != 200 {
		t.Fatal("role deletion failed")
	}
	ownerCookie = login(operator.Username)
	res, _ = f.request(t, "PATCH", "/api/services/"+app.ID, map[string]string{"name": "bad", "url": upstream.URL}, ownerCookie, "")
	if res.StatusCode != 403 {
		t.Fatal("deleted role left management capability")
	}
}
