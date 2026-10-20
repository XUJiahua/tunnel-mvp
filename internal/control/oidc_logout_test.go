package control

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func signLogout(t *testing.T, p *testIdP, claims map[string]any, wrong bool) string {
	t.Helper()
	enc := base64.RawURLEncoding.EncodeToString
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": "key1", "typ": "logout+jwt"})
	body, _ := json.Marshal(claims)
	unsigned := enc(header) + "." + enc(body)
	digest := sha256.Sum256([]byte(unsigned))
	key := p.key
	if wrong {
		key = p.wrongKey
	}
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return unsigned + "." + enc(sig)
}
func logoutClaims(p *testIdP) map[string]any {
	return map[string]any{"iss": p.server.URL, "aud": "console", "iat": time.Now().Unix(), "exp": time.Now().Add(time.Minute).Unix(), "jti": randomID(), "sid": "sid-a", "events": map[string]any{logoutEvent: map[string]any{}}}
}
func sendLogout(t *testing.T, f *fixture, token string) int {
	t.Helper()
	res, err := http.Post(f.h.URL+"/auth/oidc/backchannel-logout", "application/x-www-form-urlencoded", strings.NewReader(url.Values{"logout_token": {token}}.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	return res.StatusCode
}
func oidcLoginCookie(t *testing.T, f *fixture, p *testIdP) *http.Cookie {
	t.Helper()
	path, browser, _ := startFlow(t, f, p)
	res, _ := f.request(t, "GET", path, nil, browser, "")
	cookie := consoleSession(res)
	if cookie == nil {
		t.Fatal("OIDC login failed")
	}
	return cookie
}
func TestBackchannelLogoutScopesAndReplay(t *testing.T) {
	f, p, _ := oidcFixture(t)
	first := oidcLoginCookie(t, f, p)
	p.mu.Lock()
	p.mode = "sid-b"
	p.mu.Unlock()
	second := oidcLoginCookie(t, f, p)
	claims := logoutClaims(p)
	token := signLogout(t, p, claims, false)
	if code := sendLogout(t, f, token); code != 200 {
		t.Fatal("valid logout rejected", code)
	}
	for _, tc := range []struct {
		cookie *http.Cookie
		want   int
	}{{first, 401}, {second, 200}, {f.cookie, 200}} {
		res, _ := f.request(t, "GET", "/api/me", nil, tc.cookie, "")
		if res.StatusCode != tc.want {
			t.Fatal("wrong logout scope", res.StatusCode, tc.want)
		}
	}
	p.mu.Lock()
	p.mode = ""
	p.mu.Unlock()
	newLogin := oidcLoginCookie(t, f, p)
	if sendLogout(t, f, token) != 200 {
		t.Fatal("retry should be idempotent")
	}
	res, _ := f.request(t, "GET", "/api/me", nil, newLogin, "")
	if res.StatusCode != 200 {
		t.Fatal("replay revoked a new session")
	}
	all := logoutClaims(p)
	delete(all, "sid")
	all["sub"] = "subject-A"
	if sendLogout(t, f, signLogout(t, p, all, false)) != 200 {
		t.Fatal("subject logout failed")
	}
	for _, cookie := range []*http.Cookie{second, newLogin} {
		res, _ := f.request(t, "GET", "/api/me", nil, cookie, "")
		if res.StatusCode != 401 {
			t.Fatal("subject session survived")
		}
	}
}
func TestBackchannelRejectsInvalidTokens(t *testing.T) {
	f, p, _ := oidcFixture(t)
	login := oidcLoginCookie(t, f, p)
	for _, mode := range []string{"signature", "issuer", "audience", "expired", "missing-exp", "future-iat", "old-iat", "jti", "event", "null-event", "nonce", "null-nonce", "no-target"} {
		t.Run(mode, func(t *testing.T) {
			claims := logoutClaims(p)
			switch mode {
			case "issuer":
				claims["iss"] = "https://other.example"
			case "audience":
				claims["aud"] = "other"
			case "expired":
				claims["exp"] = time.Now().Add(-time.Minute).Unix()
			case "missing-exp":
				delete(claims, "exp")
			case "future-iat":
				claims["iat"] = time.Now().Add(time.Minute).Unix()
			case "old-iat":
				claims["iat"] = time.Now().Add(-6 * time.Minute).Unix()
			case "jti":
				delete(claims, "jti")
			case "event":
				claims["events"] = map[string]any{"other": map[string]any{}}
			case "null-event":
				claims["events"] = map[string]any{logoutEvent: nil}
			case "nonce":
				claims["nonce"] = "forbidden"
			case "null-nonce":
				claims["nonce"] = nil
			case "no-target":
				delete(claims, "sid")
			}
			if code := sendLogout(t, f, signLogout(t, p, claims, mode == "signature")); code != 400 {
				t.Fatalf("accepted %s: %d", mode, code)
			}
		})
	}
	res, _ := f.request(t, "GET", "/api/me", nil, login, "")
	if res.StatusCode != 200 {
		t.Fatal("invalid logout revoked a session")
	}
}
func TestLogoutWhileAuthorizationIsPending(t *testing.T) {
	f, p, _ := oidcFixture(t)
	path, cookie, _ := startFlow(t, f, p)
	claims := logoutClaims(p)
	if sendLogout(t, f, signLogout(t, p, claims, false)) != 200 {
		t.Fatal("logout failed")
	}
	res, _ := f.request(t, "GET", path, nil, cookie, "")
	if consoleSession(res) != nil {
		t.Fatal("pending flow resurrected logged-out IdP session")
	}
	if oidcLoginCookie(t, f, p) == nil {
		t.Fatal("new authentication could not recover")
	}
}
func TestRequiredAuthenticationContext(t *testing.T) {
	f, p, _ := oidcFixture(t)
	f.s.cfg.OIDC.RequiredACR = []string{"2"}
	f.s.cfg.OIDC.RequiredAMR = []string{"otp"}
	path, browser, _ := startFlow(t, f, p)
	res, _ := f.request(t, "GET", path, nil, browser, "")
	if consoleSession(res) != nil {
		t.Fatal("weak authentication accepted")
	}
	p.mu.Lock()
	p.mode = "mfa"
	p.mu.Unlock()
	if oidcLoginCookie(t, f, p) == nil {
		t.Fatal("strong authentication rejected")
	}
}
