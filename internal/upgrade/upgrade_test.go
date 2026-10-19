package upgrade

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func fixture(t *testing.T) (*Manager, *httptest.Server, ed25519.PrivateKey, *[]byte, *[]byte) {
	t.Helper()
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	dir := t.TempDir()
	os.Chmod(dir, 0700)
	keyPath := filepath.Join(t.TempDir(), "release.pub")
	os.WriteFile(keyPath, []byte(base64.StdEncoding.EncodeToString(pub)), 0600)
	bootstrap := filepath.Join(t.TempDir(), "bootstrap")
	os.WriteFile(bootstrap, []byte("trusted original executable"), 0700)
	m, e := Open(dir, keyPath, bootstrap)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { m.Close() })
	binary := []byte("signed new executable")
	var envelope []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/manifest" {
			w.Write(envelope)
		} else {
			w.Write(binary)
		}
	}))
	t.Cleanup(server.Close)
	manifest := testManifest(server.URL, binary, 1)
	envelope, e = Sign(manifest, key)
	if e != nil {
		t.Fatal(e)
	}
	return m, server, key, &binary, &envelope
}
func testManifest(url string, binary []byte, seq uint64) Manifest {
	h := sha256.Sum256(binary)
	return Manifest{Format: 1, Sequence: seq, Version: "v2", ExpiresAt: time.Now().Add(time.Hour).UTC(), Artifacts: []Artifact{{Platform: runtime.GOOS + "/" + runtime.GOARCH, URL: url + "/binary", SHA256: hex.EncodeToString(h[:]), Size: int64(len(binary))}}}
}
func TestSignedManifestRejectsTamperingAndExpiry(t *testing.T) {
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	m := testManifest("https://releases.example.com", []byte("binary"), 1)
	raw, e := Sign(m, key)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = Verify(raw, pub, time.Now()); e != nil {
		t.Fatal(e)
	}
	other, _, _ := ed25519.GenerateKey(rand.Reader)
	if _, e = Verify(raw, other, time.Now()); e == nil {
		t.Fatal("wrong signing key accepted")
	}
	var env Envelope
	json.Unmarshal(raw, &env)
	env.Payload = base64.StdEncoding.EncodeToString([]byte(`{"sequence":9}`))
	tampered, _ := json.Marshal(env)
	if _, e = Verify(tampered, pub, time.Now()); e == nil {
		t.Fatal("tampered payload accepted")
	}
	if _, e = Verify(raw, pub, time.Now().Add(2*time.Hour)); e == nil {
		t.Fatal("expired manifest accepted")
	}
	for _, mode := range []string{"duplicate", "http", "query", "huge", "sequence", "future"} {
		bad := m
		bad.Artifacts = append([]Artifact{}, m.Artifacts...)
		switch mode {
		case "duplicate":
			bad.Artifacts = append(bad.Artifacts, bad.Artifacts[0])
		case "http":
			bad.Artifacts[0].URL = "http://example.com/binary"
		case "query":
			bad.Artifacts[0].URL += "?secret=x"
		case "huge":
			bad.Artifacts[0].Size = MaxBinarySize + 1
		case "sequence":
			bad.Sequence = 0
		case "future":
			bad.ExpiresAt = time.Now().Add(8 * 24 * time.Hour)
		}
		if _, e = Sign(bad, key); e == nil {
			t.Fatal("unsafe manifest signed", mode)
		}
	}
}
func TestManagerIntegrityRecoveryAndReplayProtection(t *testing.T) {
	m, server, key, binary, envelope := fixture(t)
	ctx := context.Background()
	original := m.Status().CurrentSHA
	release, e := m.Check(ctx, server.URL+"/manifest")
	if e != nil {
		t.Fatal(e)
	}
	good := append([]byte{}, (*binary)...)
	*binary = []byte("modified payload")
	if e = m.Stage(ctx, release); e == nil {
		t.Fatal("bad checksum accepted")
	}
	if m.Status().CurrentSHA != original {
		t.Fatal("failed staging changed executable")
	}
	*binary = good
	if e = m.Stage(ctx, release); e != nil {
		t.Fatal(e)
	}
	if e = m.Install(release); e != nil {
		t.Fatal(e)
	}
	if !m.Status().Pending {
		t.Fatal("installation missing recovery journal")
	}
	if e = m.Recover(); e != nil {
		t.Fatal(e)
	}
	if m.Status().CurrentSHA != original || m.Status().HighWater != 1 {
		t.Fatal("recovery lost original or high water")
	}
	if r, e := m.Check(ctx, server.URL+"/manifest"); e != nil || r != nil {
		t.Fatal("failed release replayed")
	}
	*envelope, e = Sign(testManifest(server.URL, *binary, 2), key)
	if e != nil {
		t.Fatal(e)
	}
	release, e = m.Check(ctx, server.URL+"/manifest")
	if e != nil {
		t.Fatal(e)
	}
	if e = m.Stage(ctx, release); e != nil {
		t.Fatal(e)
	}
	if e = m.Install(release); e != nil {
		t.Fatal(e)
	}
	if e = m.Commit(); e != nil {
		t.Fatal(e)
	}
	if m.Status().CurrentSHA == original || m.Status().PreviousSHA != original {
		t.Fatal("healthy update did not retain rollback")
	}
	if e = m.Rollback(); e != nil {
		t.Fatal(e)
	}
	if m.Status().CurrentSHA != original || m.Status().HighWater != 2 {
		t.Fatal("rollback lost high water")
	}
}
func TestManagerLockTamperAndInterruptedInstall(t *testing.T) {
	m, server, _, _, _ := fixture(t)
	if second, e := Open(m.dir, "", ""); e == nil {
		second.Close()
		t.Fatal("second manager acquired lock")
	}
	release, e := m.Check(context.Background(), server.URL+"/manifest")
	if e != nil {
		t.Fatal(e)
	}
	if e = m.Stage(context.Background(), release); e != nil {
		t.Fatal(e)
	}
	if e = m.Install(release); e != nil {
		t.Fatal(e)
	}
	dir := m.dir
	original := m.Status().CurrentSHA
	m.Close()
	reopened, e := Open(dir, "", "")
	if e != nil {
		t.Fatal(e)
	}
	if reopened.Status().Pending || reopened.Status().CurrentSHA != original {
		t.Fatal("restart failed recovery")
	}
	reopened.Close()
	os.WriteFile(filepath.Join(dir, executableName()), []byte("tampered"), 0700)
	if reopened, e = Open(dir, "", ""); e == nil {
		reopened.Close()
		t.Fatal("modified current binary trusted")
	}
	if runtime.GOOS != "windows" {
		linkDir := filepath.Join(t.TempDir(), "linked")
		os.Symlink(dir, linkDir)
		if _, e = Open(linkDir, "", ""); e == nil {
			t.Fatal("symlink update directory accepted")
		}
	}
}
func TestManifestDoesNotFollowRedirect(t *testing.T) {
	m, _, _, _, _ := fixture(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "https://other.example.com", 302) }))
	defer server.Close()
	if _, e := m.Check(context.Background(), server.URL); e == nil || !strings.Contains(e.Error(), "302") {
		t.Fatal("redirect followed", e)
	}
}
