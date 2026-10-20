package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const postgresBinding = "state.postgres"

func rejectPostgresBinding(dir string) error {
	if found, err := exists(filepath.Join(dir, postgresBinding)); err != nil {
		return err
	} else if found {
		return fmt.Errorf("directory belongs to PostgreSQL; use a fresh directory for a local backend")
	}
	return nil
}
func postgresDirectory(dir string, fresh bool) (io.Closer, error) {
	lock, err := lockDir(dir)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (io.Closer, error) { _ = lock.Close(); return nil, err }
	if err = CheckOperationsRestore(dir); err != nil {
		return fail(err)
	}
	for _, name := range []string{"state.json", "state.db"} {
		if found, e := exists(filepath.Join(dir, name)); e != nil {
			return fail(e)
		} else if found {
			return fail(fmt.Errorf("%s exists; PostgreSQL requires a separate runtime directory", name))
		}
	}
	if err = rejectSidecars(dir); err != nil {
		return fail(err)
	}
	if fresh {
		if err = rejectPostgresBinding(dir); err != nil {
			return fail(err)
		}
	}
	return lock, nil
}
func bindPostgres(ctx context.Context, dir string, p *PostgresRepository) error {
	var id string
	if err := p.read(ctx, func(_ context.Context, _ *sql.Tx, m pgMetadata) error { id = m.InstanceID; return nil }); err != nil {
		return err
	}
	path := filepath.Join(dir, postgresBinding)
	raw, err := os.ReadFile(path)
	if err == nil {
		if strings.TrimSpace(string(raw)) != id {
			return fmt.Errorf("runtime directory belongs to another PostgreSQL database")
		}
		return nil
	}
	if !os.IsNotExist(err) {
		return err
	}
	temp, err := writeTemp(dir, []byte(id+"\n"))
	if err != nil {
		return err
	}
	defer os.Remove(temp)
	return linkNew(temp, path)
}

// OpenPostgresRuntime admits exactly one serving control plane. Shared repository
// clients are for storage transactions, not additional serving Servers (HA-1).
func OpenPostgresRuntime(ctx context.Context, dir, dsn string, initialize func() (State, error)) (*PostgresRepository, error) {
	lock, err := postgresDirectory(dir, false)
	if err != nil {
		return nil, err
	}
	p, err := openPostgres(ctx, dsn, pgOpenOptions{exclusive: true, runtime: true, initialize: initialize})
	if err != nil {
		_ = lock.Close()
		return nil, err
	}
	if err = bindPostgres(ctx, dir, p); err != nil {
		_ = p.Close()
		_ = lock.Close()
		return nil, err
	}
	p.directoryLock = lock
	return p, nil
}
func postgresMaintenance(ctx context.Context, dir, dsn string, opts pgOpenOptions) (*PostgresRepository, error) {
	lock, err := postgresDirectory(dir, opts.empty)
	if err != nil {
		return nil, err
	}
	opts.exclusive = true
	p, err := openPostgres(ctx, dsn, opts)
	if err != nil {
		_ = lock.Close()
		return nil, err
	}
	if err = bindPostgres(ctx, dir, p); err != nil {
		_ = p.Close()
		_ = lock.Close()
		return nil, err
	}
	p.directoryLock = lock
	return p, nil
}
func InspectPostgres(ctx context.Context, dir, dsn string) (Summary, error) {
	p, err := postgresMaintenance(ctx, dir, dsn, pgOpenOptions{})
	if err != nil {
		return Summary{}, err
	}
	defer p.Close()
	s, err := p.SnapshotContext(ctx)
	if err != nil {
		return Summary{}, err
	}
	return summarize(Postgres, s.State), nil
}
func ExportPostgres(ctx context.Context, dir, dsn, output string) (Summary, error) {
	p, err := postgresMaintenance(ctx, dir, dsn, pgOpenOptions{})
	if err != nil {
		return Summary{}, err
	}
	defer p.Close()
	s, err := p.SnapshotContext(ctx)
	if err != nil {
		return Summary{}, err
	}
	raw, err := json.MarshalIndent(Backup{"tunnel-state", SchemaVersion, time.Now().UTC(), checksum(s.State), s.State}, "", "  ")
	if err != nil {
		return Summary{}, err
	}
	temp, err := writeTemp(filepath.Dir(output), raw)
	if err != nil {
		return Summary{}, err
	}
	defer os.Remove(temp)
	if err = linkNew(temp, output); err != nil {
		return Summary{}, err
	}
	return summarize(Postgres, s.State), nil
}
func RestorePostgres(ctx context.Context, dir, dsn, input string) (Summary, error) {
	s, err := loadBackup(input)
	if err != nil {
		return Summary{}, err
	}
	s.Invites = nil
	return importPostgres(ctx, dir, dsn, s)
}
func importPostgres(ctx context.Context, dir, dsn string, s State) (Summary, error) {
	p, err := postgresMaintenance(ctx, dir, dsn, pgOpenOptions{empty: true, initialize: func() (State, error) { return s, nil }})
	if err != nil {
		return Summary{}, err
	}
	if err = p.Close(); err != nil {
		return Summary{}, err
	}
	return summarize(Postgres, s), nil
}

// MigratePostgres copies an offline source. SQLite schema upgrades and derived
// index rebuilds operate only on the copy, preserving the source byte for byte.
func MigratePostgres(ctx context.Context, sourceDir, kind, targetDir, dsn string) (Summary, error) {
	if kind != JSON && kind != SQLite {
		return Summary{}, fmt.Errorf("PostgreSQL migration source must be json or sqlite")
	}
	sourceAbs, err := filepath.Abs(sourceDir)
	if err != nil {
		return Summary{}, err
	}
	targetAbs, err := filepath.Abs(targetDir)
	if err != nil {
		return Summary{}, err
	}
	if sourceAbs == targetAbs {
		return Summary{}, fmt.Errorf("PostgreSQL migration requires a separate --target-data-dir")
	}
	lock, err := lockExistingDir(sourceDir)
	if err != nil {
		return Summary{}, err
	}
	defer lock.Close()
	if err = rejectPostgresBinding(sourceDir); err != nil {
		return Summary{}, err
	}
	if err = CheckOperationsRestore(sourceDir); err != nil {
		return Summary{}, err
	}
	var s State
	if kind == JSON {
		s, err = readLegacy(filepath.Join(sourceDir, "state.json"))
	} else {
		if err = rejectSidecars(sourceDir); err != nil {
			return Summary{}, err
		}
		tempDir, e := os.MkdirTemp("", "tunnel-postgres-migrate-")
		if e != nil {
			return Summary{}, e
		}
		defer os.RemoveAll(tempDir)
		copyPath := filepath.Join(tempDir, "state.db")
		if err = copyPostgresSource(filepath.Join(sourceDir, "state.db"), copyPath); err != nil {
			return Summary{}, err
		}
		var b backend
		b, s, err = readBackend(copyPath, SQLite)
		if b != nil {
			closeErr := b.close()
			if err == nil {
				err = closeErr
			}
		}
	}
	if err != nil {
		return Summary{}, err
	}
	return importPostgres(ctx, targetDir, dsn, s)
}
func ResetPostgresAdminPassword(ctx context.Context, dir, dsn, username, password string) (Summary, error) {
	if username == "" || len(password) < 12 || len(password) > 72 {
		return Summary{}, fmt.Errorf("existing administrator and recovery password (12–72 bytes) required")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return Summary{}, err
	}
	p, err := postgresMaintenance(ctx, dir, dsn, pgOpenOptions{})
	if err != nil {
		return Summary{}, err
	}
	defer p.Close()
	err = p.Update(func(s *State) error {
		for i := range s.Users {
			u := &s.Users[i]
			if u.Username == username && u.Role == "admin" {
				u.PasswordHash = string(hash)
				u.Disabled = false
				return nil
			}
		}
		return fmt.Errorf("existing administrator not found")
	})
	if err != nil {
		return Summary{}, err
	}
	s, err := p.SnapshotContext(ctx)
	if err != nil {
		return Summary{}, err
	}
	return summarize(Postgres, s.State), nil
}

// RecoverPostgresRuntime never guesses whether an old Server has stopped. The
// caller must fence it externally before explicitly acknowledging this command.
func RecoverPostgresRuntime(ctx context.Context, dir, dsn string, oldServerStopped bool) (Summary, error) {
	if !oldServerStopped {
		return Summary{}, fmt.Errorf("stop or externally fence the old Server, then pass --old-server-stopped")
	}
	ctx, cancel := context.WithTimeout(ctx, pgOperationTimeout)
	defer cancel()
	p, err := postgresMaintenance(ctx, dir, dsn, pgOpenOptions{recover: true})
	if err != nil {
		return Summary{}, err
	}
	defer p.Close()
	p.mu.Lock()
	_, err = p.conn.ExecContext(ctx, `UPDATE tunnel_state.meta SET runtime_owner='',revision=revision+1 WHERE singleton`)
	p.mu.Unlock()
	if err != nil {
		return Summary{}, p.failed(err, false)
	}
	s, err := p.SnapshotContext(ctx)
	if err != nil {
		return Summary{}, err
	}
	return summarize(Postgres, s.State), nil
}

// Copy database pages with bounded memory; schema repair only touches this copy.
func copyPostgresSource(source, target string) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}
