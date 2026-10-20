package control

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/XUJiahua/tunnel-mvp/internal/state"
)

func directoryFilter(u User) state.DirectoryFilter {
	return state.DirectoryFilter{ExpectedRevision: u.AuthRevision, ActorID: u.ID, Global: u.Role == "admin", OrganizationID: u.OrganizationID, SiteIDs: u.SiteIDs, ServiceIDs: u.ServiceIDs}
}
func anyPermission(u User, permissions ...string) bool {
	for _, p := range permissions {
		if hasPermission(u, p) {
			return true
		}
	}
	return false
}
func (s *Server) directory(w http.ResponseWriter, r *http.Request, kind string, filter state.DirectoryFilter) (State, bool) {
	limit := 0
	var err error
	if raw, present := r.URL.Query()["limit"]; present {
		if len(raw) != 1 {
			fail(w, 400, "invalid page limit")
			return State{}, false
		}
		limit, err = strconv.Atoi(raw[0])
		if err != nil || limit < 1 || limit > 200 {
			fail(w, 400, "limit must be 1–200")
			return State{}, false
		}
	}
	q := state.DirectoryQuery{Kind: kind, Filter: filter, Limit: limit, Cursor: r.URL.Query().Get("cursor")}
	var page state.DirectoryPage
	if reader, ok := s.store.Repository.(state.DirectoryReader); ok {
		page, err = reader.Directory(q)
	} else {
		page, err = state.SnapshotDirectory(s.store.snapshot(), q)
	}
	if err != nil {
		switch {
		case errors.Is(err, state.ErrDirectoryChanged):
			fail(w, 409, err.Error())
		case errors.Is(err, state.ErrDirectoryQuery):
			fail(w, 400, err.Error())
		default:
			fail(w, 503, "directory temporarily unavailable")
		}
		return State{}, false
	}
	if page.NextCursor != "" {
		w.Header().Set("X-Tunnel-Next-Cursor", page.NextCursor)
	}
	return page.State, true
}
