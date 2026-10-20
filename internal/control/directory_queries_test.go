package control

import (
	"errors"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/XUJiahua/tunnel-mvp/internal/state"
)

type failedDirectoryRepository struct{ state.Repository }

func (*failedDirectoryRepository) Snapshot() State { panic("query errors must not fall back") }
func (*failedDirectoryRepository) Directory(state.DirectoryQuery) (state.DirectoryPage, error) {
	return state.DirectoryPage{}, errors.New("database unavailable")
}
func (*failedDirectoryRepository) UserByUsername(string) (User, bool, error) {
	return User{}, false, errors.New("database unavailable")
}
func (*failedDirectoryRepository) UserByIdentity(state.Identity) (User, bool, error) {
	return User{}, false, errors.New("database unavailable")
}
func TestDirectoryAndLoginFailureNoSnapshotFallback(t *testing.T) {
	repo := &failedDirectoryRepository{}
	s := &Server{store: &Store{repo}}
	w := httptest.NewRecorder()
	if _, ok := s.directory(w, httptest.NewRequest("GET", "/api/users?limit=10", nil), "users", state.DirectoryFilter{Global: true}); ok || w.Code != 503 || strings.Contains(w.Body.String(), "database unavailable") {
		t.Fatal("read failure not isolated", w.Code)
	}
	if _, found := s.store.userByUsername("admin"); found {
		t.Fatal("failed username read accepted")
	}
	if users := s.store.identityUsers("issuer", "subject"); len(users) != 0 {
		t.Fatal("failed identity read accepted")
	}
}
func TestDirectoryHTTPPagesRevisionAndLegacyShape(t *testing.T) {
	f := setup(t)
	for _, name := range []string{"One", "Two", "Three"} {
		f.site(t, name)
	}
	res, b := f.request(t, "GET", "/api/sites?limit=2", nil, f.cookie, "")
	sites := mustJSON[[]Site](t, b)
	cursor := res.Header.Get("X-Tunnel-Next-Cursor")
	if res.StatusCode != 200 || len(sites) != 2 || cursor == "" || strings.Contains(string(b), "tokenHash") {
		t.Fatal("first page invalid", res.StatusCode, string(b))
	}
	res, b = f.request(t, "GET", "/api/sites?limit=2&cursor="+url.QueryEscape(cursor), nil, f.cookie, "")
	rest := mustJSON[[]Site](t, b)
	if res.StatusCode != 200 || len(rest) != 1 || rest[0].ID == sites[0].ID || rest[0].ID == sites[1].ID || res.Header.Get("X-Tunnel-Next-Cursor") != "" {
		t.Fatal("second page invalid", string(b))
	}
	res, b = f.request(t, "GET", "/api/sites", nil, f.cookie, "")
	if res.StatusCode != 200 || len(mustJSON[[]Site](t, b)) != 3 {
		t.Fatal("legacy array changed")
	}
	f.site(t, "Four")
	res, _ = f.request(t, "GET", "/api/sites?limit=2&cursor="+url.QueryEscape(cursor), nil, f.cookie, "")
	if res.StatusCode != 409 {
		t.Fatal("mixed revision returned", res.StatusCode)
	}
	for _, query := range []string{"limit=0", "limit=-1", "limit=201", "limit=a", "limit=1&limit=2", "cursor=bad", "limit=1&cursor=bad"} {
		res, _ = f.request(t, "GET", "/api/sites?"+query, nil, f.cookie, "")
		if res.StatusCode != 400 {
			t.Fatal("invalid page accepted", query, res.StatusCode)
		}
	}
	res, b = f.request(t, "GET", "/api/roles?limit=2", nil, f.cookie, "")
	if res.StatusCode != 200 {
		t.Fatal(string(b))
	}
	shape := mustJSON[map[string]any](t, b)
	if _, ok := shape["roles"].([]any); !ok {
		t.Fatal("roles object changed")
	}
	if _, ok := shape["permissions"].([]any); !ok {
		t.Fatal("permissions changed")
	}
}
func TestDirectoryVisibilityBeforePagination(t *testing.T) {
	st := State{Sites: []Site{{ID: "foreign", OrganizationID: "b"}, {ID: "hidden", OrganizationID: "a"}, {ID: "visible", OrganizationID: "a"}}, Services: []Service{{ID: "foreign-app", SiteID: "foreign", OrganizationID: "b"}, {ID: "visible-app", SiteID: "visible", OrganizationID: "a"}}}
	s := &Server{store: &Store{&snapshotResourceRepository{value: st}}, connections: map[string]*agentConnection{}}
	actor := User{ID: "viewer", Role: "viewer", OrganizationID: "a", ServiceIDs: []string{"visible-app", "foreign-app"}}
	w := httptest.NewRecorder()
	s.listSites(w, httptest.NewRequest("GET", "/api/sites?limit=1", nil), actor)
	got := mustJSON[[]Site](t, w.Body.Bytes())
	if w.Code != 200 || len(got) != 1 || got[0].ID != "visible" || w.Header().Get("X-Tunnel-Next-Cursor") != "" {
		t.Fatal("authorization applied after limit", w.Code, w.Body.String())
	}
}
