package state

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/XUJiahua/tunnel-mvp/internal/testpostgres"
)

var _ AuthRepository = (*PostgresRepository)(nil)

const authSessionKey = "1111111111111111111111111111111111111111111111111111111111111111"
const authTicketKey = "2222222222222222222222222222222222222222222222222222222222222222"
const authAccessKey = "3333333333333333333333333333333333333333333333333333333333333333"

func seedPostgresAuth(t *testing.T, p *PostgresRepository) {
	t.Helper()
	err := p.AuthUpdate(context.Background(), true, func(a *AuthData, st State, now time.Time) error {
		a.Sessions[authSessionKey] = AuthSession{ID: "session-one", UserID: st.Users[1].ID, Method: "password", CreatedAt: now, LastSeen: now, Expires: now.Add(time.Hour)}
		a.Tickets[authTicketKey] = AuthGrant{SessionKey: authSessionKey, ServiceID: st.Services[0].ID, Expires: now.Add(30 * time.Second)}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
func TestPostgresSharedAuthenticationAndOneTimeConsumption(t *testing.T) {
	ctx := context.Background()
	dsn := testpostgres.Database(t)
	p := mustPostgres(t, dsn, sample(t))
	q := mustPostgres(t, dsn, State{})
	policy := strings.Repeat("f", 64)
	for _, client := range []*PostgresRepository{p, q} {
		if err := client.AuthBindPolicy(ctx, policy); err != nil {
			t.Fatal(err)
		}
	}
	seedPostgresAuth(t, p)
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, client := range []*PostgresRepository{p, q} {
		wg.Add(1)
		go func(c *PostgresRepository) {
			defer wg.Done()
			<-start
			results <- c.AuthUpdate(ctx, true, func(a *AuthData, _ State, now time.Time) error {
				grant, exists := a.Tickets[authTicketKey]
				if !exists {
					return ErrAuthChanged
				}
				delete(a.Tickets, authTicketKey)
				if !now.Before(grant.Expires) {
					return ErrAuthChanged
				}
				grant.Expires = a.Sessions[grant.SessionKey].Expires
				a.Access[authAccessKey] = grant
				return nil
			})
		}(client)
	}
	close(start)
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		} else if !errors.Is(err, ErrAuthChanged) {
			t.Fatal(err)
		}
	}
	if successes != 1 {
		t.Fatal("ticket consumed multiple times", successes)
	}
	resolved, err := q.AuthResolve(ctx, "access", authAccessKey)
	if err != nil || !resolved.Found || resolved.Session.UserID != "reader" || len(resolved.State.Services) != 1 {
		t.Fatal("cross-client session/grant resolution failed", err)
	}
	projection, err := p.AuthSnapshotForKeys(ctx, []string{authAccessKey}, []string{authSessionKey}, nil)
	if err != nil || len(projection.Data.Access) != 1 || len(projection.State.Users) != 1 {
		t.Fatal("bounded flow projection failed", err)
	}
	if err = q.AuthUpdate(ctx, false, func(a *AuthData, _ State, _ time.Time) error { delete(a.Sessions, authSessionKey); return nil }); err != nil {
		t.Fatal(err)
	}
	if resolved, err = p.AuthResolve(ctx, "access", authAccessKey); err != nil || resolved.Found {
		t.Fatal("revoked session still accepted", err)
	}
	events, err := p.AuthPoll(ctx, "node-a", 200)
	if err != nil || len(events.Events) < 2 {
		t.Fatal("durable invalidation missing", err)
	}
	last := events.Events[len(events.Events)-1].ID
	if err = p.AuthAcknowledge(ctx, "node-a", last); err != nil {
		t.Fatal(err)
	}
	if err = q.AuthAcknowledge(ctx, "node-a", 0); err != nil {
		t.Fatal(err)
	}
	if events, err = q.AuthPoll(ctx, "node-a", 200); err != nil || len(events.Events) != 0 {
		t.Fatal("consumer cursor moved backwards", err)
	}
	if err = p.AuthAcknowledge(ctx, "node-a", last+100); err == nil {
		t.Fatal("unseen event acknowledged")
	}
}
func TestPostgresConfigurationAndAuthenticationCommitTogether(t *testing.T) {
	ctx := context.Background()
	dsn := testpostgres.Database(t)
	p := mustPostgres(t, dsn, sample(t))
	q := mustPostgres(t, dsn, State{})
	seedPostgresAuth(t, p)
	if err := q.Update(func(s *State) error { s.Sites[0].Name = "metadata only"; return nil }); err != nil {
		t.Fatal(err)
	}
	if s, e := p.AuthResolve(ctx, "session", authSessionKey); e != nil || !s.Found {
		t.Fatal("unrelated configuration revoked session", e)
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(`CREATE FUNCTION tunnel_state.fail_auth_config_write() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'rollback auth change'; END $$; CREATE TRIGGER fail_auth_config BEFORE INSERT ON tunnel_state.services FOR EACH ROW EXECUTE FUNCTION tunnel_state.fail_auth_config_write()`); err != nil {
		t.Fatal(err)
	}
	before := snapshotPostgres(t, p)
	err = p.Update(func(s *State) error { s.Users[1].Disabled = true; return nil })
	if err == nil {
		t.Fatal("injected failure ignored")
	}
	if !reflect.DeepEqual(before, snapshotPostgres(t, q)) {
		t.Fatal("configuration partially committed")
	}
	if s, e := q.AuthResolve(ctx, "session", authSessionKey); e != nil || !s.Found {
		t.Fatal("failed configuration commit revoked session", e)
	}
	if _, err = db.Exec(`DROP TRIGGER fail_auth_config ON tunnel_state.services`); err != nil {
		t.Fatal(err)
	}
	if err = q.Update(func(s *State) error { s.Users[1].Disabled = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if s, e := p.AuthResolve(ctx, "session", authSessionKey); e != nil || s.Found {
		t.Fatal("configuration revocation not atomic", e)
	}
	called := false
	err = p.UpdateAuthenticated(ctx, func(_ State, a AuthData, _ time.Time) error {
		if _, ok := a.Sessions[authSessionKey]; !ok {
			return ErrAuthChanged
		}
		return nil
	}, func(s *State) error { called = true; s.Sites[0].Name = "unauthorized"; return nil })
	if !errors.Is(err, ErrAuthChanged) || called {
		t.Fatal("revoked actor invoked mutation", err)
	}
}
func TestPostgresAuthenticationPolicyCapacityAndExpiry(t *testing.T) {
	ctx := context.Background()
	dsn := testpostgres.Database(t)
	p := mustPostgres(t, dsn, sample(t))
	q := mustPostgres(t, dsn, State{})
	if err := p.AuthBindPolicy(ctx, strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	seedPostgresAuth(t, p)
	if err := q.AuthBindPolicy(ctx, strings.Repeat("b", 64)); !errors.Is(err, ErrAuthPolicy) {
		t.Fatal("mismatched node policy accepted", err)
	}
	var successes int
	for range 5 {
		allowed, err := q.AuthRateLimit(ctx, "login", 3, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if allowed {
			successes++
		}
	}
	if successes != 3 {
		t.Fatal("shared rate limit not enforced", successes)
	}
	var raw string
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = db.QueryRow(`SELECT payload->>'Count' FROM tunnel_state.auth_records WHERE kind='limit' AND key='login'`).Scan(&raw); err != nil || raw != "3" {
		t.Fatal("rate window overcounted", raw, err)
	}
	_ = p.Close()
	_ = q.Close()
	runtime, err := OpenPostgresRuntime(ctx, t.TempDir(), dsn, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	if err = runtime.AuthBindPolicy(ctx, strings.Repeat("b", 64)); err != nil {
		t.Fatal(err)
	}
	if s, e := runtime.AuthResolve(ctx, "session", authSessionKey); e != nil || s.Found {
		t.Fatal("changed policy preserved old session", e)
	}
}
func TestPostgresAuthSchemaV1UpgradePreservesConfiguration(t *testing.T) {
	ctx := context.Background()
	dsn := testpostgres.Database(t)
	initial := sample(t)
	p := mustPostgres(t, dsn, initial)
	_ = p.Close()
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(`DROP TABLE tunnel_state.auth_records; DROP TABLE tunnel_state.auth_policy; DROP TABLE tunnel_state.auth_outbox; DROP TABLE tunnel_state.auth_consumers; UPDATE tunnel_state.meta SET schema_version=1`); err != nil {
		t.Fatal(err)
	}
	q, err := OpenPostgres(ctx, dsn, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	if !reflect.DeepEqual(q.Snapshot(), initial) {
		t.Fatal("auth schema upgrade changed credentials/configuration")
	}
	snapshot, err := q.AuthSnapshot(ctx, true)
	if err != nil || len(snapshot.Data.Sessions) != 0 {
		t.Fatal("schema upgrade revived an old session", err)
	}
	var version int
	if err = db.QueryRow(`SELECT schema_version FROM tunnel_state.meta`).Scan(&version); err != nil || version != PostgresSchemaVersion {
		t.Fatal("auth schema upgrade missing", err)
	}
}

func TestPostgresCancelledRequestDoesNotDropRuntimeLease(t *testing.T) {
	ctx := context.Background()
	dsn := testpostgres.Database(t)
	dir := t.TempDir()
	initial := sample(t)
	p, err := OpenPostgresRuntime(ctx, dir, dsn, func() (State, error) { return initial, nil })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	blocker, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback()
	if _, err = blocker.Exec(`SELECT revision FROM tunnel_state.meta WHERE singleton FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	requestCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	_, err = p.AuthRateLimit(requestCtx, "cancelled-request", 3, time.Minute)
	cancel()
	if err == nil {
		t.Fatal("blocked request ignored cancellation")
	}
	if err = blocker.Rollback(); err != nil {
		t.Fatal(err)
	}
	// At least one heartbeat runs after the request connection is discarded.
	select {
	case <-p.Failure():
		t.Fatal("client cancellation fenced the runtime")
	case <-time.After(1200 * time.Millisecond):
	}
	if s, e := p.SnapshotContext(ctx); e != nil || len(s.State.Users) != 2 {
		t.Fatal("runtime did not retain its live lease", e)
	}
	if _, e := OpenPostgresRuntime(ctx, t.TempDir(), dsn, nil); !errors.Is(e, ErrPostgresBusy) {
		t.Fatal("second runtime entered after request cancellation", e)
	}
	if err = p.Close(); err != nil {
		t.Fatal("graceful close left abandoned owner", err)
	}
	reopened, err := OpenPostgresRuntime(ctx, dir, dsn, nil)
	if err != nil {
		t.Fatal("graceful restart required crash recovery", err)
	}
	_ = reopened.Close()
}

func TestPostgresSharedIdentityChecksAreFencedAndNeverReviveCookies(t *testing.T) {
	ctx := context.Background()
	dsn := testpostgres.Database(t)
	initial := sample(t)
	identity := Identity{Issuer: "https://idp.example.org/realm", Subject: "subject-reader"}
	initial.Users[1].Identity = &identity
	p := mustPostgres(t, dsn, initial)
	q := mustPostgres(t, dsn, State{})
	first, owner, err := p.AuthIdentityStart(ctx, "reader", identity)
	if err != nil || !owner {
		t.Fatal(err)
	}
	joined, owner, err := q.AuthIdentityStart(ctx, "reader", identity)
	if err != nil || owner || joined.CheckID != first.CheckID {
		t.Fatal("cross-node check not coalesced", err)
	}
	verified, applied, err := p.AuthIdentityFinish(ctx, "reader", first.CheckID, true, true, time.Second)
	if err != nil || !applied || !verified.Enabled {
		t.Fatal(err)
	}
	passwordKey := strings.Repeat("4", 64)
	err = p.AuthUpdate(ctx, true, func(a *AuthData, _ State, now time.Time) error {
		a.Sessions[authSessionKey] = AuthSession{ID: "oidc-session", Method: "oidc", UserID: "reader", Issuer: identity.Issuer, Subject: identity.Subject, IdentityEpoch: verified.Epoch, CreatedAt: now, LastSeen: now, Expires: now.Add(time.Hour)}
		a.Sessions[passwordKey] = AuthSession{ID: "password-session", Method: "password", UserID: "reader", CreatedAt: now, LastSeen: now, Expires: now.Add(time.Hour)}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(1200 * time.Millisecond)
	second, owner, err := q.AuthIdentityStart(ctx, "reader", identity)
	if err != nil || !owner || second.Epoch == verified.Epoch {
		t.Fatal("expired identity epoch reused", err)
	}
	current, applied, err := q.AuthIdentityFinish(ctx, "reader", second.CheckID, true, true, time.Minute)
	if err != nil || !applied {
		t.Fatal(err)
	}
	if resolved, e := p.AuthResolve(ctx, "session", authSessionKey); e != nil || resolved.Found {
		t.Fatal("new enabled result resurrected expired cookie", e)
	}
	if resolved, e := p.AuthResolve(ctx, "session", passwordKey); e != nil || !resolved.Found {
		t.Fatal("identity expiry removed password recovery session", e)
	}
	if _, applied, e := p.AuthIdentityFinish(ctx, "reader", first.CheckID, true, false, time.Minute); e != nil || applied {
		t.Fatal("obsolete check overwrote newer authority", e)
	}
	third, owner, err := p.AuthIdentityStart(ctx, "reader", identity)
	if err != nil || !owner {
		t.Fatal(err)
	}
	failed, applied, err := p.AuthIdentityFinish(ctx, "reader", third.CheckID, false, false, time.Minute)
	if err != nil || !applied || !failed.Enabled || !failed.ValidUntil.Equal(current.ValidUntil) || failed.CheckSucceeded {
		t.Fatal("failed check extended or forged fresh authority", err)
	}
	// A fresh disabled result atomically removes only OIDC sessions.
	if err = p.AuthUpdate(ctx, true, func(a *AuthData, _ State, now time.Time) error {
		a.Sessions[authSessionKey] = AuthSession{ID: "new-oidc", Method: "oidc", UserID: "reader", Issuer: identity.Issuer, Subject: identity.Subject, IdentityEpoch: current.Epoch, CreatedAt: now, LastSeen: now, Expires: now.Add(time.Hour)}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	fourth, _, err := q.AuthIdentityStart(ctx, "reader", identity)
	if err != nil {
		t.Fatal(err)
	}
	disabled, applied, err := q.AuthIdentityFinish(ctx, "reader", fourth.CheckID, true, false, time.Minute)
	if err != nil || !applied || disabled.Revoked != 1 {
		t.Fatal("disabled result did not atomically revoke OIDC", err, disabled.Revoked)
	}
	if resolved, e := p.AuthResolve(ctx, "session", authSessionKey); e != nil || resolved.Found {
		t.Fatal("disabled identity cookie survived", e)
	}
	if resolved, e := p.AuthResolve(ctx, "session", passwordKey); e != nil || !resolved.Found {
		t.Fatal("disabled identity revoked password recovery", e)
	}
}
func TestPostgresOutboxRetentionKeepsDurableWatermark(t *testing.T) {
	ctx := context.Background()
	dsn := testpostgres.Database(t)
	p := mustPostgres(t, dsn, sample(t))
	if err := p.AuthBindPolicy(ctx, strings.Repeat("d", 64)); err != nil {
		t.Fatal(err)
	}
	events, err := p.AuthPoll(ctx, "consumer", 200)
	if err != nil || len(events.Events) == 0 {
		t.Fatal(err)
	}
	last := events.Events[len(events.Events)-1].ID
	if err = p.AuthAcknowledge(ctx, "consumer", last); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(`UPDATE tunnel_state.auth_outbox SET created_at=clock_timestamp()-interval '2 days'`); err != nil {
		t.Fatal(err)
	}
	if err = p.AuthMaintenance(ctx); err != nil {
		t.Fatal(err)
	}
	snapshot, err := p.AuthSnapshot(ctx, false)
	if err != nil || snapshot.EventID != last {
		t.Fatal("retention erased the event watermark", err)
	}
	events, err = p.AuthPoll(ctx, "consumer", 200)
	if err != nil || events.Reset || len(events.Events) != 0 {
		t.Fatal("up-to-date consumer reset after retention", err)
	}
	events, err = p.AuthPoll(ctx, "new-consumer", 200)
	if err != nil || !events.Reset {
		t.Fatal("retention gap was not detected", err)
	}
	if err = p.AuthAcknowledge(ctx, "new-consumer", snapshot.EventID); err != nil {
		t.Fatal(err)
	}
	if events, err = p.AuthPoll(ctx, "new-consumer", 200); err != nil || events.Reset {
		t.Fatal("resnapshot did not repair retention gap", err)
	}
}

func TestPostgresAuthRestartAndAuthorizationRevision(t *testing.T) {
	ctx := context.Background()
	dsn := testpostgres.Database(t)
	dir := t.TempDir()
	initial := sample(t)
	p, err := OpenPostgresRuntime(ctx, dir, dsn, func() (State, error) { return initial, nil })
	if err != nil {
		t.Fatal(err)
	}
	policy := strings.Repeat("e", 64)
	if err = p.AuthBindPolicy(ctx, policy); err != nil {
		t.Fatal(err)
	}
	seedPostgresAuth(t, p)
	if err = p.AuthUpdate(ctx, false, func(a *AuthData, _ State, now time.Time) error {
		a.Pending[strings.Repeat("8", 64)] = AuthOIDCAttempt{Browser: strings.Repeat("9", 64), Nonce: "nonce", Verifier: "verifier", Issuer: "https://idp.example", ClientID: "console", RedirectURI: "https://console.example/auth/oidc/callback", Started: now, Expires: now.Add(5 * time.Minute)}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err = p.Close(); err != nil {
		t.Fatal(err)
	}
	p, err = OpenPostgresRuntime(ctx, dir, dsn, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if err = p.AuthBindPolicy(ctx, policy); err != nil {
		t.Fatal(err)
	}
	resolution, err := p.AuthResolve(ctx, "session", authSessionKey)
	if err != nil || !resolution.Found {
		t.Fatal("normal restart discarded session", err)
	}
	snapshot, err := p.AuthSnapshot(ctx, false)
	if err != nil || len(snapshot.Data.Pending) != 1 || len(snapshot.Data.Tickets) != 1 {
		t.Fatal("normal restart discarded pending authentication", err)
	}
	query := DirectoryQuery{Kind: "services", Filter: DirectoryFilter{Global: true, ExpectedRevision: &resolution.ConfigRevision}}
	if _, err = p.Directory(query); err != nil {
		t.Fatal(err)
	}
	if err = p.Update(func(s *State) error { s.Sites[0].Name = "new metadata"; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err = p.Directory(query); !errors.Is(err, ErrDirectoryChanged) {
		t.Fatal("stale authorization scope used against new revision", err)
	}
	if err = p.Close(); err != nil {
		t.Fatal(err)
	}
	p, err = OpenPostgresRuntime(ctx, dir, dsn, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if err = p.AuthBindPolicy(ctx, strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	if resolution, err = p.AuthResolve(ctx, "session", authSessionKey); err != nil || resolution.Found {
		t.Fatal("policy change preserved old session", err)
	}
	snapshot, err = p.AuthSnapshot(ctx, false)
	if err != nil || len(snapshot.Data.Pending) != 0 {
		t.Fatal("policy change preserved old OIDC attempt", err)
	}
}
