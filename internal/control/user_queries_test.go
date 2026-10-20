package control

import (
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/XUJiahua/tunnel-mvp/internal/state"
)

type failedUserRepository struct{ state.Repository }

func (*failedUserRepository) Snapshot() State { panic("failed user query must not fall back") }
func (*failedUserRepository) UserContext(string) (State, bool, error) {
	return State{}, false, errors.New("database unavailable")
}
func (*failedUserRepository) QueryReadFailures() uint64 { return 7 }

type metricsUserRepository struct{ failedUserRepository }

func (*metricsUserRepository) Snapshot() State { return State{} }

func TestUserQueryFailureAndMetrics(t *testing.T) {
	store := &Store{&failedUserRepository{}}
	if _, ok := store.user("admin"); ok {
		t.Fatal("failed authorization lookup accepted")
	}
	s := &Server{store: &Store{&metricsUserRepository{}}}
	w := httptest.NewRecorder()
	s.metrics(w, httptest.NewRequest("GET", "/api/metrics", nil), User{})
	if !strings.Contains(w.Body.String(), "tunnel_state_query_errors_total 7\n") {
		t.Fatal("query failure metric missing")
	}
}

func TestUserQueryLegacyProjectionSemantics(t *testing.T) {
	st := State{Users: []User{{ID: "user", Role: "viewer", RoleID: "operator", SiteIDs: []string{"site-a"}, ServiceIDs: []string{"direct"}}}, Roles: []state.Role{{ID: "operator", Permissions: []string{"audit:read"}}}, Groups: []Group{{ID: "group", MemberIDs: []string{"user"}, ServiceIDs: []string{"from-group"}}}}
	store := &Store{&snapshotResourceRepository{value: st}}
	u, ok := store.user("user")
	if !ok || !hasPermission(u, "audit:read") || !containsID(u.ServiceIDs, "direct") || !containsID(u.ServiceIDs, "from-group") {
		t.Fatal("legacy effective authorization changed")
	}
}
