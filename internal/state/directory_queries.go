package state

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
)

// DirectoryFilter is supplied by the trusted control plane, never decoded from
// request parameters. Global bypasses organization scoping for the root admin.
type DirectoryFilter struct {
	ExpectedRevision *uint64 `json:"-"`
	ActorID          string
	Global           bool
	OrganizationID   string
	ExcludeAdmins    bool
	All              bool
	AllServices      bool
	MemberID         string
	SiteIDs          []string
	ServiceIDs       []string
}
type DirectoryQuery struct {
	Kind   string
	Filter DirectoryFilter
	Limit  int // zero preserves the legacy complete-array API
	Cursor string
}
type DirectoryPage struct {
	State      State
	NextCursor string
}
type DirectoryReader interface {
	Directory(DirectoryQuery) (DirectoryPage, error)
}

var ErrDirectoryChanged = errors.New("directory changed; restart pagination")
var ErrDirectoryQuery = errors.New("invalid directory query")

type directoryCursor struct {
	Epoch    string
	Revision uint64
	Scope    string
	Position int
}

func directoryScope(q DirectoryQuery) (string, error) {
	switch q.Kind {
	case "users", "sites", "services", "groups", "roles", "invitations", "organizations":
	default:
		return "", ErrDirectoryQuery
	}
	if q.Limit < 0 || q.Limit > 200 || len(q.Cursor) > 2048 || q.Cursor != "" && q.Limit == 0 {
		return "", ErrDirectoryQuery
	}
	raw, _ := json.Marshal(struct {
		Kind   string
		Filter DirectoryFilter
	}{q.Kind, q.Filter})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}
func directoryStart(q DirectoryQuery, epoch string, revision uint64) (int, string, error) {
	scope, err := directoryScope(q)
	if err != nil {
		return 0, "", err
	}
	if q.Cursor == "" {
		return -1, scope, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(q.Cursor)
	if err != nil {
		return 0, "", ErrDirectoryQuery
	}
	var c directoryCursor
	if err = decodeStrict(raw, &c); err != nil || c.Position < 0 {
		return 0, "", ErrDirectoryQuery
	}
	if c.Epoch != epoch || c.Revision != revision {
		return 0, "", ErrDirectoryChanged
	}
	if c.Scope != scope {
		return 0, "", ErrDirectoryQuery
	}
	return c.Position, scope, nil
}
func directoryNext(epoch string, revision uint64, scope string, position int) string {
	raw, _ := json.Marshal(directoryCursor{epoch, revision, scope, position})
	return base64.RawURLEncoding.EncodeToString(raw)
}
func (s *Store) Directory(q DirectoryQuery) (DirectoryPage, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return DirectoryPage{}, fmt.Errorf("state store is closed")
	}
	after, scope, err := directoryStart(q, s.epoch, s.revision)
	if err != nil {
		return DirectoryPage{}, err
	}
	var page DirectoryPage
	var last int
	var more bool
	if b, ok := s.disk.(*sqliteBackend); ok {
		page.State, last, more, err = b.directory(q, after)
		if err != nil {
			s.queryReadFailures.Add(1)
			return DirectoryPage{}, err
		}
	} else {
		page.State, last, more = memoryDirectory(s.state, q, after)
	}
	if more {
		page.NextCursor = directoryNext(s.epoch, s.revision, scope, last)
	}
	return page, nil
}

// SnapshotDirectory supports third-party Repository adapters without the optional
// reader. Its digest also rejects mixed revisions; built-in stores never use it.
func SnapshotDirectory(st State, q DirectoryQuery) (DirectoryPage, error) {
	raw, _ := json.Marshal(st)
	sum := sha256.Sum256(raw)
	epoch := hex.EncodeToString(sum[:])
	after, scope, err := directoryStart(q, epoch, 0)
	if err != nil {
		return DirectoryPage{}, err
	}
	out, last, more := memoryDirectory(st, q, after)
	page := DirectoryPage{State: out}
	if more {
		page.NextCursor = directoryNext(epoch, 0, scope, last)
	}
	return page, nil
}
func visitSlice[T any](vs []T, visit func(int, any) bool) {
	for i, v := range vs {
		if !visit(i, v) {
			return
		}
	}
}
func visitDirectory(st State, kind string, visit func(int, any) bool) {
	switch kind {
	case "users":
		visitSlice(st.Users, visit)
	case "sites":
		visitSlice(st.Sites, visit)
	case "services":
		visitSlice(st.Services, visit)
	case "groups":
		visitSlice(st.Groups, visit)
	case "roles":
		visitSlice(st.Roles, visit)
	case "invitations":
		visitSlice(st.Invites, visit)
	case "organizations":
		visitSlice(st.Organizations, visit)
	}
}
func directoryMatch(st State, v any, f DirectoryFilter) bool {
	owned := func(org string) bool { return f.Global || org == f.OrganizationID }
	service := func(v Service) bool {
		return owned(v.OrganizationID) && (f.Global || f.All || slices.Contains(f.SiteIDs, v.SiteID) || slices.Contains(f.ServiceIDs, v.ID))
	}
	switch v := v.(type) {
	case User:
		return owned(v.OrganizationID) && (!f.ExcludeAdmins || v.Role != "admin")
	case Site:
		if !owned(v.OrganizationID) {
			return false
		}
		if f.Global || f.All || slices.Contains(f.SiteIDs, v.ID) {
			return true
		}
		for _, svc := range st.Services {
			if svc.SiteID == v.ID && (f.AllServices || slices.Contains(f.ServiceIDs, svc.ID)) {
				return true
			}
		}
		return false
	case Service:
		return service(v)
	case Group:
		return owned(v.OrganizationID) && (f.Global || f.All || slices.Contains(v.MemberIDs, f.MemberID))
	case Role:
		return owned(v.OrganizationID)
	case Invite:
		return owned(v.OrganizationID)
	case Organization:
		return f.Global || v.ID == f.OrganizationID
	}
	return false
}
func appendDirectory(st *State, v any) {
	switch v := v.(type) {
	case User:
		st.Users = append(st.Users, v)
	case Site:
		st.Sites = append(st.Sites, v)
	case Service:
		st.Services = append(st.Services, v)
	case Group:
		st.Groups = append(st.Groups, v)
	case Role:
		st.Roles = append(st.Roles, v)
	case Invite:
		st.Invites = append(st.Invites, v)
	case Organization:
		st.Organizations = append(st.Organizations, v)
	}
}
func memoryDirectory(st State, q DirectoryQuery, after int) (State, int, bool) {
	var out State
	last, count, more := -1, 0, false
	visitDirectory(st, q.Kind, func(pos int, v any) bool {
		if pos <= after || !directoryMatch(st, v, q.Filter) {
			return true
		}
		if q.Limit > 0 && count == q.Limit {
			more = true
			return false
		}
		appendDirectory(&out, v)
		count++
		last = pos
		return true
	})
	if q.Kind == "users" {
		for _, r := range st.Roles {
			for _, u := range out.Users {
				if u.RoleID == r.ID && u.OrganizationID == r.OrganizationID {
					out.Roles = append(out.Roles, r)
					break
				}
			}
		}
	}
	return Clone(out), last, more
}
