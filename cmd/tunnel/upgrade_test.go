package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/XUJiahua/tunnel-mvp/internal/upgrade"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestUpdateKeyAndManifestCommands(t *testing.T) {
	dir := t.TempDir()
	private, public := filepath.Join(dir, "release.key"), filepath.Join(dir, "release.pub")
	run := func(args ...string) error {
		cmd := newRootCommand()
		cmd.SetOut(new(bytes.Buffer))
		cmd.SetErr(new(bytes.Buffer))
		cmd.SetArgs(args)
		return cmd.Execute()
	}
	args := []string{"generate-update-key", "--private-output", private, "--public-output", public}
	if e := run(args...); e != nil {
		t.Fatal(e)
	}
	if e := run(args...); e == nil {
		t.Fatal("key overwrite allowed")
	}
	h := sha256.Sum256([]byte("candidate"))
	m := upgrade.Manifest{Format: 1, Sequence: 1, Version: "v2", ExpiresAt: time.Now().Add(time.Hour), Artifacts: []upgrade.Artifact{{Platform: runtime.GOOS + "/" + runtime.GOARCH, URL: "https://updates.example.com/binary", SHA256: fmt.Sprintf("%x", h), Size: 9}}}
	raw, _ := json.Marshal(m)
	input, output := filepath.Join(dir, "manifest.json"), filepath.Join(dir, "signed.json")
	os.WriteFile(input, raw, 0600)
	signArgs := []string{"sign-update-manifest", "--input", input, "--private-key", private, "--output", output}
	if e := run(signArgs...); e != nil {
		t.Fatal(e)
	}
	signed, _ := os.ReadFile(output)
	pubRaw, _ := os.ReadFile(public)
	pub, _ := base64.StdEncoding.DecodeString(string(bytes.TrimSpace(pubRaw)))
	if _, e := upgrade.Verify(signed, ed25519.PublicKey(pub), time.Now()); e != nil {
		t.Fatal(e)
	}
	if e := run(signArgs...); e == nil {
		t.Fatal("manifest overwrite allowed")
	}
}
