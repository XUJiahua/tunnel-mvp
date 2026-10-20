package control

import (
	"encoding/json"
	"mime"
	"net/http"
	"net/url"
	"time"
)

const logoutEvent = "http://schemas.openid.net/event/backchannel-logout"

// The back-channel endpoint is deliberately outside browser Origin checks. Its
// authority is the IdP signature and token claims, never a console Cookie.
func (s *Server) backchannelLogout(w http.ResponseWriter, r *http.Request) {
	if s.oidc == nil {
		http.NotFound(w, r)
		return
	}
	media, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if media != "application/x-www-form-urlencoded" {
		fail(w, 400, "invalid logout request")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil || len(r.PostForm["logout_token"]) != 1 {
		fail(w, 400, "invalid logout request")
		return
	}
	id, err := s.oidc.verifier.Verify(r.Context(), r.PostForm.Get("logout_token"))
	if err != nil {
		fail(w, 400, "invalid logout token")
		return
	}
	var claims struct {
		SID      string                     `json:"sid"`
		IssuedAt int64                      `json:"iat"`
		JTI      string                     `json:"jti"`
		Nonce    json.RawMessage            `json:"nonce"`
		Events   map[string]json.RawMessage `json:"events"`
	}
	if err = id.Claims(&claims); err != nil {
		fail(w, 400, "invalid logout token")
		return
	}
	var event map[string]any
	eventRaw, present := claims.Events[logoutEvent]
	eventErr := json.Unmarshal(eventRaw, &event)
	now := time.Now()
	issued := time.Unix(claims.IssuedAt, 0)
	if (id.Subject == "" && claims.SID == "") || len(id.Subject) > 255 || len(claims.SID) > 1024 || claims.JTI == "" || len(claims.JTI) > 1024 || claims.IssuedAt == 0 || issued.After(now.Add(30*time.Second)) || issued.Before(now.Add(-5*time.Minute)) || !present || eventErr != nil || event == nil || len(claims.Nonce) > 0 {
		fail(w, 400, "invalid logout token")
		return
	}
	// Serialize revocation with callback session issuance and proxy registration.
	unlock := s.lockMutation(r)
	defer unlock()
	s.oidc.mu.Lock()
	for key, expires := range s.oidc.logoutReplay {
		if !now.Before(expires) {
			delete(s.oidc.logoutReplay, key)
		}
	}
	replayKey := digest(id.Issuer + "\x00" + claims.JTI)
	if _, seen := s.oidc.logoutReplay[replayKey]; seen {
		s.oidc.mu.Unlock()
		jsonOut(w, 200, map[string]bool{"ok": true})
		return
	}
	if len(s.oidc.logoutReplay) >= 4096 {
		s.oidc.mu.Unlock()
		fail(w, 503, "logout replay window is full; retry later")
		return
	}
	// Retain the JTI through the maximum accepted iat window, not just exp: the
	// same signed token cannot log out a later session after a clock skew.
	s.oidc.logoutReplay[replayKey] = issued.Add(5*time.Minute + 30*time.Second)
	for key, cutoff := range s.oidc.logoutCutoff {
		if cutoff.Before(now.Add(-5 * time.Minute)) {
			delete(s.oidc.logoutCutoff, key)
		}
	}
	if s.oidc.logoutCutoff == nil {
		s.oidc.logoutCutoff = map[string]time.Time{}
	}
	if claims.SID != "" {
		s.oidc.logoutCutoff["sid:"+claims.SID] = now
	} else {
		s.oidc.logoutCutoff["sub:"+id.Subject] = now
	}
	s.oidc.mu.Unlock()
	keys := map[string]bool{}
	s.auth.mu.Lock()
	for key, value := range s.auth.sessions {
		if value.Method != "oidc" || value.Issuer != id.Issuer {
			continue
		}
		if claims.SID != "" && value.SID != claims.SID {
			continue
		}
		if id.Subject != "" && value.Subject != id.Subject {
			continue
		}
		keys[key] = true
	}
	s.auth.mu.Unlock()
	s.revokeSessionKeys(keys)
	s.audit("oidc_backchannel_logout", "", "", "", map[string]any{"sessions": len(keys)})
	jsonOut(w, 200, map[string]bool{"ok": true})
}

// Logout initiation is an explicit POST with the normal browser Origin check.
// client_id avoids retaining raw ID Tokens merely to construct an id_token_hint;
// the IdP may ask the user to confirm ending its SSO session.
func (s *Server) logoutIdentity(w http.ResponseWriter, r *http.Request, actor User) {
	unlock := s.lockMutation(r)
	defer unlock()
	_, key, ok := s.currentUser(r)
	if !ok {
		fail(w, 401, "sign in required")
		return
	}
	s.auth.mu.Lock()
	sess := s.auth.sessions[key]
	s.auth.mu.Unlock()
	if s.oidc == nil || s.oidc.endSession == "" || sess.Method != "oidc" {
		fail(w, 400, "OIDC logout is unavailable for this session")
		return
	}
	target, _ := url.Parse(s.oidc.endSession)
	target.RawQuery = url.Values{"client_id": {s.cfg.OIDC.ClientID}, "post_logout_redirect_uri": {s.cfg.PublicURL + "/"}}.Encode()
	s.revokeSessionKeys(map[string]bool{key: true})
	s.clearConsoleCookie(w)
	s.audit("oidc_logout_initiated", actor.Username, "", actor.ID)
	jsonOut(w, 200, map[string]string{"url": target.String()})
}
