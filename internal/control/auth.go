package control

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const consoleCookie = "tunnel_console"
const accessCookie = "tunnel_access"

type session struct {
	UserID  string
	Expires time.Time
}
type grant struct {
	SessionKey string
	ServiceID  string
	Expires    time.Time
}
type loginLimit struct {
	Count int
	Until time.Time
}
type authState struct {
	mu       sync.Mutex
	sessions map[string]session
	tickets  map[string]grant
	access   map[string]grant
	attempts map[string]loginLimit
}

func newAuth() *authState {
	return &authState{sessions: map[string]session{}, tickets: map[string]grant{}, access: map[string]grant{}, attempts: map[string]loginLimit{}}
}
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
func (a *authState) clean(now time.Time) { // caller holds mu
	for k, v := range a.sessions {
		if now.After(v.Expires) {
			delete(a.sessions, k)
		}
	}
	for k, v := range a.tickets {
		if now.After(v.Expires) {
			delete(a.tickets, k)
		}
	}
	for k, v := range a.access {
		if now.After(v.Expires) {
			delete(a.access, k)
		}
	}
	for k, v := range a.attempts {
		if now.After(v.Until) {
			delete(a.attempts, k)
		}
	}
}
func (s *Server) currentUser(r *http.Request) (User, string, bool) {
	c, err := r.Cookie(consoleCookie)
	if err != nil {
		return User{}, "", false
	}
	key := digest(c.Value)
	s.auth.mu.Lock()
	sess, ok := s.auth.sessions[key]
	s.auth.mu.Unlock()
	if !ok || time.Now().After(sess.Expires) {
		return User{}, "", false
	}
	u, ok := s.store.user(sess.UserID)
	return u, key, ok
}
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !decode(w, r, &body) {
		return
	}
	// Global bounded window prevents unbounded keyed maps and limits bcrypt work.
	s.auth.mu.Lock()
	s.auth.clean(time.Now())
	lim := s.auth.attempts["login"]
	if lim.Count >= 60 && time.Now().Before(lim.Until) {
		s.auth.mu.Unlock()
		fail(w, 429, "too many login attempts; retry in one minute")
		return
	}
	if lim.Until.IsZero() {
		lim.Until = time.Now().Add(time.Minute)
	}
	lim.Count++
	s.auth.attempts["login"] = lim
	s.auth.mu.Unlock()
	var found User
	for _, u := range s.store.snapshot().Users {
		if u.Username == body.Username {
			found = u
		}
	}
	hash := found.PasswordHash
	if hash == "" {
		hash = s.dummyHash
	}
	err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(body.Password))
	if err != nil || found.ID == "" {
		s.audit("login_failed", body.Username, "", "")
		fail(w, 401, "invalid username or password")
		return
	}
	token := randomID()
	s.auth.mu.Lock()
	s.auth.sessions[digest(token)] = session{found.ID, time.Now().Add(12 * time.Hour)}
	s.auth.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: consoleCookie, Value: token, Path: "/", HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteLaxMode, MaxAge: 43200})
	found.PasswordHash = ""
	s.audit("login", found.Username, "", "")
	jsonOut(w, 200, found)
}
func constantEqual(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }
