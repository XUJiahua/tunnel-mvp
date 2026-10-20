package state

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/XUJiahua/tunnel-mvp/internal/testpostgres"
)

func clusterRepository(t *testing.T, dsn, id string, initial State) *PostgresRepository {
	t.Helper()
	p, err := OpenPostgresClusterRuntime(context.Background(), t.TempDir(), dsn, func() (State, error) { return initial, nil })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	if err = p.AuthBindPolicy(context.Background(), strings.Repeat("f", 64)); err != nil {
		t.Fatal(err)
	}
	if _, err = p.ClusterRegister(context.Background(), ClusterNode{ID: id, Epoch: strings.Repeat(id[:1], 32), URL: "https://" + id + ".example:8443", Fingerprint: strings.Repeat("a", 64)}); err != nil {
		t.Fatal(err)
	}
	return p
}
func TestPostgresClusterOwnershipGenerationAndCredentialChanges(t *testing.T) {
	ctx := context.Background()
	dsn := testpostgres.Database(t)
	initial := sample(t)
	p := clusterRepository(t, dsn, "a", initial)
	q := clusterRepository(t, dsn, "b", State{})
	if _, err := OpenPostgresRuntime(ctx, t.TempDir(), dsn, nil); !errors.Is(err, ErrPostgresBusy) {
		t.Fatal("single-active entered a cluster", err)
	}
	if _, err := OpenPostgres(ctx, dsn, nil); !errors.Is(err, ErrPostgresRecovery) {
		t.Fatal("unregistered client entered cluster runtime", err)
	}
	agent := ClusterAgent{SiteID: "site-a", CredentialHash: initial.Sites[0].TokenHash}
	one, err := p.ClusterClaimAgent(ctx, agent)
	if err != nil || one.Agent.Generation != 1 {
		t.Fatal(err)
	}
	if _, err = q.ClusterClaimAgent(ctx, agent); !errors.Is(err, ErrClusterBusy) {
		t.Fatal("duplicate Agent admitted", err)
	}
	view, err := q.ClusterRoutes(ctx, "site-a")
	if err != nil || len(view.Routes) != 1 || view.Routes[0].Node.ID != "a" {
		t.Fatal("remote owner missing", err)
	}
	one.Agent.Channels = 3
	heartbeat, err := p.ClusterHeartbeat(ctx, []ClusterAgent{one.Agent})
	if err != nil || len(heartbeat.Routes) != 1 || heartbeat.Routes[0].Agent.Channels != 3 || len(heartbeat.Nodes) != 2 {
		t.Fatal("heartbeat did not bind channels and peers", err)
	}
	if err = p.ClusterReleaseAgent(ctx, one.Agent.Key, one.Agent.Generation); err != nil {
		t.Fatal(err)
	}
	two, err := q.ClusterClaimAgent(ctx, agent)
	if err != nil || two.Agent.Generation <= one.Agent.Generation {
		t.Fatal("generation did not increase", err)
	}
	if err = p.ClusterReleaseAgent(ctx, one.Agent.Key, one.Agent.Generation); err != nil {
		t.Fatal(err)
	}
	view, err = p.ClusterRoutes(ctx, "site-a")
	if err != nil || len(view.Routes) != 1 || view.Routes[0].Agent.Generation != two.Agent.Generation {
		t.Fatal("old release removed new owner", err)
	}
	if err = p.Update(func(s *State) error { s.Sites[0].TokenHash = strings.Repeat("c", 64); return nil }); err != nil {
		t.Fatal(err)
	}
	view, err = q.ClusterRoutes(ctx, "site-a")
	if err != nil || len(view.Routes) != 0 {
		t.Fatal("rotated credential retained route", err)
	}
	heartbeat, err = q.ClusterHeartbeat(ctx, nil)
	if err != nil || len(heartbeat.Routes) != 0 {
		t.Fatal("revoked connection still owned", err)
	}
}
func TestPostgresClusterExpiredIncarnationsCannotRevive(t *testing.T) {
	ctx := context.Background()
	dsn := testpostgres.Database(t)
	initial := sample(t)
	p := clusterRepository(t, dsn, "a", initial)
	q := clusterRepository(t, dsn, "b", State{})
	claim, err := p.ClusterClaimAgent(ctx, ClusterAgent{SiteID: "site-a", CredentialHash: initial.Sites[0].TokenHash})
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(`UPDATE tunnel_state.cluster_nodes SET expires_at=clock_timestamp()-interval '1 second' WHERE id='a'`); err != nil {
		t.Fatal(err)
	}
	successor, err := q.ClusterClaimAgent(ctx, ClusterAgent{SiteID: "site-a", CredentialHash: initial.Sites[0].TokenHash})
	if err != nil || successor.Agent.Generation <= claim.Agent.Generation {
		t.Fatal("expired owner not replaced", err)
	}
	if _, err = p.ClusterHeartbeat(ctx, nil); !errors.Is(err, ErrClusterLease) {
		t.Fatal("expired node revived", err)
	}
	replacement, e := OpenPostgresClusterRuntime(ctx, t.TempDir(), dsn, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer replacement.Close()
	if e = replacement.AuthBindPolicy(ctx, strings.Repeat("f", 64)); e != nil {
		t.Fatal(e)
	}
	if _, e = replacement.ClusterRegister(ctx, ClusterNode{ID: "a", Epoch: strings.Repeat("e", 32), URL: "https://a.example:8443", Fingerprint: strings.Repeat("a", 64)}); e != nil {
		t.Fatal(e)
	}
	if _, err = p.ClusterRoutes(ctx, "site-a"); err == nil {
		t.Fatal("old incarnation read authority after replacement")
	}
	if err = replacement.ClusterCloseNode(ctx); err != nil {
		t.Fatal("graceful node release failed", err)
	}
	view, err := q.ClusterRoutes(ctx, "site-a")
	if err != nil || len(view.Routes) != 1 || view.Routes[0].Node.ID != "b" {
		t.Fatal("node close affected another owner", err)
	}
}
func TestPostgresClusterV2UpgradeAndModeExclusion(t *testing.T) {
	ctx := context.Background()
	dsn := testpostgres.Database(t)
	p := mustPostgres(t, dsn, sample(t))
	p.Close()
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(`DROP TABLE tunnel_state.cluster_agents,tunnel_state.cluster_nodes;UPDATE tunnel_state.meta SET schema_version=2`); err != nil {
		t.Fatal(err)
	}
	node := clusterRepository(t, dsn, "a", State{})
	snapshot, err := node.SnapshotContext(ctx)
	if err != nil || len(snapshot.State.Users) != 2 {
		t.Fatal("schema upgrade lost configuration", err)
	}
	if err = node.ClusterCloseNode(ctx); err != nil {
		t.Fatal(err)
	}
	node.Close()
	if _, err = OpenPostgresRuntime(ctx, t.TempDir(), dsn, nil); !errors.Is(err, ErrPostgresRecovery) {
		t.Fatal("mode switch bypassed offline recovery", err)
	}
}
