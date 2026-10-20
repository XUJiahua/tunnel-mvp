package state

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Additive, independently versioned tables keep the HA-3 base schema and wire
// protocol readable during a rolling upgrade. No FK targets replaceable config.
func pgCreateObservationSchema(ctx context.Context, tx *sql.Tx) error {
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS tunnel_state.observation_meta(singleton BOOLEAN PRIMARY KEY DEFAULT true CHECK(singleton),version INTEGER NOT NULL,epoch TEXT NOT NULL,last_cleanup TIMESTAMPTZ NOT NULL)`,
		`INSERT INTO tunnel_state.observation_meta VALUES(true,1,repeat('0',48),'-infinity') ON CONFLICT DO NOTHING`,
		`CREATE TABLE IF NOT EXISTS tunnel_state.observation_sources(source TEXT PRIMARY KEY,node_id TEXT NOT NULL,legacy_snapshot TEXT NOT NULL DEFAULT '')`,
		`CREATE TABLE IF NOT EXISTS tunnel_state.observations(id BIGSERIAL PRIMARY KEY,event_id TEXT NOT NULL UNIQUE,source TEXT NOT NULL,node_id TEXT NOT NULL,node_epoch TEXT NOT NULL,kind TEXT NOT NULL CHECK(kind IN ('audit','sample')),legacy_snapshot TEXT NOT NULL,organization_id TEXT,service_id TEXT NOT NULL,profile TEXT NOT NULL,occurred_at TIMESTAMPTZ NOT NULL,received_at TIMESTAMPTZ NOT NULL,payload JSONB NOT NULL CHECK(octet_length(payload::text)<1048576))`,
		`CREATE INDEX IF NOT EXISTS observations_org ON tunnel_state.observations(kind,organization_id,id DESC)`,
		`CREATE INDEX IF NOT EXISTS observations_time ON tunnel_state.observations(received_at,id)`,
		`CREATE INDEX IF NOT EXISTS observations_sample ON tunnel_state.observations(service_id,profile,occurred_at DESC,id DESC) WHERE kind='sample'`,
		`CREATE INDEX IF NOT EXISTS observations_source ON tunnel_state.observations(source,legacy_snapshot)`,
		`CREATE TABLE IF NOT EXISTS tunnel_state.node_observations(node_id TEXT PRIMARY KEY,epoch TEXT NOT NULL,reported_at TIMESTAMPTZ NOT NULL,draining BOOLEAN NOT NULL DEFAULT false,payload JSONB NOT NULL)`,
	} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	var version int
	if err := tx.QueryRowContext(ctx, `SELECT version FROM tunnel_state.observation_meta WHERE singleton`).Scan(&version); err != nil {
		return err
	}
	if version != ObservationVersion {
		return ErrObservationSchema
	}
	return nil
}
func observationEpoch() (string, error) {
	var b [24]byte
	_, err := rand.Read(b[:])
	return hex.EncodeToString(b[:]), err
}
func observationHex(s string, n int) bool {
	raw, err := hex.DecodeString(s)
	return err == nil && len(raw) == n && s == strings.ToLower(s)
}
func validObservation(o Observation) bool {
	var object map[string]json.RawMessage
	if json.Unmarshal(o.Payload, &object) != nil || object == nil {
		return false
	}

	if !observationHex(o.ID, 32) || !observationHex(o.Source, 24) || !ValidClusterNodeID(o.NodeID) || o.Epoch != "" && !observationHex(o.Epoch, 16) || !o.At.IsZero() && o.At.Year() < 1970 || o.At.IsZero() || len(o.Payload) == 0 || len(o.Payload) > 128<<10 || !json.Valid(o.Payload) {
		return false
	}
	if o.LegacySnapshot != "" && !observationHex(o.LegacySnapshot, 24) {
		return false
	}
	if o.OrganizationID != nil && *o.OrganizationID != "" && !resourceID.MatchString(*o.OrganizationID) {
		return false
	}
	switch o.Kind {
	case "audit":
		return o.ServiceID == "" && o.Profile == ""
	case "sample":
		return resourceID.MatchString(o.ServiceID) && authHash(o.Profile) && o.LegacySnapshot == ""
	case "legacy_end":
		return o.LegacySnapshot != "" && o.ServiceID == "" && o.Profile == ""
	}
	return false
}
func (p *PostgresRepository) ObservationPublish(ctx context.Context, batch []Observation) error {
	if !p.cluster || p.nodeID == "" || len(batch) == 0 || len(batch) > 100 {
		return ErrClusterLease
	}
	bytes := 0
	for _, o := range batch {
		bytes += len(o.Payload)
		if !validObservation(o) {
			return fmt.Errorf("invalid observation batch")
		}
	}
	if bytes > 2<<20 {
		return fmt.Errorf("observation batch too large")
	}
	rows := make([]pgObservationRow, 0, len(batch))
	for _, o := range batch {
		if o.NodeID != p.nodeID {
			return ErrClusterLease
		}
		if o.Kind == "legacy_end" {
			continue
		}
		var object map[string]json.RawMessage
		if json.Unmarshal(o.Payload, &object) != nil || object == nil {
			return fmt.Errorf("observation payload must be an object")
		}
		for key, value := range map[string]string{"eventID": o.ID, "nodeID": o.NodeID, "nodeEpoch": o.Epoch} {
			object[key], _ = json.Marshal(value)
		}
		raw, err := json.Marshal(object)
		if err != nil {
			return err
		}
		rows = append(rows, pgObservationRow{o.ID, o.Source, o.NodeID, o.Epoch, o.Kind, o.LegacySnapshot, o.OrganizationID, o.ServiceID, o.Profile, o.At, raw})
	}
	raw, err := json.Marshal(rows)
	if err != nil {
		return err
	}
	return p.observationWrite(ctx, func(ctx context.Context, tx *sql.Tx, _ pgMetadata, now time.Time) error {
		// All writers lock observation_meta, independently of authorization/config
		// writes. This also orders commit IDs for stable descending pagination.
		seen := map[string]bool{}
		for _, o := range batch {
			if seen[o.Source] {
				continue
			}
			seen[o.Source] = true
			if _, err := tx.ExecContext(ctx, `INSERT INTO tunnel_state.observation_sources(source,node_id) VALUES($1,$2) ON CONFLICT DO NOTHING`, o.Source, o.NodeID); err != nil {
				return err
			}
			var owner string
			if err := tx.QueryRowContext(ctx, `SELECT node_id FROM tunnel_state.observation_sources WHERE source=$1`, o.Source).Scan(&owner); err != nil {
				return err
			}
			if owner != o.NodeID {
				return fmt.Errorf("observation source identity conflict")
			}
		}
		if len(rows) > 0 {
			if _, err := tx.ExecContext(ctx, `WITH input AS (`+pgObservationInput+`) INSERT INTO tunnel_state.observations(event_id,source,node_id,node_epoch,kind,legacy_snapshot,organization_id,service_id,profile,occurred_at,received_at,payload) SELECT event_id,source,node_id,node_epoch,kind,legacy_snapshot,organization_id,service_id,profile,occurred_at,$2,payload FROM input ON CONFLICT(event_id) DO NOTHING`, string(raw), now); err != nil {
				return err
			}
			var same bool
			if err := tx.QueryRowContext(ctx, `WITH input AS (`+pgObservationInput+`) SELECT NOT EXISTS(SELECT 1 FROM input i LEFT JOIN tunnel_state.observations o USING(event_id) WHERE o.event_id IS NULL OR ROW(o.source,o.node_id,o.node_epoch,o.kind,o.legacy_snapshot,o.organization_id,o.service_id,o.profile,o.occurred_at,o.payload) IS DISTINCT FROM ROW(i.source,i.node_id,i.node_epoch,i.kind,i.legacy_snapshot,i.organization_id,i.service_id,i.profile,i.occurred_at,i.payload))`, string(raw)).Scan(&same); err != nil {
				return err
			}
			if !same {
				return fmt.Errorf("observation ID content conflict")
			}
		}
		for _, o := range batch {
			if o.Kind != "legacy_end" {
				continue
			}
			result, err := tx.ExecContext(ctx, `UPDATE tunnel_state.observation_sources SET legacy_snapshot=$2 WHERE source=$1 AND legacy_snapshot<>$2`, o.Source, o.LegacySnapshot)
			if err != nil {
				return err
			}
			n, err := result.RowsAffected()
			if err != nil {
				return err
			}
			if n > 0 {
				epoch, e := observationEpoch()
				if e != nil {
					return e
				}
				if _, err = tx.ExecContext(ctx, `UPDATE tunnel_state.observation_meta SET epoch=$1 WHERE singleton`, epoch); err != nil {
					return err
				}
			}
		}
		return pgObservationCleanup(ctx, tx, now)
	})
}

type pgObservationRow struct {
	ID       string          `json:"event_id"`
	Source   string          `json:"source"`
	Node     string          `json:"node_id"`
	Epoch    string          `json:"node_epoch"`
	Kind     string          `json:"kind"`
	Snapshot string          `json:"legacy_snapshot"`
	Org      *string         `json:"organization_id"`
	Service  string          `json:"service_id"`
	Profile  string          `json:"profile"`
	At       time.Time       `json:"occurred_at"`
	Payload  json.RawMessage `json:"payload"`
}

const pgObservationInput = `SELECT * FROM jsonb_to_recordset($1::jsonb) AS i(event_id TEXT,source TEXT,node_id TEXT,node_epoch TEXT,kind TEXT,legacy_snapshot TEXT,organization_id TEXT,service_id TEXT,profile TEXT,occurred_at TIMESTAMPTZ,payload JSONB)`

func (p *PostgresRepository) observationWrite(ctx context.Context, fn func(context.Context, *sql.Tx, pgMetadata, time.Time) error) error {
	ctx, cancel := context.WithTimeout(ctx, authOperationTimeout)
	defer cancel()
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.check(); err != nil {
		return err
	}
	tx, err := p.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return p.failed(err, false)
	}
	defer tx.Rollback()
	if err = p.validateLease(ctx, tx); err != nil {
		return p.failed(err, false)
	}
	meta, err := pgReadMeta(ctx, tx, "")
	if err != nil {
		return p.failed(err, false)
	}
	if !p.validOwner(meta.Owner) {
		return ErrClusterLease
	}
	if err = p.authPolicy(ctx, tx); err != nil {
		return err
	}
	var version int
	if err = tx.QueryRowContext(ctx, `SELECT version FROM tunnel_state.observation_meta WHERE singleton FOR UPDATE`).Scan(&version); err != nil {
		return p.failed(err, false)
	}
	if version != ObservationVersion {
		return ErrObservationSchema
	}
	now, err := pgAuthNow(ctx, tx)
	if err != nil {
		return p.failed(err, false)
	}
	if err = fn(ctx, tx, meta, now); err != nil {
		return err
	}
	if err = p.check(); err != nil {
		return err
	}
	if err = p.validateLease(ctx, tx); err != nil {
		return p.failed(err, false)
	}
	return p.failed(tx.Commit(), false)
}

func pgObservationCleanup(ctx context.Context, tx *sql.Tx, now time.Time) error {
	var due bool
	if err := tx.QueryRowContext(ctx, `SELECT last_cleanup<$1 FROM tunnel_state.observation_meta WHERE singleton`, now.Add(-time.Minute)).Scan(&due); err != nil {
		return err
	}
	if !due {
		return nil
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM tunnel_state.observations WHERE kind='audit' AND (received_at<$1 OR id IN (SELECT id FROM tunnel_state.observations WHERE kind='audit' ORDER BY id DESC OFFSET $2))`, now.Add(-ObservationRetention), ObservationAuditLimit)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM tunnel_state.observations WHERE kind='sample' AND (occurred_at<$1 OR id IN (SELECT id FROM (SELECT id,row_number() OVER(PARTITION BY service_id ORDER BY occurred_at DESC,id DESC) AS position FROM tunnel_state.observations WHERE kind='sample') s WHERE position>$2) OR id IN (SELECT id FROM tunnel_state.observations WHERE kind='sample' ORDER BY occurred_at DESC,id DESC OFFSET $3))`, now.Add(-ObservationRetention), ObservationSamplesPerService, ObservationSampleLimit); err != nil {
		return err
	}
	if n > 0 {
		epoch, e := observationEpoch()
		if e != nil {
			return e
		}
		if _, err = tx.ExecContext(ctx, `UPDATE tunnel_state.observation_meta SET epoch=$1 WHERE singleton`, epoch); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `UPDATE tunnel_state.observation_meta SET last_cleanup=$1 WHERE singleton`, now)
	return err
}
func (p *PostgresRepository) ObservationReport(ctx context.Context, raw json.RawMessage) error {
	var report struct {
		Version   string `json:"version"`
		Protocol  string `json:"protocol"`
		Extension int    `json:"extension"`
	}
	if json.Unmarshal(raw, &report) != nil || report.Version == "" || len(report.Version) > 128 || report.Protocol != ClusterProtocol || report.Extension != ObservationVersion {
		return fmt.Errorf("invalid observation node report")
	}

	if !p.cluster || p.nodeID == "" || len(raw) > 16<<10 || !json.Valid(raw) {
		return ErrClusterLease
	}
	return p.authWrite(ctx, func(ctx context.Context, tx *sql.Tx, _ pgMetadata, now time.Time) error {
		var draining bool
		if err := tx.QueryRowContext(ctx, `SELECT coalesce((SELECT draining FROM tunnel_state.node_observations WHERE node_id=$1 AND epoch=$2),false)`, p.nodeID, p.nodeEpoch).Scan(&draining); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO tunnel_state.node_observations(node_id,epoch,reported_at,draining,payload) VALUES($1,$2,$3,$4,$5) ON CONFLICT(node_id) DO UPDATE SET epoch=excluded.epoch,reported_at=excluded.reported_at,draining=excluded.draining,payload=excluded.payload`, p.nodeID, p.nodeEpoch, now, draining, string(raw))
		return err
	})
}
func (p *PostgresRepository) ClusterDrain(ctx context.Context, key string, value bool, guard func(AuthResolution) error) error {
	if !p.cluster || p.nodeID == "" {
		return ErrClusterLease
	}
	return p.authWrite(ctx, func(ctx context.Context, tx *sql.Tx, _ pgMetadata, now time.Time) error {
		actor, err := pgObservationActor(ctx, tx, key, now)
		if err != nil {
			return err
		}
		if err = guard(actor); err != nil {
			return err
		}

		if _, err := tx.ExecContext(ctx, `INSERT INTO tunnel_state.node_observations(node_id,epoch,reported_at,draining,payload) VALUES($1,$2,$3,$4,'{}') ON CONFLICT(node_id) DO UPDATE SET epoch=excluded.epoch,draining=excluded.draining`, p.nodeID, p.nodeEpoch, now, value); err != nil {
			return err
		}
		if value {
			_, err := tx.ExecContext(ctx, `UPDATE tunnel_state.cluster_agents SET ready=false WHERE owner_id=$1 AND owner_epoch=$2`, p.nodeID, p.nodeEpoch)
			return err
		}
		return nil
	})
}
func pgObservationNodes(ctx context.Context, tx *sql.Tx, out *[]ObservationNode) error {
	rows, err := tx.QueryContext(ctx, `SELECT n.id,n.epoch,n.expires_at,r.reported_at,r.payload,n.expires_at>clock_timestamp(),coalesce(r.reported_at>clock_timestamp()-interval '10 seconds' AND r.payload->>'extension'='1' AND r.payload->>'protocol'=$1 AND coalesce(r.payload->>'version','')<>'',false),coalesce(r.draining,false) FROM tunnel_state.cluster_nodes n LEFT JOIN tunnel_state.node_observations r ON r.node_id=n.id AND r.epoch=n.epoch ORDER BY n.id`, ClusterProtocol)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var n ObservationNode
		var at sql.NullTime
		var raw []byte
		if err = rows.Scan(&n.NodeID, &n.Epoch, &n.Expires, &at, &raw, &n.Alive, &n.Fresh, &n.Draining); err != nil {
			return err
		}
		if at.Valid {
			n.ReportedAt = &at.Time
			n.Report = json.RawMessage(raw)
		}
		*out = append(*out, n)
	}
	return rows.Err()
}
func (p *PostgresRepository) ObservationNodes(ctx context.Context) ([]ObservationNode, error) {
	out := []ObservationNode{}
	err := p.read(ctx, func(ctx context.Context, tx *sql.Tx, _ pgMetadata) error {
		if err := p.authPolicy(ctx, tx); err != nil {
			return err
		}
		return pgObservationNodes(ctx, tx, &out)
	})
	return out, err
}
func (p *PostgresRepository) ObservationNodesAuthorized(ctx context.Context, key string, guard func(AuthResolution) error) ([]ObservationNode, error) {
	out := []ObservationNode{}
	err := p.observationRead(ctx, key, func(ctx context.Context, tx *sql.Tx, a AuthResolution) error {
		if err := guard(a); err != nil {
			return err
		}
		return pgObservationNodes(ctx, tx, &out)
	})
	return out, err
}
func pgObservationActor(ctx context.Context, tx *sql.Tx, key string, now time.Time) (AuthResolution, error) {
	out := AuthResolution{Now: now}
	if !authHash(key) {
		return out, ErrAuthChanged
	}
	data, err := pgAuthLoad(ctx, tx, `kind='session' AND key=$1 AND expires_at>$2`, key, now)
	if err != nil {
		return out, err
	}
	session, ok := data.Sessions[key]
	if !ok {
		return out, ErrAuthChanged
	}
	if session.IdentityEpoch != "" {
		status, found, e := pgIdentityStatus(ctx, tx, session.UserID)
		if e != nil {
			return out, e
		}
		if !found || !AuthIdentityAllowed(status, session, now) {
			return out, ErrAuthChanged
		}
		out.Identity = status
	}
	projection, found, err := pgUserContext(ctx, tx, session.UserID)
	if err != nil {
		return out, err
	}
	if !found || projection.Users[0].Disabled {
		return out, ErrAuthChanged
	}
	out.Session, out.State, out.Found = session, projection, true
	return out, nil
}
func (p *PostgresRepository) observationRead(ctx context.Context, key string, fn func(context.Context, *sql.Tx, AuthResolution) error) error {
	return p.read(ctx, func(ctx context.Context, tx *sql.Tx, _ pgMetadata) error {
		if err := p.authPolicy(ctx, tx); err != nil {
			return err
		}
		now, err := pgAuthNow(ctx, tx)
		if err != nil {
			return err
		}
		actor, err := pgObservationActor(ctx, tx, key, now)
		if err != nil {
			return err
		}
		return fn(ctx, tx, actor)
	})
}
func (p *PostgresRepository) ObservationAuthorize(ctx context.Context, key string, guard func(AuthResolution) error) error {
	return p.observationRead(ctx, key, func(_ context.Context, _ *sql.Tx, actor AuthResolution) error { return guard(actor) })
}
func (p *PostgresRepository) ObservationAudit(ctx context.Context, key string, f ObservationAuditFilter, guard func(AuthResolution) (ObservationScope, error)) (ObservationAuditPage, error) {
	out := ObservationAuditPage{Entries: []json.RawMessage{}}
	if f.Limit < 1 || f.Limit > 500 || f.Before < 0 {
		return out, fmt.Errorf("invalid observation page")
	}
	err := p.observationRead(ctx, key, func(ctx context.Context, tx *sql.Tx, actor AuthResolution) error {
		scope, err := guard(actor)
		if err != nil {
			return err
		}
		if err = tx.QueryRowContext(ctx, `SELECT epoch FROM tunnel_state.observation_meta WHERE singleton`).Scan(&out.Epoch); err != nil {
			return err
		}
		if f.Epoch != "" && f.Epoch != out.Epoch {
			return ErrObservationCursor
		}
		q := `SELECT o.id,o.payload FROM tunnel_state.observations o WHERE o.kind='audit' AND (o.legacy_snapshot='' OR o.legacy_snapshot=(SELECT legacy_snapshot FROM tunnel_state.observation_sources s WHERE s.source=o.source))`
		args := []any{}
		add := func(condition string, value any) {
			args = append(args, value)
			q += " AND " + fmt.Sprintf(condition, len(args))
		}
		if !scope.All {
			add("o.organization_id=$%d", scope.OrganizationID)
		}
		for _, v := range []struct{ condition, value string }{{"o.payload->>'event'=$%d", f.Event}, {"o.payload->>'user'=$%d", f.User}, {"o.payload->>'siteID'=$%d", f.Site}, {"o.payload->>'resourceID'=$%d", f.Resource}, {"o.node_id=$%d", f.Node}} {
			if v.value != "" {
				if len(v.value) > 100 {
					return fmt.Errorf("invalid observation filter")
				}
				add(v.condition, v.value)
			}
		}
		if !f.Since.IsZero() {
			add("o.occurred_at>=$%d", f.Since)
		}
		if f.Before > 0 {
			add("o.id<$%d", f.Before)
		}
		args = append(args, f.Limit+1)
		q += fmt.Sprintf(" ORDER BY o.id DESC LIMIT $%d", len(args))
		rows, err := tx.QueryContext(ctx, q, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		bytes := 0
		last := int64(0)
		for rows.Next() {
			var id int64
			var raw []byte
			if err = rows.Scan(&id, &raw); err != nil {
				return err
			}
			if len(out.Entries) == f.Limit || len(out.Entries) > 0 && bytes+len(raw) > 2<<20 {
				out.Next = last
				break
			}
			out.Entries = append(out.Entries, json.RawMessage(raw))
			last = id
			bytes += len(raw)
		}
		return rows.Err()
	})
	return out, err
}
func (p *PostgresRepository) ObservationSamples(ctx context.Context, key, service string, since time.Time, limit int, guard func(AuthResolution, Service) (string, error)) ([]json.RawMessage, error) {
	out := []json.RawMessage{}
	if !resourceID.MatchString(service) || limit < 1 || limit > ObservationSamplesPerService {
		return out, fmt.Errorf("invalid observation samples query")
	}
	err := p.observationRead(ctx, key, func(ctx context.Context, tx *sql.Tx, actor AuthResolution) error {
		value, found, err := pgOne(ctx, tx, "services", "id=$1", service)
		if err != nil {
			return err
		}
		if !found {
			return ErrAuthChanged
		}
		profile, err := guard(actor, value.(Service))
		if err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT payload FROM tunnel_state.observations WHERE kind='sample' AND service_id=$1 AND profile=$2 AND occurred_at>=$3 ORDER BY occurred_at DESC,id DESC LIMIT $4`, service, profile, since, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var raw []byte
			if err = rows.Scan(&raw); err != nil {
				return err
			}
			out = append(out, json.RawMessage(raw))
		}
		return rows.Err()
	})
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, err
}

// Select current service profiles and samples under one snapshot. The callback
// applies current authorization before any health payload leaves the repository.
func pgObservationLatest(ctx context.Context, tx *sql.Tx, profile func(Service) (string, bool, error)) ([]ObservationHealth, error) {
	out := []ObservationHealth{}
	rows, err := tx.QueryContext(ctx, `SELECT id,payload FROM tunnel_state.services WHERE id<>'' ORDER BY position`)
	if err != nil {
		return nil, err
	}
	services := map[string]Service{}
	wanted := []map[string]string{}
	for rows.Next() {
		var id string
		var raw []byte
		if err = rows.Scan(&id, &raw); err != nil {
			break
		}
		var value any
		value, err = pgPayload("services", id, raw)
		if err != nil {
			break
		}
		svc := value.(Service)
		var hash string
		var ok bool
		hash, ok, err = profile(svc)
		if err != nil {
			break
		}
		if ok {
			services[id] = svc
			wanted = append(wanted, map[string]string{"id": id, "profile": hash})
		}
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return nil, err
	}
	if len(wanted) == 0 {
		return out, nil
	}
	raw, err := json.Marshal(wanted)
	if err != nil {
		return nil, err
	}
	rows, err = tx.QueryContext(ctx, `SELECT s.id,o.payload FROM jsonb_to_recordset($1::jsonb) AS s(id TEXT,profile TEXT) JOIN LATERAL (SELECT payload FROM tunnel_state.observations WHERE kind='sample' AND service_id=s.id AND profile=s.profile AND occurred_at>=clock_timestamp()-interval '7 days' ORDER BY occurred_at DESC,id DESC LIMIT 1) o ON true ORDER BY s.id`, string(raw))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var raw []byte
		if err = rows.Scan(&id, &raw); err != nil {
			return nil, err
		}
		out = append(out, ObservationHealth{services[id], json.RawMessage(raw)})
	}
	return out, rows.Err()
}
func (p *PostgresRepository) ObservationLatest(ctx context.Context, key string, guard func(AuthResolution, Service) (string, bool, error)) ([]ObservationHealth, error) {
	var out []ObservationHealth
	err := p.observationRead(ctx, key, func(ctx context.Context, tx *sql.Tx, a AuthResolution) error {
		var err error
		out, err = pgObservationLatest(ctx, tx, func(s Service) (string, bool, error) { return guard(a, s) })
		return err
	})
	return out, err
}

// Metrics bearer tokens already authorize the global operational projection.
func (p *PostgresRepository) ObservationLatestInternal(ctx context.Context, profile func(Service) string) ([]ObservationHealth, error) {
	var out []ObservationHealth
	err := p.read(ctx, func(ctx context.Context, tx *sql.Tx, _ pgMetadata) error {
		if err := p.authPolicy(ctx, tx); err != nil {
			return err
		}
		var err error
		out, err = pgObservationLatest(ctx, tx, func(s Service) (string, bool, error) { return profile(s), true, nil })
		return err
	})
	return out, err
}
