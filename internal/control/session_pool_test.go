package control

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

type connectionIdentity struct {
	mu   sync.Mutex
	user string
}
type identityKey struct{}

func TestSessionPoolDoesNotShareConnectionIdentity(t *testing.T) {
	f := setup(t)
	up := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := r.Context().Value(identityKey{}).(*connectionIdentity)
		c.mu.Lock()
		defer c.mu.Unlock()
		if auth := r.Header.Get("Authorization"); auth != "" {
			c.user = auth
		}
		if c.user == "" {
			io.WriteString(w, "anonymous")
		} else {
			io.WriteString(w, c.user)
		}
	}))
	up.Config.ConnContext = func(ctx context.Context, _ net.Conn) context.Context {
		return context.WithValue(ctx, identityKey{}, &connectionIdentity{})
	}
	up.Start()
	defer up.Close()
	site, token := f.site(t, "bound")
	svc := f.service(t, site, "legacy", up.URL)
	res, b := f.request(t, "PATCH", "/api/services/"+svc.ID, map[string]string{"name": svc.Name, "url": svc.URL, "connectionMode": "session"}, f.cookie, "")
	if res.StatusCode != 200 {
		t.Fatal(string(b))
	}
	svc = mustJSON[Service](t, b)
	f.agent(t, site, token, portOf(up.URL))
	_, bob := f.member(t, nil, []string{svc.ID})
	aliceCookie, host, _ := f.grant(t, svc, f.cookie)
	bobCookie, _, _ := f.grant(t, svc, bob)
	request := func(cookie *http.Cookie, auth string) string {
		t.Helper()
		r, _ := http.NewRequest("GET", f.h.URL+"/", nil)
		r.Host = host
		r.AddCookie(cookie)
		r.Header.Set("Authorization", auth)
		res, e := http.DefaultClient.Do(r)
		if e != nil {
			t.Fatal(e)
		}
		defer res.Body.Close()
		data, _ := io.ReadAll(res.Body)
		if res.StatusCode != 200 {
			t.Fatalf("proxy: %d %s", res.StatusCode, data)
		}
		return string(data)
	}
	if request(aliceCookie, "Mock Alice") != "Mock Alice" || request(aliceCookie, "") != "Mock Alice" {
		t.Fatal("Alice connection identity not retained")
	}
	if request(bobCookie, "") != "anonymous" {
		t.Fatal("Alice identity leaked to Bob")
	}
	if request(bobCookie, "Mock Bob") != "Mock Bob" || request(aliceCookie, "") != "Mock Alice" || request(bobCookie, "") != "Mock Bob" {
		t.Fatal("login session pools mixed")
	}
	// Health traffic must have a third, unauthenticated transport.
	transport := f.s.serviceTransport(svc)
	response, e := (&http.Client{Transport: transport, Timeout: 3 * time.Second}).Get(up.URL)
	if e != nil {
		t.Fatal(e)
	}
	data, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if string(data) != "anonymous" {
		t.Fatal("probe reused user connection identity")
	}
	f.request(t, "POST", "/api/logout", nil, f.cookie, "")
	f.s.poolMu.Lock()
	for _, p := range f.s.pools {
		if p.sessionKey == digest(f.cookie.Value) {
			t.Error("logout retained idle session pool")
		}
	}
	f.s.poolMu.Unlock()
	if request(bobCookie, "") != "Mock Bob" {
		t.Fatal("Alice logout affected Bob")
	}
}
