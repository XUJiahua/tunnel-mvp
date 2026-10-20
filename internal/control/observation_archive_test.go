package control

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/XUJiahua/tunnel-mvp/internal/state"
)

func TestObservationJournalOperationsArchiveRoundTrip(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	h, err := openHistory(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err = h.enableObservations("node-a", 16<<20); err != nil {
		h.close()
		t.Fatal(err)
	}
	raw := journalAudit(t, h, "archive_pending")
	if err = os.WriteFile(filepath.Join(dir, "audit.jsonl"), append(raw, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	if err = h.indexAudit(raw); err != nil {
		t.Fatal(err)
	}
	svc := Service{ID: "app", SiteID: "site", URL: "http://127.0.0.1"}
	if err = h.save(svc, probeReport{CheckedAt: time.Now().UTC(), Reachable: true, Healthy: true, Status: 200}); err != nil {
		t.Fatal(err)
	}
	before, err := h.pendingObservations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	source := h.observer.Source
	h.close()
	archive := filepath.Join(t.TempDir(), "operations.tar.gz")
	if _, err = state.ExportOperations(dir, archive); err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	if _, err = state.RestoreOperations(dest, archive); err != nil {
		t.Fatal(err)
	}
	restored, err := openHistory(dest)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.close()
	// Restored pending facts must survive before a publisher can run. Startup
	// replay may add a new legacy completion marker but preserves original IDs.
	if err = restored.enableObservations("node-a", 16<<20); err != nil {
		t.Fatal(err)
	}
	after, err := restored.pendingObservations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, v := range after {
		ids[v.Record.ID] = true
	}
	for _, v := range before {
		if !ids[v.Record.ID] {
			t.Fatal("restore lost unacknowledged fact", v.Record.ID)
		}
	}
	if restored.observer.Source != source {
		t.Fatal("restore changed source")
	}
	if _, err = restored.db.Exec(`UPDATE observation_local SET buffered_bytes=buffered_bytes+1`); err != nil {
		t.Fatal(err)
	}
	restored.close()
	if _, err = state.ExportOperations(dest, filepath.Join(t.TempDir(), "invalid.tar.gz")); err == nil {
		t.Fatal("archive accepted corrupted queue byte counter")
	}
}

// Uses an actual pre-extension executable, never a simulated old schema reader.
func TestObservationJournalHA3ArchiveCompatibility(t *testing.T) {
	old := os.Getenv("TUNNEL_TEST_HA3_BINARY")
	if old == "" {
		if os.Getenv("TUNNEL_REQUIRE_HA3_TESTS") == "1" {
			t.Fatal("HA-3 binary required")
		}
		t.Skip("requires TUNNEL_TEST_HA3_BINARY")
	}
	dir := t.TempDir()
	h, err := openHistory(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err = h.enableObservations("node-a", 16<<20); err != nil {
		t.Fatal(err)
	}
	raw := journalAudit(t, h, "ha3_archive")
	if err = os.WriteFile(filepath.Join(dir, "audit.jsonl"), append(raw, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	if err = h.indexAudit(raw); err != nil {
		t.Fatal(err)
	}
	h.close()
	archive := filepath.Join(t.TempDir(), "old-export.tar.gz")
	for _, args := range [][]string{{"storage", "export-operations", "--data-dir", dir, "--output", archive}, {"storage", "validate-operations", "--input", archive}} {
		cmd := exec.Command(old, args...)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("actual HA-3 archive command %v: %v %s", args, err, output)
		}
	}
	if _, err = state.ValidateOperations(archive); err != nil {
		t.Fatal("old binary corrupted extension archive", err)
	}
	// Exercise the actual old Server's monitor reader/writer after rollback too.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	public := "http://" + address
	cfg, _ := json.Marshal(map[string]any{"listen": address, "publicURL": public, "accessURL": "http://apps.localhost:39999", "dataDir": dir, "storage": "json"})
	configPath := filepath.Join(t.TempDir(), "server.json")
	if err = os.WriteFile(configPath, cfg, 0600); err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(runCtx, old, "server", "-c", configPath)
	cmd.Env = append(os.Environ(), "TUNNEL_STORAGE=json", "TUNNEL_ADMIN_PASSWORD="+testPassword)
	log, err := os.Create(filepath.Join(t.TempDir(), "old-server.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	cmd.Stdout = log
	cmd.Stderr = log
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("old Server did not stop")
		}
	}()
	client := &http.Client{Timeout: time.Second}
	waitFor(t, func() bool {
		res, e := client.Get(public + "/healthz")
		if e != nil {
			return false
		}
		res.Body.Close()
		return res.StatusCode == 200
	})
	payload, _ := json.Marshal(map[string]string{"username": "admin", "password": testPassword})
	req, err := http.NewRequest("POST", public+"/api/login", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", public)
	req.Header.Set("Content-Type", "application/json")
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatal("old Server could not use extended monitor", res.StatusCode)
	}
	// Stop cleanly so the old runtime has flushed a legacy audit record.
	if err = cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-done:
		done <- err
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("old Server shutdown timeout")
	}
	upgraded, err := openHistory(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.close()
	if err = upgraded.enableObservations("node-a", 16<<20); err != nil {
		t.Fatal("re-upgrade after old writer", err)
	}
	pending, err := upgraded.pendingObservations(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	legacy := false
	for _, entry := range pending {
		if entry.Record.Kind == "audit" && entry.Record.LegacySnapshot != "" {
			legacy = true
		}
	}
	if !legacy {
		t.Fatal("old Server rollback audit not captured on re-upgrade")
	}

}
