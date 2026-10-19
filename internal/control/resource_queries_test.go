package control

import (
	"errors"
	"net/url"
	"testing"

	"github.com/XUJiahua/tunnel-mvp/internal/state"
)

type failedResourceRepository struct{ state.Repository }

func (*failedResourceRepository) Snapshot() State {
	panic("failed SQL read must not fall back to snapshot")
}
func (*failedResourceRepository) SiteByID(string) (Site, bool, error) {
	return Site{}, false, errors.New("database unavailable")
}
func (*failedResourceRepository) ServiceByID(string) (Service, bool, error) {
	return Service{}, false, errors.New("database unavailable")
}
func (*failedResourceRepository) ServiceByDomain(string) (Service, bool, error) {
	return Service{}, false, errors.New("database unavailable")
}

func TestResourceQueryFailureDoesNotUseSnapshot(t *testing.T) {
	store := &Store{&failedResourceRepository{}}
	if _, ok := store.site("site-a"); ok {
		t.Fatal("failed site read accepted")
	}
	if _, ok := store.service("app"); ok {
		t.Fatal("failed service read accepted")
	}
	if _, ok := store.serviceByDomain("demo.example.com"); ok {
		t.Fatal("failed domain read accepted")
	}
	access, _ := url.Parse("http://apps.localhost:18080")
	s := &Server{store: store, access: access}
	for _, host := range []string{"app.apps.localhost:18080", "demo.example.com:18080"} {
		if _, ok := s.serviceForHost(host); ok {
			t.Fatal("failed gateway route accepted")
		}
	}
}

type snapshotResourceRepository struct{ value State }

func (r *snapshotResourceRepository) Snapshot() State { return state.Clone(r.value) }
func (*snapshotResourceRepository) Update(func(*State) error) error {
	return errors.New("read only test repository")
}
func (*snapshotResourceRepository) Close() error { return nil }

func TestResourceQueryLegacyRepositoryAndHostMatching(t *testing.T) {
	repo := &snapshotResourceRepository{State{Sites: []Site{{ID: "site-a"}}, Services: []Service{{ID: "app", SiteID: "site-a", CustomDomain: "demo.example.com"}, {ID: "other", SiteID: "site-a"}}}}
	store := &Store{repo}
	if _, ok := store.site("site-a"); !ok {
		t.Fatal("legacy site lookup missing")
	}
	for _, origin := range []string{"http://apps.localhost:18080", "https://apps.example.net"} {
		access, _ := url.Parse(origin)
		s := &Server{store: store, access: access}
		for _, host := range []string{"app." + access.Host, customHost("demo.example.com", access)} {
			v, ok := s.serviceForHost(host)
			if !ok || v.ID != "app" {
				t.Fatal("route missing", host)
			}
		}
		for _, host := range []string{"missing." + access.Host, "notdemo.example.com", "demo.example.com:wrong", "app.attacker.example"} {
			if _, ok := s.serviceForHost(host); ok {
				t.Fatal("wrong host matched", host)
			}
		}
	}
	if _, ok := store.serviceByDomain(""); ok {
		t.Fatal("empty custom domain matched default service")
	}
}
