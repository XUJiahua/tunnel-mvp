package state

import (
	"context"
	"crypto/rand"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lib/pq"
)

var ErrPostgresBusy = errors.New("PostgreSQL database is in use")
var ErrPostgresRecovery = errors.New("previous control plane did not release PostgreSQL; stop the old Server and recover its runtime lease")
var ErrPostgresUnavailable = errors.New("PostgreSQL repository unavailable")
var ErrRevisionConflict = errors.New("configuration revision changed; rebuild the transaction from fresh state")

const pgOperationTimeout = 10 * time.Second

type RevisionedState struct {
	State      State
	InstanceID string
	Revision   uint64
}
type pgMetadata struct {
	InstanceID string
	Revision   uint64
	Owner      string
}

// PostgresRepository owns a pinned connection: its session lock can never be
// renewed on a replacement connection. There is no retained authorization cache.
type PostgresRepository struct {
	mu            sync.Mutex
	db            *sql.DB
	conn          *sql.Conn
	exclusive     bool
	runtime       bool
	owner         string
	pid           int
	closed        atomic.Bool
	lost          atomic.Bool
	failures      atomic.Uint64
	loss          chan struct{}
	lossOnce      sync.Once
	cancel        context.CancelFunc
	wg            sync.WaitGroup
	directoryLock io.Closer
}
type pgOpenOptions struct {
	exclusive, runtime, empty, recover bool
	initialize                         func() (State, error)
}

func OpenPostgres(ctx context.Context, dsn string, initialize func() (State, error)) (*PostgresRepository, error) {
	return openPostgres(ctx, dsn, pgOpenOptions{initialize: initialize})
}
func pgRandomID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
func pgSafeError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrRevisionConflict) || errors.Is(err, ErrPostgresBusy) || errors.Is(err, ErrPostgresRecovery) || errors.Is(err, ErrDirectoryChanged) || errors.Is(err, ErrDirectoryQuery) {
		return err
	}
	var pg *pq.Error
	if errors.As(err, &pg) {
		return fmt.Errorf("PostgreSQL operation failed (SQLSTATE %s)", pg.Code)
	}
	// Never expose a DSN, authentication material or offending row/payload.
	return ErrPostgresUnavailable
}
func openPostgres(ctx context.Context, dsn string, options pgOpenOptions) (*PostgresRepository, error) {
	if dsn == "" {
		return nil, fmt.Errorf("PostgreSQL requires TUNNEL_POSTGRES_DSN")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, pgSafeError(err)
	}
	db.SetMaxOpenConns(2)
	db.SetMaxIdleConns(1)
	p := &PostgresRepository{db: db, exclusive: options.exclusive, runtime: options.runtime, loss: make(chan struct{})}
	success := false
	defer func() {
		if !success {
			if p.conn != nil {
				_ = p.conn.Close()
			}
			_ = db.Close()
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, pgOperationTimeout)
	defer cancel()
	p.conn, err = db.Conn(ctx)
	if err != nil {
		return nil, pgSafeError(err)
	}
	lock := "pg_try_advisory_lock_shared"
	if p.exclusive {
		lock = "pg_try_advisory_lock"
	}
	var acquired bool
	if err = p.conn.QueryRowContext(ctx, "SELECT "+lock+"($1,$2),pg_backend_pid()", pgNamespace, pgRuntimeKey).Scan(&acquired, &p.pid); err != nil {
		return nil, pgSafeError(err)
	}
	if !acquired {
		return nil, ErrPostgresBusy
	}
	tx, err := p.conn.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, pgSafeError(err)
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1,$2)`, pgNamespace, pgSchemaKey); err != nil {
		return nil, pgSafeError(err)
	}
	var present bool
	if err = tx.QueryRowContext(ctx, `SELECT to_regclass('tunnel_state.meta') IS NOT NULL`).Scan(&present); err != nil {
		return nil, pgSafeError(err)
	}
	if !present {
		if options.initialize == nil || options.recover {
			return nil, fmt.Errorf("PostgreSQL is not initialized")
		}
		initial, initErr := options.initialize()
		if initErr != nil {
			return nil, initErr
		}
		if initErr = Validate(&initial); initErr != nil {
			return nil, initErr
		}
		if err = pgCreateSchema(ctx, tx); err != nil {
			return nil, pgSafeError(err)
		}
		// Refuse to adopt orphaned tables if metadata was externally removed.
		for _, kind := range pgEntityKinds {
			var count int
			if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM `+pgTable(kind)).Scan(&count); err != nil {
				return nil, pgSafeError(err)
			}
			if count != 0 {
				return nil, fmt.Errorf("PostgreSQL contains configuration without metadata")
			}
		}
		id, idErr := pgRandomID()
		if idErr != nil {
			return nil, idErr
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO tunnel_state.meta(singleton,schema_version,model_version,instance_id,revision) VALUES(true,$1,$2,$3,0)`, PostgresSchemaVersion, SchemaVersion, id); err != nil {
			return nil, pgSafeError(err)
		}
		if err = pgWriteState(ctx, tx, initial); err != nil {
			return nil, pgSafeError(err)
		}
	} else if options.empty {
		return nil, fmt.Errorf("PostgreSQL already initialized; refusing to overwrite")
	}
	meta, err := pgReadMeta(ctx, tx, " FOR SHARE")
	if err != nil {
		return nil, pgSafeError(err)
	}
	if meta.Owner != "" && !options.recover {
		return nil, ErrPostgresRecovery
	}
	if _, err = pgReadState(ctx, tx); err != nil {
		return nil, pgSafeError(err)
	}
	if options.runtime {
		p.owner, err = pgRandomID()
		if err != nil {
			return nil, err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE tunnel_state.meta SET runtime_owner=$1 WHERE singleton`, p.owner); err != nil {
			return nil, pgSafeError(err)
		}
	}
	if err = tx.Commit(); err != nil {
		return nil, pgSafeError(err)
	}
	probeCtx, probeCancel := context.WithCancel(context.Background())
	p.cancel = probeCancel
	p.wg.Add(1)
	go p.probe(probeCtx)
	success = true
	return p, nil
}
func pgReadMeta(ctx context.Context, tx *sql.Tx, lock string) (pgMetadata, error) {
	var m pgMetadata
	var version, model int
	var revision int64
	err := tx.QueryRowContext(ctx, `SELECT schema_version,model_version,instance_id,revision,runtime_owner FROM tunnel_state.meta WHERE singleton`+lock).Scan(&version, &model, &m.InstanceID, &revision, &m.Owner)
	if err != nil {
		return m, err
	}
	if version != PostgresSchemaVersion || model != SchemaVersion || revision < 0 || len(m.InstanceID) != 32 {
		return m, fmt.Errorf("unsupported PostgreSQL schema")
	}
	m.Revision = uint64(revision)
	return m, nil
}
func (p *PostgresRepository) Failure() <-chan struct{}  { return p.loss }
func (p *PostgresRepository) QueryReadFailures() uint64 { return p.failures.Load() }
func (p *PostgresRepository) fence() {
	if !p.closed.Load() {
		p.lost.Store(true)
		p.lossOnce.Do(func() { close(p.loss) })
	}
}
func (p *PostgresRepository) probe(ctx context.Context) {
	defer p.wg.Done()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	mode := "ShareLock"
	if p.exclusive {
		mode = "ExclusiveLock"
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		var held bool
		err := p.db.QueryRowContext(checkCtx, `SELECT EXISTS(SELECT 1 FROM pg_catalog.pg_locks WHERE locktype='advisory' AND pid=$1 AND classid=$2 AND objid=$3 AND objsubid=2 AND mode=$4 AND granted AND database=(SELECT oid FROM pg_catalog.pg_database WHERE datname=current_database()))`, p.pid, pgNamespace, pgRuntimeKey, mode).Scan(&held)
		cancel()
		if ctx.Err() != nil {
			return
		}
		if err != nil || !held {
			p.fence()
			return
		}
	}
}
func (p *PostgresRepository) check() error {
	if p.closed.Load() || p.lost.Load() {
		return ErrPostgresUnavailable
	}
	return nil
}
func (p *PostgresRepository) failed(err error, read bool) error {
	if err == nil {
		return nil
	}
	if read && !errors.Is(err, ErrDirectoryChanged) && !errors.Is(err, ErrDirectoryQuery) {
		p.failures.Add(1)
	}
	var network net.Error
	if errors.Is(err, sql.ErrConnDone) || errors.Is(err, driver.ErrBadConn) || errors.Is(err, io.EOF) || errors.As(err, &network) {
		p.fence()
	}
	return pgSafeError(err)
}
func (p *PostgresRepository) read(ctx context.Context, fn func(context.Context, *sql.Tx, pgMetadata) error) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.check(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, pgOperationTimeout)
	defer cancel()
	tx, err := p.conn.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return p.failed(err, true)
	}
	defer tx.Rollback()
	meta, err := pgReadMeta(ctx, tx, "")
	if err == nil && p.runtime && meta.Owner != p.owner {
		p.fence()
		err = ErrPostgresUnavailable
	}
	if err == nil {
		err = fn(ctx, tx, meta)
	}
	if err == nil {
		err = p.check()
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		return p.failed(err, true)
	}
	return nil
}
func (p *PostgresRepository) SnapshotContext(ctx context.Context) (RevisionedState, error) {
	var out RevisionedState
	err := p.read(ctx, func(ctx context.Context, tx *sql.Tx, meta pgMetadata) error {
		st, err := pgReadState(ctx, tx)
		if err != nil {
			return err
		}
		out = RevisionedState{st, meta.InstanceID, meta.Revision}
		return nil
	})
	if err != nil {
		return RevisionedState{}, err
	}
	return out, nil
}

// Snapshot preserves the legacy interface while failing closed. Error-aware
// maintenance and new callers use SnapshotContext; no cached state is returned.
func (p *PostgresRepository) Snapshot() State {
	st, err := p.SnapshotContext(context.Background())
	if err != nil {
		return State{}
	}
	return st.State
}
func (p *PostgresRepository) Update(fn func(*State) error) error {
	return p.transact(context.Background(), nil, fn)
}
func (p *PostgresRepository) UpdateAtRevision(ctx context.Context, revision uint64, fn func(*State) error) error {
	return p.transact(ctx, &revision, fn)
}
func (p *PostgresRepository) transact(ctx context.Context, expected *uint64, fn func(*State) error) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.check(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, pgOperationTimeout)
	defer cancel()
	tx, err := p.conn.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return p.failed(err, false)
	}
	defer tx.Rollback()
	// This row is locked BEFORE reading configuration or invoking the callback.
	// Waiting writers read the latest commit under READ COMMITTED. A callback runs
	// once, avoiding unsafe automatic replay of captured response variables.
	meta, err := pgReadMeta(ctx, tx, " FOR UPDATE")
	if err != nil {
		return p.failed(err, false)
	}
	if p.runtime && meta.Owner != p.owner {
		p.fence()
		return ErrPostgresUnavailable
	}
	if expected != nil && *expected != meta.Revision {
		return ErrRevisionConflict
	}
	st, err := pgReadState(ctx, tx)
	if err != nil {
		return p.failed(err, false)
	}
	if err = fn(&st); err != nil {
		return err
	}
	if err = Validate(&st); err != nil {
		return err
	}
	if err = p.check(); err != nil {
		return err
	}
	if err = pgWriteState(ctx, tx, st); err != nil {
		return p.failed(err, false)
	}
	if _, err = tx.ExecContext(ctx, `UPDATE tunnel_state.meta SET revision=revision+1 WHERE singleton`); err != nil {
		return p.failed(err, false)
	}
	if err = p.check(); err != nil {
		return err
	}
	return p.failed(tx.Commit(), false)
}

// RetryUpdate explicitly rebuilds a pure mutation from a fresh snapshot. Actor
// permissions and intent must be checked inside that mutation's locked State.
// Opaque legacy Update callbacks are never automatically replayed.
func (p *PostgresRepository) RetryUpdate(ctx context.Context, attempts int, build func(RevisionedState) (func(*State) error, error)) error {
	if attempts < 1 || attempts > 8 {
		return fmt.Errorf("retry attempts must be 1–8")
	}
	for i := 0; i < attempts; i++ {
		snapshot, err := p.SnapshotContext(ctx)
		if err != nil {
			return err
		}
		fn, err := build(snapshot)
		if err != nil {
			return err
		}
		err = p.UpdateAtRevision(ctx, snapshot.Revision, fn)
		if !errors.Is(err, ErrRevisionConflict) {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	return ErrRevisionConflict
}
func (p *PostgresRepository) Close() error {
	if !p.closed.CompareAndSwap(false, true) {
		return nil
	}
	p.cancel()
	p.wg.Wait()
	p.mu.Lock()
	defer p.mu.Unlock()
	var closeErr error
	if p.runtime {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		// The control plane cancels all flows before calling Close. A broken pinned
		// connection cannot clear this durable latch: offline recovery is required.
		_, closeErr = p.conn.ExecContext(ctx, `UPDATE tunnel_state.meta SET runtime_owner='' WHERE singleton AND runtime_owner=$1`, p.owner)
		cancel()
	}
	unlock := "pg_advisory_unlock_shared"
	if p.exclusive {
		unlock = "pg_advisory_unlock"
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	_, _ = p.conn.ExecContext(ctx, "SELECT "+unlock+"($1,$2)", pgNamespace, pgRuntimeKey)
	cancel()
	_ = p.conn.Close()
	if err := p.db.Close(); closeErr == nil {
		closeErr = err
	}
	if p.directoryLock != nil {
		if err := p.directoryLock.Close(); closeErr == nil {
			closeErr = err
		}
	}
	return pgSafeError(closeErr)
}
