package control

import (
	"encoding/json"
	"github.com/XUJiahua/tunnel-mvp/internal/state"
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
	revoked := 0
	keys := map[string]bool{}
	err = s.authUpdate(r.Context(), false, func(a *state.AuthData, _ State, now time.Time) error {
		if issued.After(now.Add(30*time.Second)) || issued.Before(now.Add(-5*time.Minute)) {
			return state.ErrAuthChanged
		}
		replayKey := digest(id.Issuer + "\x00" + claims.JTI)
		if _, seen := a.Replay[replayKey]; seen {
			return nil
		}
		if len(a.Replay) >= 4096 || len(a.Cutoff) >= 4096 {
			return state.ErrAuthCapacity
		}
		a.Replay[replayKey] = issued.Add(5*time.Minute + 30*time.Second)
		if claims.SID != "" {
			a.Cutoff["sid:"+claims.SID] = now
		} else {
			a.Cutoff["sub:"+id.Subject] = now
		}
		for key, value := range a.Sessions {
			if value.Method != "oidc" || value.Issuer != id.Issuer {
				continue
			}
			if claims.SID != "" && value.SID != claims.SID {
				continue
			}
			if id.Subject != "" && value.Subject != id.Subject {
				continue
			}
			delete(a.Sessions, key)
			keys[key] = true
			revoked++
		}
		return nil
	})
	if err != nil {
		fail(w, 503, "logout state unavailable; retry later")
		return
	}
	s.pruneSessionPools()
	s.cancelRequests(func(req liveRequest) bool { return keys[req.sessionKey] })
	s.audit("oidc_backchannel_logout", "", "", "", map[string]any{"sessions": revoked})
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
	sess, found, err := s.authSession(r.Context(), key)
	if err != nil || !found {
		fail(w, 503, "session storage unavailable")
		return
	}
	if s.oidc == nil || s.oidc.endSession == "" || sess.Method != "oidc" {
		fail(w, 400, "OIDC logout is unavailable for this session")
		return
	}
	target, _ := url.Parse(s.oidc.endSession)
	target.RawQuery = url.Values{"client_id": {s.cfg.OIDC.ClientID}, "post_logout_redirect_uri": {s.cfg.PublicURL + "/"}}.Encode()
	if err := s.revokeSessionKeys(map[string]bool{key: true}); err != nil {
		fail(w, 503, "session revocation unavailable")
		return
	}
	s.clearConsoleCookie(w)
	s.audit("oidc_logout_initiated", actor.Username, "", actor.ID)
	jsonOut(w, 200, map[string]string{"url": target.String()})
}
