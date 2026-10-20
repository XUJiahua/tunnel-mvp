package state

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOperationsArchiveRoundTripAndResume(t *testing.T) {
	dir := t.TempDir()
	audit := []byte("{\"event\":\"example\",\"organizationID\":\"org-a\"}\n")
	for _, name := range []string{"audit.jsonl", "audit.jsonl.1"} {
		if err := os.WriteFile(filepath.Join(dir, name), audit, 0600); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(dir, "state.json"), []byte("credential-must-not-be-included"), 0600)
	archive := filepath.Join(t.TempDir(), "operations.tar.gz")
	summary, err := ExportOperations(dir, archive)
	if err != nil || summary.Files != 2 {
		t.Fatal(summary, err)
	}
	if _, err = ExportOperations(dir, archive); err == nil {
		t.Fatal("archive overwritten")
	}
	if info, _ := os.Stat(archive); info.Mode().Perm() != 0600 {
		t.Fatal("archive is not private")
	}
	if validated, err := ValidateOperations(archive); err != nil || validated != summary {
		t.Fatal("archive failed validation", err)
	}
	m, stage, err := readOperations(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(stage)
	dest := t.TempDir()
	if _, err = RestoreOperations(dest, archive); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"audit.jsonl", "audit.jsonl.1"} {
		got, _ := os.ReadFile(filepath.Join(dest, name))
		if !bytes.Equal(got, audit) {
			t.Fatal("audit bytes changed")
		}
	}
	if _, err = os.Stat(filepath.Join(dest, "state.json")); !os.IsNotExist(err) {
		t.Fatal("state included in operational backup")
	}
	if _, err = RestoreOperations(dest, archive); err == nil {
		t.Fatal("existing operational files overwritten")
	}
	// Simulate a crash after the first file was published. The pending marker
	// blocks startup and only the exact same manifest can resume it.
	resumed := t.TempDir()
	raw, _ := json.Marshal(m)
	os.WriteFile(filepath.Join(resumed, operationsPending), raw, 0600)
	os.WriteFile(filepath.Join(resumed, m.Files[0].Name), audit, 0600)
	if CheckOperationsRestore(resumed) == nil {
		t.Fatal("pending restore allowed startup")
	}
	if _, err = RestoreOperations(resumed, archive); err != nil {
		t.Fatal("resume failed", err)
	}
	if err = CheckOperationsRestore(resumed); err != nil {
		t.Fatal(err)
	}
	bad := t.TempDir()
	os.WriteFile(filepath.Join(bad, operationsPending), raw, 0600)
	os.WriteFile(filepath.Join(bad, m.Files[0].Name), []byte("unrelated data"), 0600)
	if _, err = RestoreOperations(bad, archive); err == nil {
		t.Fatal("resume overwrote mismatched existing file")
	}
}
func TestOperationsArchiveRejectsTraversalCorruptionAndSymlinks(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "audit.jsonl"), []byte("{\"event\":\"ok\"}\n"), 0600)
	archive := filepath.Join(t.TempDir(), "good.tar.gz")
	if _, err := ExportOperations(dir, archive); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(archive)
	truncated := filepath.Join(t.TempDir(), "truncated.tar.gz")
	os.WriteFile(truncated, raw[:len(raw)-8], 0600)
	if _, err := ValidateOperations(truncated); err == nil {
		t.Fatal("truncated gzip footer accepted")
	}
	malicious := filepath.Join(t.TempDir(), "traversal.tar.gz")
	f, _ := os.Create(malicious)
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	tw.WriteHeader(&tar.Header{Name: "../outside", Mode: 0600, Size: 1})
	tw.Write([]byte("x"))
	tw.Close()
	gz.Close()
	f.Close()
	target := filepath.Join(t.TempDir(), "untouched")
	if _, err := RestoreOperations(target, malicious); err == nil {
		t.Fatal("traversal accepted")
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatal("invalid archive created target directory")
	}
	linked := t.TempDir()
	if err := os.Symlink(filepath.Join(dir, "audit.jsonl"), filepath.Join(linked, "audit.jsonl")); err == nil {
		if _, err = ExportOperations(linked, filepath.Join(t.TempDir(), "linked.tar.gz")); err == nil {
			t.Fatal("symlink archived")
		}
	}
	os.WriteFile(filepath.Join(dir, "monitor.db-wal"), []byte("stale"), 0600)
	if _, err := ExportOperations(dir, filepath.Join(t.TempDir(), "sidecar.tar.gz")); err == nil {
		t.Fatal("database sidecar ignored")
	}
}
func TestOperationsArchiveRespectsLiveDirectoryLock(t *testing.T) {
	dir := t.TempDir()
	r := mustOpen(t, dir, JSON, sample(t))
	os.WriteFile(filepath.Join(dir, "audit.jsonl"), []byte("{}\n"), 0600)
	_, err := ExportOperations(dir, filepath.Join(t.TempDir(), "locked.tar.gz"))
	if err == nil || !strings.Contains(err.Error(), "in use") {
		t.Fatal("live operations archived", err)
	}
	r.Close()
}
