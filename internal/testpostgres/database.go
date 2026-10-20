// Package testpostgres allocates disposable databases on an explicitly supplied
// test cluster. It never drops schemas or data in the database named by the DSN.
package testpostgres

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/lib/pq"
)

func Database(t *testing.T) string {
	t.Helper()
	raw := os.Getenv("TUNNEL_TEST_POSTGRES_DSN")
	if raw == "" {
		if os.Getenv("TUNNEL_REQUIRE_POSTGRES_TESTS") == "1" {
			t.Fatal("PostgreSQL tests required but test DSN missing")
		}
		t.Skip("requires TUNNEL_TEST_POSTGRES_DSN")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		t.Fatal("test DSN must be a PostgreSQL URL")
	}
	var random [12]byte
	if _, err = rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	name := "tunnel_test_" + hex.EncodeToString(random[:])
	admin, err := sql.Open("postgres", raw)
	if err != nil {
		t.Fatal("open test cluster failed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err = admin.ExecContext(ctx, "CREATE DATABASE "+pq.QuoteIdentifier(name)); err != nil {
		_ = admin.Close()
		t.Fatal("create disposable test database failed")
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, e := admin.ExecContext(ctx, "DROP DATABASE "+pq.QuoteIdentifier(name)+" WITH (FORCE)"); e != nil {
			t.Error("drop disposable test database failed")
		}
		_ = admin.Close()
	})
	u.Path = "/" + name
	u.RawPath = ""
	return u.String()
}
