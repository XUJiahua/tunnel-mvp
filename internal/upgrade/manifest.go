// Package upgrade verifies and stages signed Agent executables. It never trusts
// control-plane credentials as release signing authority.
package upgrade

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const MaxBinarySize int64 = 128 << 20
const MaxManifestSize int64 = 128 << 10

type Artifact struct {
	Platform string `json:"platform"`
	URL      string `json:"url"`
	SHA256   string `json:"sha256"`
	Size     int64  `json:"size"`
}
type Manifest struct {
	Format    int        `json:"format"`
	Sequence  uint64     `json:"sequence"`
	Version   string     `json:"version"`
	ExpiresAt time.Time  `json:"expiresAt"`
	Artifacts []Artifact `json:"artifacts"`
}
type Envelope struct {
	Payload   string `json:"payload"`
	Signature string `json:"signature"`
}

func strictJSON(raw []byte, out any) error {
	d := json.NewDecoder(strings.NewReader(string(raw)))
	d.DisallowUnknownFields()
	if e := d.Decode(out); e != nil {
		return e
	}
	if d.Decode(new(any)) != io.EOF {
		return fmt.Errorf("trailing JSON content")
	}
	return nil
}
func validURL(raw string) error {
	u, e := url.Parse(raw)
	if e != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery {
		return fmt.Errorf("invalid update URL")
	}
	if u.Scheme != "https" && (u.Scheme != "http" || (u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost" && u.Hostname() != "::1")) {
		return fmt.Errorf("updates require HTTPS except loopback")
	}
	return nil
}
func validateManifest(m Manifest, now time.Time) error {
	if m.Format != 1 || m.Sequence == 0 || m.Version == "" || len(m.Version) > 100 || strings.ContainsAny(m.Version, "\x00\r\n") || !m.ExpiresAt.After(now) || m.ExpiresAt.After(now.Add(7*24*time.Hour)) || len(m.Artifacts) == 0 || len(m.Artifacts) > 6 {
		return fmt.Errorf("invalid or expired update manifest")
	}
	seen := map[string]bool{}
	for _, a := range m.Artifacts {
		valid := false
		for _, os := range []string{"linux", "darwin", "windows"} {
			for _, arch := range []string{"amd64", "arm64"} {
				valid = valid || a.Platform == os+"/"+arch
			}
		}
		hash, e := hex.DecodeString(a.SHA256)
		if !valid || seen[a.Platform] || e != nil || len(hash) != 32 || a.Size <= 0 || a.Size > MaxBinarySize {
			return fmt.Errorf("invalid update artifact")
		}
		if e := validURL(a.URL); e != nil {
			return e
		}
		seen[a.Platform] = true
	}
	return nil
}
func Sign(m Manifest, key ed25519.PrivateKey) ([]byte, error) {
	if len(key) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("invalid Ed25519 private key")
	}
	if e := validateManifest(m, time.Now()); e != nil {
		return nil, e
	}
	raw, e := json.Marshal(m)
	if e != nil {
		return nil, e
	}
	return json.MarshalIndent(Envelope{Payload: base64.StdEncoding.EncodeToString(raw), Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(key, raw))}, "", "  ")
}
func Verify(raw []byte, key ed25519.PublicKey, now time.Time) (Manifest, error) {
	var m Manifest
	if int64(len(raw)) > MaxManifestSize || len(key) != ed25519.PublicKeySize {
		return m, fmt.Errorf("invalid manifest size or public key")
	}
	var env Envelope
	if e := strictJSON(raw, &env); e != nil {
		return m, e
	}
	payload, e := base64.StdEncoding.DecodeString(env.Payload)
	if e != nil {
		return m, fmt.Errorf("invalid signed payload")
	}
	sig, e := base64.StdEncoding.DecodeString(env.Signature)
	if e != nil || !ed25519.Verify(key, payload, sig) {
		return m, fmt.Errorf("update signature verification failed")
	}
	if e := strictJSON(payload, &m); e != nil {
		return m, e
	}
	return m, validateManifest(m, now)
}
func download(ctx context.Context, raw string) (*http.Response, error) {
	if e := validURL(raw); e != nil {
		return nil, e
	}
	req, e := http.NewRequestWithContext(ctx, "GET", raw, nil)
	if e != nil {
		return nil, e
	}
	res, e := (&http.Client{Timeout: 60 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
	if e != nil {
		return nil, fmt.Errorf("update download failed")
	}
	if res.StatusCode != 200 {
		res.Body.Close()
		return nil, fmt.Errorf("update endpoint returned HTTP %d", res.StatusCode)
	}
	return res, nil
}
func Fetch(ctx context.Context, raw string, key ed25519.PublicKey) (Manifest, error) {
	res, e := download(ctx, raw)
	if e != nil {
		return Manifest{}, e
	}
	defer res.Body.Close()
	b, e := io.ReadAll(io.LimitReader(res.Body, MaxManifestSize+1))
	if e != nil {
		return Manifest{}, e
	}
	return Verify(b, key, time.Now())
}
