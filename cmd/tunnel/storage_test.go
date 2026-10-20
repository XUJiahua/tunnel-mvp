package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/XUJiahua/tunnel-mvp/internal/state"
	"golang.org/x/crypto/bcrypt"
)

func TestStorageCLIWorkflowAndConfiguration(t *testing.T) {
	dir := t.TempDir()
	hash, err := bcrypt.GenerateFromPassword([]byte("cli-storage-password"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(dir, state.JSON, func() (state.State, error) {
		return state.State{Users: []state.User{{ID: "admin", Username: "admin", Role: "admin", PasswordHash: string(hash)}}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err = execute("storage", "inspect", "--data-dir", dir); err == nil || !strings.Contains(err.Error(), "in use") {
		t.Fatal("CLI did not reject active directory", err)
	}
	_ = store.Close()
	configPath := filepath.Join(t.TempDir(), "server.json")
	raw, _ := json.Marshal(map[string]any{"dataDir": dir, "storage": "json"})
	if err = os.WriteFile(configPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	out, err := execute("storage", "inspect", "-c", configPath)
	if err != nil || !strings.Contains(out, `"users": 1`) {
		t.Fatalf("inspect: %s %v", out, err)
	}
	if strings.Contains(out, string(hash)) || strings.Contains(out, "passwordHash") {
		t.Fatal("CLI summary leaked credentials")
	}
	out, err = execute("storage", "migrate", "-c", configPath)
	if err != nil || !strings.Contains(out, `"storage": "sqlite"`) {
		t.Fatalf("migrate: %s %v", out, err)
	}
	backup := filepath.Join(t.TempDir(), "backup.json")
	out, err = execute("storage", "export", "-c", configPath, "--storage", "sqlite", "--output", backup)
	if err != nil {
		t.Fatalf("export: %s %v", out, err)
	}
	if _, err = execute("storage", "validate", "--input", backup); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "restored")
	if _, err = execute("storage", "restore", "--storage", "sqlite", "--data-dir", target, "--input", backup); err != nil {
		t.Fatal(err)
	}
	if _, err = state.Inspect(target, state.SQLite); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"storage", "export"}, {"storage", "restore"}, {"storage", "validate"}, {"storage", "migrate", "--from", "sqlite", "--to", "json"}, {"storage", "inspect", "--storage", "postgres"}, {"storage", "migrate", "-c", configPath}} {
		if _, err := execute(args...); err == nil {
			t.Fatalf("accepted invalid operation: %v", args)
		}
	}
	t.Setenv("TUNNEL_STORAGE", "sqlite")
	out, err = execute("server", "--data-dir", target, "--check-config")
	if err != nil || !strings.Contains(out, `"storage": "sqlite"`) {
		t.Fatalf("storage environment ignored: %s %v", out, err)
	}
	out, err = execute("server", "--storage", "json", "--check-config")
	if err != nil || !strings.Contains(out, `"storage": "json"`) {
		t.Fatalf("explicit storage flag not applied: %s %v", out, err)
	}
}
