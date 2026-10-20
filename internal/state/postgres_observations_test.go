package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/XUJiahua/tunnel-mvp/internal/testpostgres"
)

func observationForTest(id, kind string) Observation {
	return Observation{ID: strings.Repeat(id, 64), Source: strings.Repeat("a", 48), NodeID: "a", Epoch: strings.Repeat("a", 32), Kind: kind, At: time.Now().UTC().Truncate(time.Microsecond), Payload: json.RawMessage(`{"event":"shared","nodeID":"forged"}`)}
}
func observationGuard(a AuthResolution) (ObservationScope, error) {
	if !a.Found {
		return ObservationScope{}, ErrAuthChanged
	}
	return ObservationScope{All: true}, nil
}
func TestPostgresClusterObservationReplayAtomicityAuthorizationAndProfiles(t *testing.T) {
	ctx := context.Background()
	dsn := testpostgres.Database(t)
	p := clusterRepository(t, dsn, "a", sample(t))
	q := clusterRepository(t, dsn, "b", State{})
	seedPostgresAuth(t, p)
	one := observationForTest("1", "audit")
	if err := p.ObservationPublish(ctx, []Observation{one, one}); err != nil {
		t.Fatal(err)
	}
	if err := p.ObservationPublish(ctx, []Observation{one}); err != nil {
		t.Fatal("ambiguous commit retry", err)
	}
	page, err := q.ObservationAudit(ctx, authSessionKey, ObservationAuditFilter{Limit: 10}, observationGuard)
	if err != nil || len(page.Entries) != 1 || !strings.Contains(string(page.Entries[0]), `"nodeID": "a"`) {
		t.Fatal("shared dedup or provenance", page, err)
	}
	conflict := one
	conflict.Payload = json.RawMessage(`{"event":"corrupted"}`)
	two := observationForTest("2", "audit")
	if err = p.ObservationPublish(ctx, []Observation{two, conflict}); err == nil {
		t.Fatal("reused ID accepted different content")
	}
	page, err = q.ObservationAudit(ctx, authSessionKey, ObservationAuditFilter{Limit: 10}, observationGuard)
	if err != nil || len(page.Entries) != 1 {
		t.Fatal("conflict partially committed", page, err)
	}
	orgA, orgB := "org-a", "org-b"
	scopedA := observationForTest("6", "audit")
	scopedA.OrganizationID = &orgA
	scopedB := observationForTest("7", "audit")
	scopedB.OrganizationID = &orgB
	if err = p.ObservationPublish(ctx, []Observation{scopedA, scopedB}); err != nil {
		t.Fatal(err)
	}
	scoped, err := q.ObservationAudit(ctx, authSessionKey, ObservationAuditFilter{Limit: 10}, func(AuthResolution) (ObservationScope, error) { return ObservationScope{OrganizationID: orgA}, nil })
	if err != nil || len(scoped.Entries) != 1 {
		t.Fatal("organization scope leaked unknown or other-tenant audit", scoped, err)
	}

	// A source identifier cannot silently migrate between nodes.
	copied := two
	copied.NodeID = "b"
	copied.Epoch = strings.Repeat("b", 32)
	if err = q.ObservationPublish(ctx, []Observation{copied}); err == nil {
		t.Fatal("copied source changed provenance")
	}
	sample := observationForTest("3", "sample")
	sample.ServiceID = "platform"
	sample.Profile = strings.Repeat("f", 64)
	sample.Payload = json.RawMessage(`{"healthy":true}`)
	if err = p.ObservationPublish(ctx, []Observation{sample}); err != nil {
		t.Fatal(err)
	}
	guard := func(a AuthResolution, s Service) (string, error) {
		if !a.Found || s.ID != "platform" {
			return "", ErrAuthChanged
		}
		return strings.Repeat("f", 64), nil
	}
	samples, err := q.ObservationSamples(ctx, authSessionKey, "platform", time.Now().Add(-time.Hour), 10, guard)
	if err != nil || len(samples) != 1 {
		t.Fatal("shared samples", err)
	}
	latest, err := q.ObservationLatest(ctx, authSessionKey, func(a AuthResolution, s Service) (string, bool, error) {
		return strings.Repeat("f", 64), s.ID == "platform", nil
	})
	if err != nil || len(latest) != 1 {
		t.Fatal("shared latest", latest, err)
	}
	samples, err = q.ObservationSamples(ctx, authSessionKey, "platform", time.Now().Add(-time.Hour), 10, func(AuthResolution, Service) (string, error) { return strings.Repeat("e", 64), nil })
	if err != nil || len(samples) != 0 {
		t.Fatal("old health profile leaked", err)
	}
	if err = p.Update(func(s *State) error { s.Users[1].Disabled = true; return nil }); err != nil {
		t.Fatal(err)
	}
	called := false
	_, err = q.ObservationAudit(ctx, authSessionKey, ObservationAuditFilter{Limit: 10}, func(a AuthResolution) (ObservationScope, error) { called = true; return observationGuard(a) })
	if !errors.Is(err, ErrAuthChanged) || called {
		t.Fatal("revoked actor reached data guard", err)
	}
}
func TestPostgresClusterObservationLegacySnapshotBarrierAndDrain(t *testing.T) {
	ctx := context.Background()
	dsn := testpostgres.Database(t)
	initial := sample(t)
	p := clusterRepository(t, dsn, "a", initial)
	q := clusterRepository(t, dsn, "b", State{})
	seedPostgresAuth(t, p)
	first := observationForTest("1", "audit")
	first.LegacySnapshot = strings.Repeat("c", 48)
	first.Epoch = ""
	equal := first
	equal.ID = strings.Repeat("2", 64)
	if err := p.ObservationPublish(ctx, []Observation{first, equal}); err != nil {
		t.Fatal(err)
	}
	page, err := q.ObservationAudit(ctx, authSessionKey, ObservationAuditFilter{Limit: 10}, observationGuard)
	if err != nil || len(page.Entries) != 0 {
		t.Fatal("partial legacy snapshot visible", err)
	}
	end := first
	end.Kind = "legacy_end"
	end.ID = strings.Repeat("3", 64)
	end.Payload = json.RawMessage(`{}`)
	if err = p.ObservationPublish(ctx, []Observation{end}); err != nil {
		t.Fatal(err)
	}
	page, err = q.ObservationAudit(ctx, authSessionKey, ObservationAuditFilter{Limit: 1}, observationGuard)
	if err != nil || len(page.Entries) != 1 || page.Next == 0 {
		t.Fatal("identical legacy lines collapsed", page, err)
	}
	epoch := page.Epoch
	if err = p.ObservationPublish(ctx, []Observation{end}); err != nil {
		t.Fatal(err)
	}
	if _, err = q.ObservationAudit(ctx, authSessionKey, ObservationAuditFilter{Limit: 10, Epoch: epoch}, observationGuard); err != nil {
		t.Fatal("idempotent completion changed cursor", err)
	}
	next := first
	next.ID = strings.Repeat("4", 64)
	next.LegacySnapshot = strings.Repeat("d", 48)
	end.ID = strings.Repeat("5", 64)
	end.LegacySnapshot = next.LegacySnapshot
	if err = p.ObservationPublish(ctx, []Observation{next, end}); err != nil {
		t.Fatal(err)
	}
	if _, err = q.ObservationAudit(ctx, authSessionKey, ObservationAuditFilter{Limit: 10, Epoch: epoch}, observationGuard); !errors.Is(err, ErrObservationCursor) {
		t.Fatal("legacy replacement retained unsafe cursor", err)
	}
	page, err = q.ObservationAudit(ctx, authSessionKey, ObservationAuditFilter{Limit: 10}, observationGuard)
	if err != nil || len(page.Entries) != 1 {
		t.Fatal("old legacy snapshot still counted", err)
	}
	claim, err := p.ClusterClaimAgent(ctx, ClusterAgent{SiteID: "site-a", CredentialHash: initial.Sites[0].TokenHash})
	if err != nil {
		t.Fatal(err)
	}
	claim.Agent.Ready = true
	if _, err = p.ClusterHeartbeat(ctx, []ClusterAgent{claim.Agent}); err != nil {
		t.Fatal(err)
	}
	guard := func(a AuthResolution) error {
		if !a.Found {
			return ErrAuthChanged
		}
		return nil
	}
	if err = p.ClusterDrain(ctx, authSessionKey, true, guard); err != nil {
		t.Fatal(err)
	}
	if _, err = p.ClusterHeartbeat(ctx, []ClusterAgent{claim.Agent}); err != nil {
		t.Fatal(err)
	}
	routes, err := q.ClusterRoutes(ctx, "site-a")
	if err != nil || len(routes.Routes) != 0 {
		t.Fatal("heartbeat revived drained route", err)
	}
	if err = p.ObservationReport(ctx, json.RawMessage(`{"requests":3,"version":"test","protocol":"`+ClusterProtocol+`","extension":1}`)); err != nil {
		t.Fatal(err)
	}
	nodes, err := q.ObservationNodesAuthorized(ctx, authSessionKey, guard)
	if err != nil || len(nodes) != 2 || !nodes[0].Draining || !nodes[0].Fresh || nodes[1].Fresh {
		t.Fatal("reporting coverage/drain", nodes, err)
	}
	if err = p.ClusterDrain(ctx, authSessionKey, false, guard); err != nil {
		t.Fatal(err)
	}
	if _, err = p.ClusterHeartbeat(ctx, []ClusterAgent{claim.Agent}); err != nil {
		t.Fatal(err)
	}
	routes, err = q.ClusterRoutes(ctx, "site-a")
	if err != nil || len(routes.Routes) != 1 {
		t.Fatal("undrain did not restore route", err)
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(`UPDATE tunnel_state.cluster_nodes SET expires_at=clock_timestamp()-interval '1 second' WHERE id='a'`); err != nil {
		t.Fatal(err)
	}
	if err = p.ObservationPublish(ctx, []Observation{first}); !errors.Is(err, ErrClusterLease) {
		t.Fatal("expired node published facts", err)
	}
}
