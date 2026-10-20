package control

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestMountedApplicationAndCustomDomain(t *testing.T) {
	f := setup(t)
	var origin string
	var private atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/grafana/":
			fmt.Fprint(w, "mounted application")
		case "/grafana/redirect":
			http.Redirect(w, r, origin+"/grafana/login?next=%2F", 302)
		case "/grafana/cookies":
			w.Header().Set("Set-Cookie", "app=secret; Domain=internal.example; Path=/grafana/; HttpOnly")
			fmt.Fprint(w, "cookie")
		case "/grafana/echo":
			w.Header().Set("Access-Control-Allow-Origin", origin)
			fmt.Fprintf(w, "%s|%s|%s|%s|%s", r.URL.RequestURI(), r.Header.Get("Origin"), r.Header.Get("Referer"), r.Header.Get("X-Forwarded-Host"), r.Header.Get("Cookie"))
		default:
			private.Add(1)
			fmt.Fprint(w, "private origin application")
		}
	}))
	defer upstream.Close()
	origin = upstream.URL
	site, token := f.site(t, "mount site")
	f.agent(t, site, token, portOf(origin))
	svc := f.service(t, site, "mounted", origin+"/grafana")
	if !strings.HasSuffix(svc.URL, "/grafana/") {
		t.Fatal("mount not canonical")
	}
	res, b := f.request(t, "PATCH", "/api/services/"+svc.ID, map[string]string{"name": "mounted", "url": svc.URL, "customDomain": "reports.customer.test"}, f.cookie, "")
	if res.StatusCode != 200 {
		t.Fatal(string(b))
	}
	svc = mustJSON[Service](t, b)
	access, host, _ := f.grant(t, svc, f.cookie)
	if !strings.HasPrefix(host, "reports.customer.test") {
		t.Fatal("custom host not used", host)
	}
	res, _ = f.request(t, "GET", "/", nil, access, host)
	if res.StatusCode != 307 || res.Header.Get("Location") != "/grafana/" {
		t.Fatal("root did not redirect to mount")
	}
	res, b = f.request(t, "GET", "/grafana/", nil, access, host)
	if res.StatusCode != 200 || string(b) != "mounted application" {
		t.Fatal("mount forwarded incorrectly", res.StatusCode, string(b))
	}
	for _, path := range []string{"/private", "/grafana/../../private", "/grafana/%2e%2e/private", "/grafana/%252e%252e/private", "/grafana/..%5cprivate", "//private"} {
		res, b = f.request(t, "GET", path, nil, access, host)
		if res.StatusCode != 404 {
			t.Fatalf("mount escaped: %s %d %s", path, res.StatusCode, b)
		}
	}
	if private.Load() != 0 {
		t.Fatal("private origin path was dialed")
	}
	res, _ = f.request(t, "GET", "/grafana/redirect", nil, access, host)
	if res.Header.Get("Location") != "http://"+host+"/grafana/login?next=%2F" {
		t.Fatal("redirect not rewritten", res.Header.Get("Location"))
	}
	res, _ = f.request(t, "GET", "/grafana/cookies", nil, access, host)
	if c := res.Cookies()[0]; c.Domain != "" || c.Path != "/grafana/" {
		t.Fatal("cookie mount/domain changed", c)
	}
	req, _ := http.NewRequest("POST", f.h.URL+"/grafana/echo?q=a%2Fb", strings.NewReader("payload"))
	req.Host = host
	req.AddCookie(access)
	req.AddCookie(&http.Cookie{Name: consoleCookie, Value: "never-forward"})
	req.AddCookie(&http.Cookie{Name: "app", Value: "yes"})
	req.Header.Set("Origin", "http://"+host)
	req.Header.Set("Referer", "http://"+host+"/grafana/form")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	data, readErr := io.ReadAll(res.Body)
	expected := "/grafana/echo?q=a%2Fb|" + origin + "|" + origin + "/grafana/form|" + host + "|app=yes"
	if readErr != nil || string(data) != expected || res.StatusCode != 200 || res.Header.Get("Access-Control-Allow-Origin") != "http://"+host {
		t.Fatal("mounted headers/path/cookie isolation failed")
	}

	for _, domain := range []string{f.s.public.Hostname(), f.s.access.Hostname(), "child." + f.s.access.Hostname(), "bad/host"} {
		res, _ = f.request(t, "PATCH", "/api/services/"+svc.ID, map[string]string{"name": svc.Name, "url": svc.URL, "customDomain": domain}, f.cookie, "")
		if res.StatusCode != 400 {
			t.Fatal("reserved domain accepted", domain)
		}
	}
	other := f.service(t, site, "other", origin)
	res, _ = f.request(t, "PATCH", "/api/services/"+other.ID, map[string]string{"name": other.Name, "url": other.URL, "customDomain": svc.CustomDomain}, f.cookie, "")
	if res.StatusCode != 400 {
		t.Fatal("duplicate custom domain accepted")
	}
}

func TestApplicationMoveAndAtomicBulk(t *testing.T) {
	f := setup(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "moved application") }))
	defer upstream.Close()
	old, token := f.site(t, "old")
	next, token2 := f.site(t, "new")
	f.agent(t, old, token, portOf(upstream.URL))
	f.agent(t, next, token2, portOf(upstream.URL))
	svc := f.service(t, old, "move", upstream.URL)
	u, login := f.member(t, []string{old.ID}, nil)
	access, host, _ := f.grant(t, svc, login)
	res, b := f.request(t, "PATCH", "/api/services/"+svc.ID, map[string]string{"name": svc.Name, "url": svc.URL, "siteID": next.ID}, f.cookie, "")
	if res.StatusCode != 200 {
		t.Fatal(string(b))
	}
	res, _ = f.request(t, "GET", "/", nil, access, host)
	if res.StatusCode != 401 {
		t.Fatal("move retained old application grant")
	}
	res, _ = f.request(t, "POST", "/api/services/"+svc.ID+"/launch", nil, login, "")
	if res.StatusCode != 404 {
		t.Fatal("old site grant followed moved application")
	}
	res, _ = f.request(t, "PATCH", "/api/users/"+u.ID, map[string]any{"serviceIDs": []string{svc.ID}}, f.cookie, "")
	if res.StatusCode != 200 {
		t.Fatal("direct grant failed")
	}
	access, host, _ = f.grant(t, svc, login)
	res, b = f.request(t, "GET", "/", nil, access, host)
	if res.StatusCode != 200 || string(b) != "moved application" {
		t.Fatal("new Agent route failed")
	}
	count := len(f.s.store.snapshot().Services)
	res, _ = f.request(t, "POST", "/api/services/bulk", map[string]any{"services": []map[string]string{{"name": "valid", "siteID": old.ID, "url": upstream.URL}, {"name": "invalid", "siteID": "missing", "url": upstream.URL}}}, f.cookie, "")
	if res.StatusCode != 400 || len(f.s.store.snapshot().Services) != count {
		t.Fatal("bulk create partially committed")
	}
	res, b = f.request(t, "POST", "/api/services/bulk", map[string]any{"services": []map[string]string{{"name": "first", "siteID": old.ID, "url": upstream.URL}, {"name": "second", "siteID": old.ID, "url": upstream.URL}}}, f.cookie, "")
	if res.StatusCode != 201 {
		t.Fatal(string(b))
	}
	created := mustJSON[[]Service](t, b)
	ids := []string{created[0].ID, created[1].ID}
	res, _ = f.request(t, "POST", "/api/services/bulk-move", map[string]any{"ids": []string{ids[0], "missing"}, "siteID": next.ID}, f.cookie, "")
	current, _ := f.s.store.service(ids[0])
	if res.StatusCode != 400 || current.SiteID != old.ID {
		t.Fatal("bulk move partially committed")
	}
	res, _ = f.request(t, "POST", "/api/services/bulk-move", map[string]any{"ids": ids, "siteID": next.ID}, f.cookie, "")
	if res.StatusCode != 200 {
		t.Fatal("bulk move failed")
	}
	res, _ = f.request(t, "POST", "/api/services/bulk-delete", map[string]any{"ids": append(ids, svc.ID)}, login, "")
	if res.StatusCode != 403 {
		t.Fatal("member performed bulk delete")
	}
	res, _ = f.request(t, "POST", "/api/services/bulk-delete", map[string]any{"ids": append(ids, svc.ID)}, f.cookie, "")
	if res.StatusCode != 200 || len(f.s.store.snapshot().Services) != 0 {
		t.Fatal("bulk delete failed")
	}
	if len(f.s.store.snapshot().Users[1].ServiceIDs) != 0 {
		t.Fatal("bulk delete retained grant reference")
	}
}
