package control

import (
	"net/http"
	"strings"
	"sync"
	"testing"
)

func TestAdministratorLifecycle(t *testing.T) {
	f := setup(t)
	u, member := f.member(t, nil, nil)
	change := func(method, path string, body map[string]string, cookie *http.Cookie) (*http.Response, []byte) {
		t.Helper()
		return f.request(t, method, path, body, cookie, "")
	}
	res, _ := change("POST", "/api/administrators", map[string]string{"username": "another", "password": testPassword, "currentPassword": testPassword}, member)
	if res.StatusCode != 403 {
		t.Fatal("member created administrator")
	}
	res, _ = change("POST", "/api/administrators", map[string]string{"username": "another", "password": testPassword, "currentPassword": "wrong"}, f.cookie)
	if res.StatusCode != 403 {
		t.Fatal("stale proof accepted")
	}
	res, b := change("POST", "/api/administrators", map[string]string{"username": "another", "password": testPassword, "currentPassword": testPassword}, f.cookie)
	if res.StatusCode != 201 || strings.Contains(string(b), "passwordHash") {
		t.Fatal(string(b))
	}
	created := mustJSON[struct{ User User }](t, b).User
	res, _ = f.request(t, "POST", "/api/login", map[string]string{"username": "another", "password": testPassword}, nil, "")
	second := res.Cookies()[0]
	res, _ = change("PATCH", "/api/administrators/"+u.ID, map[string]string{"action": "promote", "currentPassword": testPassword}, f.cookie)
	if res.StatusCode != 200 {
		t.Fatal("promotion failed")
	}
	res, _ = f.request(t, "GET", "/api/me", nil, member, "")
	if res.StatusCode != 401 {
		t.Fatal("promotion retained old session")
	}
	res, b = change("PATCH", "/api/administrators/"+created.ID, map[string]string{"action": "disable", "currentPassword": testPassword}, f.cookie)
	if res.StatusCode != 200 {
		t.Fatal(string(b))
	}
	res, _ = f.request(t, "GET", "/api/me", nil, second, "")
	if res.StatusCode != 401 {
		t.Fatal("disabled admin session remained")
	}
	res, _ = change("PATCH", "/api/administrators/"+created.ID, map[string]string{"action": "enable", "currentPassword": testPassword}, f.cookie)
	if res.StatusCode != 200 {
		t.Fatal("enable failed")
	}
	res, _ = change("PATCH", "/api/administrators/"+u.ID, map[string]string{"action": "demote", "currentPassword": testPassword}, f.cookie)
	if res.StatusCode != 200 {
		t.Fatal("demotion failed")
	}
	res, _ = change("DELETE", "/api/administrators/"+created.ID, map[string]string{"currentPassword": testPassword}, f.cookie)
	if res.StatusCode != 200 {
		t.Fatal("delete failed")
	}
	root := f.s.store.snapshot().Users[0]
	for _, action := range []string{"disable", "demote", "delete"} {
		method := "PATCH"
		body := map[string]string{"action": action, "currentPassword": testPassword}
		if action == "delete" {
			method = "DELETE"
			delete(body, "action")
		}
		res, b = change(method, "/api/administrators/"+root.ID, body, f.cookie)
		if res.StatusCode != 400 || !strings.Contains(string(b), "at least one") {
			t.Fatal("last administrator protection failed", string(b))
		}
	}
}

func TestConcurrentAdministratorSelfDisable(t *testing.T) {
	f := setup(t)
	res, b := f.request(t, "POST", "/api/administrators", map[string]string{"username": "second", "password": testPassword, "currentPassword": testPassword}, f.cookie, "")
	if res.StatusCode != 201 {
		t.Fatal(string(b))
	}
	second := mustJSON[struct{ User User }](t, b).User
	res, _ = f.request(t, "POST", "/api/login", map[string]string{"username": "second", "password": testPassword}, nil, "")
	accounts := []struct {
		id     string
		cookie *http.Cookie
	}{{f.s.store.snapshot().Users[0].ID, f.cookie}, {second.ID, res.Cookies()[0]}}
	statuses := make(chan int, 2)
	var wg sync.WaitGroup
	for _, a := range accounts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, _ := f.request(t, "PATCH", "/api/administrators/"+a.id, map[string]string{"action": "disable", "currentPassword": testPassword}, a.cookie, "")
			statuses <- res.StatusCode
		}()
	}
	wg.Wait()
	close(statuses)
	success := 0
	for status := range statuses {
		if status == 200 {
			success++
		} else if status != 400 {
			t.Fatal(status)
		}
	}
	if success != 1 {
		t.Fatal("concurrent changes removed last administrator")
	}
}
