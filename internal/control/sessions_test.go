package control

import (
	"net/http"
	"strings"
	"testing"
)

func TestSessionInventoryAndScopedRevocation(t *testing.T) {
	f := setup(t)
	u, first := f.member(t, nil, nil)
	res, b := f.request(t, "POST", "/api/login", map[string]string{"username": u.Username, "password": testPassword}, nil, "")
	if res.StatusCode != 200 {
		t.Fatal(string(b))
	}
	second := res.Cookies()[0]
	res, b = f.request(t, "GET", "/api/me/sessions", nil, second, "")
	sessions := mustJSON[[]sessionInfo](t, b)
	if res.StatusCode != 200 || len(sessions) != 2 {
		t.Fatal("missing sessions", string(b))
	}
	var old, current sessionInfo
	for _, v := range sessions {
		if v.Current {
			current = v
		} else {
			old = v
		}
	}
	if current.ID == "" || old.ID == "" || current.Method != "password" || current.RemoteIP == "" || current.CreatedAt.IsZero() || current.LastSeen.IsZero() || strings.Contains(string(b), first.Value) || strings.Contains(string(b), digest(first.Value)) {
		t.Fatal("bad session inventory", string(b))
	}
	res, _ = f.request(t, "GET", "/api/sessions", nil, second, "")
	if res.StatusCode != 403 {
		t.Fatal("member listed other sessions")
	}
	res, _ = f.request(t, "DELETE", "/api/sessions/"+old.ID, nil, second, "")
	if res.StatusCode != 403 {
		t.Fatal("member used admin route")
	}
	res, _ = f.request(t, "DELETE", "/api/me/sessions/"+old.ID, nil, f.cookie, "")
	if res.StatusCode != 404 {
		t.Fatal("self route crossed ownership")
	}
	res, _ = f.request(t, "DELETE", "/api/sessions/"+old.ID, nil, f.cookie, "")
	if res.StatusCode != 200 {
		t.Fatal("admin revoke failed")
	}
	res, _ = f.request(t, "GET", "/api/me", nil, first, "")
	if res.StatusCode != 401 {
		t.Fatal("revoked device survived")
	}
	res, _ = f.request(t, "GET", "/api/me", nil, second, "")
	if res.StatusCode != 200 {
		t.Fatal("other device was revoked")
	}
	res, _ = f.request(t, "DELETE", "/api/me/sessions/"+current.ID, nil, second, "")
	if res.StatusCode != 200 {
		t.Fatal("self revoke failed")
	}
	cleared := false
	for _, c := range res.Cookies() {
		if c.Name == consoleCookie && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Fatal("self revocation did not clear cookie")
	}
	res, _ = f.request(t, "GET", "/api/me", nil, second, "")
	if res.StatusCode != 401 {
		t.Fatal("self revoked session survived")
	}
}
func TestSessionRoutesRequireExactBrowserOrigin(t *testing.T) {
	f := setup(t)
	req, _ := http.NewRequest("DELETE", f.h.URL+"/api/me/sessions/unknown", nil)
	req.AddCookie(f.cookie)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 403 {
		t.Fatal("missing Origin accepted")
	}
}
