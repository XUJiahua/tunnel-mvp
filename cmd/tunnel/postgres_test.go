package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/XUJiahua/tunnel-mvp/internal/state"
	"github.com/XUJiahua/tunnel-mvp/internal/testpostgres"
	"golang.org/x/crypto/bcrypt"
)

func TestPostgresCLIWorkflow(t *testing.T) {
	dsn := testpostgres.Database(t)
	t.Setenv("TUNNEL_POSTGRES_DSN", dsn)
	source := t.TempDir()
	dir := t.TempDir()
	hash, e := bcrypt.GenerateFromPassword([]byte("cli-postgres-password"), bcrypt.MinCost)
	if e != nil {
		t.Fatal(e)
	}
	p, e := state.Open(source, state.SQLite, func() (state.State, error) {
		return state.State{Users: []state.User{{ID: "admin", Username: "admin", Role: "admin", PasswordHash: string(hash)}}}, nil
	})
	if e != nil {
		t.Fatal(e)
	}
	_ = p.Close()
	out, e := execute("storage", "migrate", "--data-dir", source, "--from", "sqlite", "--to", "postgres", "--target-data-dir", dir)
	if e != nil || !strings.Contains(out, `"storage": "postgres"`) {
		t.Fatal("migration CLI failed", out, e)
	}
	out, e = execute("storage", "inspect", "--storage", "postgres", "--data-dir", dir)
	if e != nil || !strings.Contains(out, `"users": 1`) || strings.Contains(out, "passwordHash") {
		t.Fatal("inspect CLI failed", out, e)
	}
	out, e = execute("server", "--storage", "postgres", "--check-config")
	if e != nil || strings.Contains(out, "tunnel-test-only") || strings.Contains(out, "postgres://") {
		t.Fatal("config exposed DSN", out, e)
	}
	backup := filepath.Join(t.TempDir(), "backup.json")
	if _, e = execute("storage", "export", "--storage", "postgres", "--data-dir", dir, "--output", backup); e != nil {
		t.Fatal(e)
	}
	t.Setenv("TUNNEL_RECOVERY_PASSWORD", "new-postgres-password")
	if _, e = execute("storage", "reset-admin-password", "--storage", "postgres", "--data-dir", dir, "--username", "admin"); e != nil {
		t.Fatal(e)
	}
	repo, e := state.OpenPostgres(context.Background(), dsn, nil)
	if e != nil {
		t.Fatal(e)
	}
	if e = bcrypt.CompareHashAndPassword([]byte(repo.Snapshot().Users[0].PasswordHash), []byte(os.Getenv("TUNNEL_RECOVERY_PASSWORD"))); e != nil {
		t.Fatal(e)
	}
	_ = repo.Close()
	if _, e = execute("storage", "recover-postgres-runtime", "--storage", "postgres", "--data-dir", dir); e == nil {
		t.Fatal("recovery flag not required")
	}
	if _, e = execute("storage", "recover-postgres-runtime", "--storage", "postgres", "--data-dir", dir, "--old-server-stopped"); e != nil {
		t.Fatal(e)
	}
	t.Setenv("TUNNEL_POSTGRES_DSN", testpostgres.Database(t))
	if _, e = execute("storage", "restore", "--storage", "postgres", "--data-dir", t.TempDir(), "--input", backup); e != nil {
		t.Fatal(e)
	}
}
