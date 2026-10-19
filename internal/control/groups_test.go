package control

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestGroupGrantsAndInvitationLifecycle(t *testing.T) {
	f := setup(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "group application") }))
	defer upstream.Close()
	site, token := f.site(t, "group site")
	f.agent(t, site, token, portOf(upstream.URL))
	app := f.service(t, site, "group app", upstream.URL)
	sibling := f.service(t, site, "private sibling", upstream.URL)
	u, login := f.member(t, nil, nil)
	res, b := f.request(t, "POST", "/api/groups", map[string]any{"name": "Operators", "memberIDs": []string{u.ID}, "serviceIDs": []string{app.ID}}, login, "")
	if res.StatusCode != 403 {
		t.Fatal("member created group")
	}
	res, b = f.request(t, "POST", "/api/groups", map[string]any{"name": "Operators", "memberIDs": []string{u.ID}, "serviceIDs": []string{app.ID}}, f.cookie, "")
	if res.StatusCode != 201 {
		t.Fatal(string(b))
	}
	group := mustJSON[Group](t, b)
	access, host, _ := f.grant(t, app, login)
	res, b = f.request(t, "GET", "/", nil, access, host)
	if res.StatusCode != 200 {
		t.Fatal("group grant rejected", string(b))
	}
	res, _ = f.request(t, "POST", "/api/services/"+sibling.ID+"/launch", nil, login, "")
	if res.StatusCode != 404 {
		t.Fatal("group grant leaked to sibling")
	}
	res, b = f.request(t, "GET", "/api/me/groups", nil, login, "")
	if res.StatusCode != 200 || !strings.Contains(string(b), "Operators") || strings.Contains(string(b), u.ID) {
		t.Fatal("member group view leaked membership", string(b))
	}
	// Removing direct grants must preserve access supplied by a group.
	res, _ = f.request(t, "PATCH", "/api/users/"+u.ID, map[string]any{"siteIDs": []string{}, "serviceIDs": []string{}}, f.cookie, "")
	if res.StatusCode != 200 {
		t.Fatal("direct edit failed")
	}
	res, _ = f.request(t, "GET", "/", nil, access, host)
	if res.StatusCode != 200 {
		t.Fatal("direct edit discarded group access")
	}
	// The link is shown once and never stored as plaintext or exposed in lists.
	res, b = f.request(t, "POST", "/api/invitations", map[string]any{"username": "invited", "hours": 24, "groupIDs": []string{group.ID}}, f.cookie, "")
	if res.StatusCode != 201 {
		t.Fatal(string(b))
	}
	created := mustJSON[struct {
		URL        string     `json:"url"`
		Invitation inviteView `json:"invitation"`
	}](t, b)
	raw := strings.Split(created.URL, "#invite=")[1]
	if strings.Contains(string(mustMarshal(f.s.store.snapshot())), raw) {
		t.Fatal("plaintext invitation persisted")
	}
	res, b = f.request(t, "GET", "/api/invitations", nil, f.cookie, "")
	if strings.Contains(string(b), raw) || strings.Contains(string(b), "tokenHash") {
		t.Fatal("list leaked invitation credential")
	}
	res, b = f.request(t, "POST", "/api/invitations/inspect", map[string]string{"token": raw}, nil, "")
	if res.StatusCode != 200 || !strings.Contains(string(b), "invited") {
		t.Fatal("inspection failed", string(b))
	}
	res, b = f.request(t, "POST", "/api/invitations/accept", map[string]string{"token": raw, "password": testPassword}, nil, "")
	if res.StatusCode != 201 {
		t.Fatal("accept failed", string(b))
	}
	invited := mustJSON[User](t, b)
	inviteLogin := res.Cookies()[0]
	if invited.Role != "viewer" {
		t.Fatal("invitation elevated role")
	}
	f.grant(t, app, inviteLogin)
	res, _ = f.request(t, "POST", "/api/invitations/accept", map[string]string{"token": raw, "password": testPassword}, nil, "")
	if res.StatusCode != 404 {
		t.Fatal("invitation replay accepted")
	}
	res, _ = f.request(t, "PATCH", "/api/groups/"+group.ID, map[string]any{"name": "Operators", "memberIDs": []string{u.ID, invited.ID}, "serviceIDs": []string{}}, f.cookie, "")
	if res.StatusCode != 200 {
		t.Fatal("group edit failed")
	}
	res, _ = f.request(t, "GET", "/", nil, access, host)
	if res.StatusCode != 401 {
		t.Fatal("group revocation left app accessible", res.StatusCode)
	}
	res, _ = f.request(t, "GET", "/api/me", nil, login, "")
	if res.StatusCode != 200 {
		t.Fatal("group edit revoked console")
	}
}
func mustMarshal(v any) []byte { b, _ := json.Marshal(v); return b }
func TestInvitationConcurrentUseAndRevocation(t *testing.T) {
	f := setup(t)
	create := func(name string) (string, string) {
		res, b := f.request(t, "POST", "/api/invitations", map[string]any{"username": name}, f.cookie, "")
		if res.StatusCode != 201 {
			t.Fatal(string(b))
		}
		v := mustJSON[struct {
			URL        string     `json:"url"`
			Invitation inviteView `json:"invitation"`
		}](t, b)
		return strings.Split(v.URL, "#invite=")[1], v.Invitation.ID
	}
	raw, _ := create("raced")
	var wg sync.WaitGroup
	statuses := make(chan int, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, _ := f.request(t, "POST", "/api/invitations/accept", map[string]string{"token": raw, "password": testPassword}, nil, "")
			statuses <- res.StatusCode
		}()
	}
	wg.Wait()
	close(statuses)
	successes := 0
	for code := range statuses {
		if code == 201 {
			successes++
		} else if code != 404 {
			t.Fatal("unexpected race status", code)
		}
	}
	if successes != 1 {
		t.Fatal("not single use", successes)
	}
	raw, id := create("revoked")
	res, _ := f.request(t, "DELETE", "/api/invitations/"+id, nil, f.cookie, "")
	if res.StatusCode != 200 {
		t.Fatal("revoke failed")
	}
	res, _ = f.request(t, "POST", "/api/invitations/accept", map[string]string{"token": raw, "password": testPassword}, nil, "")
	if res.StatusCode != 404 {
		t.Fatal("revoked invitation accepted")
	}
	res, _ = f.request(t, "POST", "/api/invitations", map[string]any{"username": "admin"}, f.cookie, "")
	if res.StatusCode != 400 {
		t.Fatal("existing admin name invited")
	}
}
