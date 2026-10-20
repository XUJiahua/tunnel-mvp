package control

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/XUJiahua/tunnel-mvp/internal/state"
	"golang.org/x/crypto/bcrypt"
)

const consoleCookie = "tunnel_console"
const accessCookie = "tunnel_access"

type session = state.AuthSession
type grant = state.AuthGrant
type loginLimit = state.AuthLimit
type authState struct {
	mu sync.Mutex
	state.AuthData
	shared state.AuthRepository
}

func newAuth() *authState { return &authState{AuthData: state.NewAuthData()} }
func jsonOut(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
func fail(w http.ResponseWriter, code int, msg string) {
	jsonOut(w, code, map[string]string{"error": msg})
}
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		fail(w, 415, "application/json required")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		fail(w, 400, "invalid JSON body")
		return false
	}
	return true
}
func (a *authState) clean(now time.Time) { a.AuthData.Clean(now) }
func (s *Server) currentUser(r *http.Request) (User, string, bool) {
	cookie, err := r.Cookie(consoleCookie)
	if err != nil {
		return User{}, "", false
	}
	key := digest(cookie.Value)
	result, err := s.resolveAuth(r.Context(), "session", key)
	if err != nil {
		return User{}, "", false
	}
	user, ok := s.resolvedUser(result)
	if s.auth.shared != nil {
		user.AuthRevision = &result.ConfigRevision
	}
	return user, key, ok
}
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	s.accessMu.RLock()
	defer s.accessMu.RUnlock()
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !decode(w, r, &body) {
		return
	}
	allowed, err := s.authRateLimit(r.Context(), "login", 60, time.Minute)
	if err != nil {
		fail(w, 503, "authentication storage unavailable")
		return
	}
	if !allowed {
		fail(w, 429, "too many login attempts; retry in one minute")
		return
	}
	found, exists := s.store.userByUsername(body.Username)
	hash := found.PasswordHash
	if hash == "" {
		hash = s.dummyHash
	}
	err = bcrypt.CompareHashAndPassword([]byte(hash), []byte(body.Password))
	if err != nil || !exists || found.ID == "" || found.Disabled || found.PasswordHash == "" {
		s.audit("login_failed", body.Username, "", "")
		fail(w, 401, "invalid username or password")
		return
	}
	if !s.issueSession(w, found, time.Now().Add(12*time.Hour), sessionMetadata(r, "password", "", "", "")) {
		return
	}
	s.audit("login", found.Username, "", "")
	found, _ = s.store.user(found.ID)
	jsonOut(w, 200, publicUser(found))
}
func constantEqual(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }
