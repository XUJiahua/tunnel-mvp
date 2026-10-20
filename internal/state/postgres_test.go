package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/XUJiahua/tunnel-mvp/internal/testpostgres"
)

func mustPostgres(t *testing.T, dsn string, initial State) *PostgresRepository {
	t.Helper()
	p, err := OpenPostgres(context.Background(), dsn, func() (State, error) { return initial, nil })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}
func snapshotPostgres(t *testing.T, p *PostgresRepository) RevisionedState {
	t.Helper()
	s, err := p.SnapshotContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func TestPostgresConcurrentTransactions(t *testing.T) {
	dsn := testpostgres.Database(t)
	initial := organizationSample(t)
	p := mustPostgres(t, dsn, initial)
	q := mustPostgres(t, dsn, State{})
	if p.pid == q.pid {
		t.Fatal("clients share a connection")
	}
	if !reflect.DeepEqual(snapshotPostgres(t, p).State, initial) {
		t.Fatal("round trip changed state")
	}
	snap := snapshotPostgres(t, p)
	snap.State.Users[1].Username = "alias"
	if snapshotPostgres(t, q).State.Users[1].Username == "alias" {
		t.Fatal("snapshot aliases committed state")
	}
	const n = 12
	var callbacks atomic.Int32
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			client := p
			if i%2 != 0 {
				client = q
			}
			errs <- client.Update(func(s *State) error {
				callbacks.Add(1)
				s.Sites = append(s.Sites, Site{ID: fmt.Sprintf("new-%d", i), Name: "concurrent", OrganizationID: "org-a", TokenHash: fmt.Sprintf("%064x", i+100), CreatedAt: time.Now().UTC()})
				return nil
			})
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	after := snapshotPostgres(t, p)
	if len(after.State.Sites) != n+1 || after.Revision != n || callbacks.Load() != n {
		t.Fatalf("lost update or replay: sites=%d revision=%d callbacks=%d", len(after.State.Sites), after.Revision, callbacks.Load())
	}
	called := false
	err := q.UpdateAtRevision(context.Background(), 0, func(*State) error { called = true; return nil })
	if !errors.Is(err, ErrRevisionConflict) || called {
		t.Fatal("stale revision invoked mutation", err)
	}
	builds := 0
	err = p.RetryUpdate(context.Background(), 2, func(RevisionedState) (func(*State) error, error) {
		builds++
		if builds == 1 {
			if e := q.Update(func(s *State) error { s.Sites[0].Name = "competing"; return nil }); e != nil {
				return nil, e
			}
		}
		return func(s *State) error { s.Sites[0].Name = "rebuilt"; return nil }, nil
	})
	if err != nil || builds != 2 || snapshotPostgres(t, q).State.Sites[0].Name != "rebuilt" {
		t.Fatal("explicit retry did not rebuild intent", err, builds)
	}
	before := snapshotPostgres(t, p)
	rejected := errors.New("reject")
	if err = p.Update(func(s *State) error { s.Users[0].Disabled = true; return rejected }); !errors.Is(err, rejected) {
		t.Fatal(err)
	}
	if err = p.Update(func(s *State) error { s.Users[0].Disabled = true; return nil }); err == nil {
		t.Fatal("last administrator disabled")
	}
	if !reflect.DeepEqual(before, snapshotPostgres(t, q)) {
		t.Fatal("rejection advanced revision/state")
	}
	// Atomic unique-key swaps work because uniqueness remains deferred to commit.
	if err = p.Update(func(s *State) error {
		s.Users[0].Username, s.Users[1].Username = s.Users[1].Username, s.Users[0].Username
		s.Services[0].CustomDomain = "one.example.org"
		s.Services[1].CustomDomain = "two.example.org"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err = q.Update(func(s *State) error {
		s.Services[0].CustomDomain, s.Services[1].CustomDomain = s.Services[1].CustomDomain, s.Services[0].CustomDomain
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if v, ok, e := p.ServiceByDomain("two.example.org"); e != nil || !ok || v.ID != "platform" {
		t.Fatal("indexed domain lookup failed", e)
	}
}
func TestPostgresProjectionAndCursor(t *testing.T) {
	dsn := testpostgres.Database(t)
	initial := organizationSample(t)
	p := mustPostgres(t, dsn, initial)
	q := mustPostgres(t, dsn, State{})
	local := mustOpen(t, t.TempDir(), SQLite, initial)
	for _, kind := range []string{"organizations", "roles", "users", "sites", "services", "groups", "invitations"} {
		for _, filter := range []DirectoryFilter{{Global: true}, {OrganizationID: "org-a", All: true}, {OrganizationID: "org-b", All: true}, {OrganizationID: "org-a", ServiceIDs: []string{"platform"}, MemberID: "reader", ExcludeAdmins: true}} {
			query := DirectoryQuery{Kind: kind, Filter: filter}
			want, e := local.Directory(query)
			if e != nil {
				t.Fatal(e)
			}
			got, e := p.Directory(query)
			if e != nil {
				t.Fatal(kind, e)
			}
			if !reflect.DeepEqual(want.State, got.State) {
				t.Fatalf("%s filter mismatch: want %+v got %+v", kind, want.State, got.State)
			}
		}
	}
	projection, ok, e := p.UserContext("reader")
	if e != nil || !ok || len(projection.Groups) != 1 || len(projection.Roles) != 1 {
		t.Fatal("membership/role point projection failed", e)
	}
	query := DirectoryQuery{Kind: "services", Filter: DirectoryFilter{Global: true}, Limit: 1}
	first, e := p.Directory(query)
	if e != nil || first.NextCursor == "" {
		t.Fatal(e)
	}
	query.Cursor = first.NextCursor
	second, e := q.Directory(query)
	if e != nil || len(second.State.Services) != 1 || second.State.Services[0].ID != "metrics" {
		t.Fatal("cross-client cursor failed", e)
	}
	if e = q.Update(func(s *State) error { s.Services[1].Name = "new"; return nil }); e != nil {
		t.Fatal(e)
	}
	if _, e = p.Directory(query); !errors.Is(e, ErrDirectoryChanged) {
		t.Fatal("stale cursor accepted", e)
	}
	if p.QueryReadFailures() != 0 {
		t.Fatal("cursor conflict counted as DB read failure")
	}
	if e = q.Update(func(s *State) error {
		s.Groups[0].MemberIDs = nil
		s.Users[1].RoleID = ""
		s.Users[1].Disabled = true
		return nil
	}); e != nil {
		t.Fatal(e)
	}
	projection, ok, e = p.UserContext("reader")
	if e != nil || !ok || !projection.Users[0].Disabled || len(projection.Groups) != 0 || len(projection.Roles) != 0 {
		t.Fatal("stale ACL returned", e)
	}
	if e = p.Close(); e != nil {
		t.Fatal(e)
	}
	if _, ok, e = p.ServiceByDomain(""); e == nil || ok {
		t.Fatal("closed repository accepted query")
	}
}
func TestPostgresDatabaseConstraintsAndRollback(t *testing.T) {
	dsn := testpostgres.Database(t)
	p := mustPostgres(t, dsn, organizationSample(t))
	db, e := sql.Open("postgres", dsn)
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	// Direct SQL cannot manufacture cross-organization grants.
	if _, e = db.Exec(`INSERT INTO tunnel_state.user_sites(user_id,site_id,organization_id,position) VALUES('reader','site-a','org-b',0)`); e == nil {
		t.Fatal("database accepted foreign grant")
	}
	if _, e = db.Exec(`UPDATE tunnel_state.services SET payload=jsonb_set(payload,'{siteID}','"missing"') WHERE id='platform'`); e == nil {
		t.Fatal("database accepted orphan service")
	}
	before := snapshotPostgres(t, p)
	if _, e = db.Exec(`CREATE FUNCTION tunnel_state.reject_write() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected write failure'; END $$; CREATE TRIGGER fail_write BEFORE INSERT ON tunnel_state.services FOR EACH ROW EXECUTE FUNCTION tunnel_state.reject_write()`); e != nil {
		t.Fatal(e)
	}
	e = p.Update(func(s *State) error { s.Sites[0].Name = "must rollback"; return nil })
	if e == nil || strings.Contains(e.Error(), "injected") {
		t.Fatal("write failure absent or leaked", e)
	}
	if !reflect.DeepEqual(before, snapshotPostgres(t, p)) {
		t.Fatal("failed write changed data or revision")
	}
	if _, e = db.Exec(`DROP TRIGGER fail_write ON tunnel_state.services`); e != nil {
		t.Fatal(e)
	}
	// A reader's multiple statements cannot mix pre/post-write ACL versions.
	entered, committed := make(chan struct{}), make(chan struct{})
	errCh := make(chan error, 1)
	go func() {
		errCh <- p.read(context.Background(), func(ctx context.Context, tx *sql.Tx, _ pgMetadata) error {
			var old string
			if err := tx.QueryRowContext(ctx, `SELECT payload->>'name' FROM tunnel_state.sites WHERE id='site-a'`).Scan(&old); err != nil {
				return err
			}
			close(entered)
			<-committed
			var later string
			if err := tx.QueryRowContext(ctx, `SELECT payload->>'name' FROM tunnel_state.sites WHERE id='site-a'`).Scan(&later); err != nil {
				return err
			}
			if old != later {
				return errors.New("mixed snapshots")
			}
			return nil
		})
	}()
	<-entered
	q := mustPostgres(t, dsn, State{})
	e = q.Update(func(s *State) error { s.Sites[0].Name = "concurrent commit"; return nil })
	close(committed)
	if e != nil {
		t.Fatal(e)
	}
	if e = <-errCh; e != nil {
		t.Fatal(e)
	}
}
func TestPostgresRuntimeLeaseAndRecovery(t *testing.T) {
	ctx := context.Background()
	dsn := testpostgres.Database(t)
	initial := sample(t)
	shared := mustPostgres(t, dsn, initial)
	if _, e := OpenPostgresRuntime(ctx, t.TempDir(), dsn, nil); !errors.Is(e, ErrPostgresBusy) {
		t.Fatal("runtime coexists with shared client", e)
	}
	_ = shared.Close()
	dir := t.TempDir()
	runtime, e := OpenPostgresRuntime(ctx, dir, dsn, nil)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	if _, e = OpenPostgres(ctx, dsn, nil); !errors.Is(e, ErrPostgresBusy) {
		t.Fatal("shared client entered active runtime", e)
	}
	if _, e = OpenPostgresRuntime(ctx, t.TempDir(), dsn, nil); !errors.Is(e, ErrPostgresBusy) {
		t.Fatal("second runtime entered", e)
	}
	db, e := sql.Open("postgres", dsn)
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	if _, e = db.Exec(`SELECT pg_terminate_backend($1)`, runtime.pid); e != nil {
		t.Fatal(e)
	}
	select {
	case <-runtime.Failure():
	case <-time.After(8 * time.Second):
		t.Fatal("lost lease not detected")
	}
	if got := runtime.Snapshot(); len(got.Users) != 0 {
		t.Fatal("failed repository served cached state")
	}
	if _, e = OpenPostgresRuntime(ctx, t.TempDir(), dsn, nil); !errors.Is(e, ErrPostgresRecovery) {
		t.Fatal("crashed runtime latch bypassed", e)
	}
	_ = runtime.Close()
	if _, e = RecoverPostgresRuntime(ctx, dir, dsn, false); e == nil {
		t.Fatal("recovery without external fence acknowledgement")
	}
	if _, e = RecoverPostgresRuntime(ctx, dir, dsn, true); e != nil {
		t.Fatal(e)
	}
	replacement, e := OpenPostgresRuntime(ctx, dir, dsn, nil)
	if e != nil {
		t.Fatal(e)
	}
	if e = replacement.Close(); e != nil {
		t.Fatal(e)
	}
	p, e := OpenPostgresRuntime(ctx, dir, dsn, nil)
	if e != nil {
		t.Fatal("graceful close left crash latch", e)
	}
	_ = p.Close()
	if _, e = Open(dir, JSON, nil); e == nil {
		t.Fatal("local backend adopted PostgreSQL directory")
	}
	other := testpostgres.Database(t)
	mustPostgres(t, other, initial).Close()
	if _, e = OpenPostgresRuntime(ctx, dir, other, nil); e == nil {
		t.Fatal("directory rebound to other database")
	}
}
func TestPostgresMigrationAndPortableRestore(t *testing.T) {
	for _, kind := range []string{JSON, SQLite} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			initial := organizationSample(t)
			source := t.TempDir()
			local := mustOpen(t, source, kind, initial)
			_ = local.Close()
			name, _ := Filename(kind)
			original, e := os.ReadFile(filepath.Join(source, name))
			if e != nil {
				t.Fatal(e)
			}
			dsn := testpostgres.Database(t)
			dir := t.TempDir()
			if _, e = MigratePostgres(ctx, source, kind, dir, dsn); e != nil {
				t.Fatal(e)
			}
			after, e := os.ReadFile(filepath.Join(source, name))
			if e != nil || !reflect.DeepEqual(original, after) {
				t.Fatal("source changed", e)
			}
			p := mustPostgres(t, dsn, State{})
			if !reflect.DeepEqual(snapshotPostgres(t, p).State, initial) {
				t.Fatal("migration lost identities, credential hashes, grants or invites")
			}
			_ = p.Close()
			if _, e = MigratePostgres(ctx, source, kind, t.TempDir(), dsn); e == nil {
				t.Fatal("migration overwrote initialized DB")
			}
			backup := filepath.Join(t.TempDir(), "backup.json")
			if _, e = ExportPostgres(ctx, dir, dsn, backup); e != nil {
				t.Fatal(e)
			}
			if _, e = ExportPostgres(ctx, dir, dsn, backup); e == nil {
				t.Fatal("export overwrote backup")
			}
			restoredDSN := testpostgres.Database(t)
			if _, e = RestorePostgres(ctx, t.TempDir(), restoredDSN, backup); e != nil {
				t.Fatal(e)
			}
			want := Clone(initial)
			want.Invites = nil
			got := mustPostgres(t, restoredDSN, State{})
			if !reflect.DeepEqual(snapshotPostgres(t, got).State, want) {
				t.Fatal("PG restore differs")
			}
			for _, target := range []string{JSON, SQLite} {
				dest := t.TempDir()
				if _, e = Restore(dest, target, backup); e != nil {
					t.Fatal(e)
				}
				restored := mustOpen(t, dest, target, State{})
				if !reflect.DeepEqual(restored.Snapshot(), want) {
					t.Fatal("portable restore differs", target)
				}
			}
		})
	}
}
func TestPostgresDSNRedaction(t *testing.T) {
	_, e := OpenPostgres(context.Background(), "postgres://secret-user:secret-password@127.0.0.1:1/private-db?sslmode=disable", nil)
	if e == nil || strings.Contains(e.Error(), "secret") || strings.Contains(e.Error(), "private-db") {
		t.Fatal("DSN exposed", e)
	}
}

func TestPostgresConcurrentLastAdminProtection(t *testing.T) {
	dsn := testpostgres.Database(t)
	initial := sample(t)
	initial.Users[1].Role = "admin"
	p := mustPostgres(t, dsn, initial)
	q := mustPostgres(t, dsn, State{})
	start := make(chan struct{})
	results := make(chan error, 2)
	for i, client := range []*PostgresRepository{p, q} {
		go func(i int, c *PostgresRepository) {
			<-start
			results <- c.Update(func(s *State) error { s.Users[i].Disabled = true; return nil })
		}(i, client)
	}
	close(start)
	successes := 0
	for range 2 {
		if <-results == nil {
			successes++
		}
	}
	final := snapshotPostgres(t, p)
	enabled := 0
	for _, u := range final.State.Users {
		if u.Role == "admin" && !u.Disabled {
			enabled++
		}
	}
	if successes != 1 || enabled != 1 || final.Revision != 1 {
		t.Fatal("concurrent writes removed last administrator", successes, enabled, final.Revision)
	}
}
func TestPostgresInitializationAndSchemaRefusal(t *testing.T) {
	dsn := testpostgres.Database(t)
	initial := sample(t)
	start := make(chan struct{})
	results := make(chan *PostgresRepository, 2)
	errs := make(chan error, 2)
	var calls atomic.Int32
	for range 2 {
		go func() {
			<-start
			p, e := OpenPostgres(context.Background(), dsn, func() (State, error) { calls.Add(1); return Clone(initial), nil })
			results <- p
			errs <- e
		}()
	}
	close(start)
	for range 2 {
		p := <-results
		err := <-errs
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = p.Close() })
	}
	if calls.Load() != 1 {
		t.Fatal("initializer replayed", calls.Load())
	}
	db, e := sql.Open("postgres", dsn)
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	if _, e = db.Exec(`UPDATE tunnel_state.meta SET schema_version=999`); e != nil {
		t.Fatal(e)
	}
	if p, e := OpenPostgres(context.Background(), dsn, func() (State, error) { t.Error("unknown schema reinitialized"); return initial, nil }); e == nil {
		_ = p.Close()
		t.Fatal("unknown schema accepted")
	}
}
