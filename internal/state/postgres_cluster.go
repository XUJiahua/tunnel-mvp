package state

import (
	"context"
	"database/sql"
	"net/url"
	"time"
)

func pgCreateClusterSchema(ctx context.Context, tx *sql.Tx) error {
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS tunnel_state.cluster_nodes(id TEXT PRIMARY KEY,epoch TEXT NOT NULL,endpoint TEXT UNIQUE,fingerprint TEXT NOT NULL,expires_at TIMESTAMPTZ NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS tunnel_state.cluster_agents(agent_key TEXT PRIMARY KEY,site_id TEXT NOT NULL,instance_id TEXT NOT NULL,generation BIGINT NOT NULL CHECK(generation>0),owner_id TEXT,owner_epoch TEXT,credential_hash TEXT NOT NULL,connected_at TIMESTAMPTZ NOT NULL,remote_ip TEXT NOT NULL,version TEXT NOT NULL,policy_version TEXT NOT NULL,channels BIGINT NOT NULL DEFAULT 0 CHECK(channels>=0),ready BOOLEAN NOT NULL DEFAULT false,CHECK((owner_id IS NULL)=(owner_epoch IS NULL)))`,
		`CREATE INDEX IF NOT EXISTS cluster_agents_site ON tunnel_state.cluster_agents(site_id,agent_key)`,
		`CREATE INDEX IF NOT EXISTS cluster_agents_owner ON tunnel_state.cluster_agents(owner_id,owner_epoch)`,
	} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	return nil
}
func OpenPostgresClusterRuntime(ctx context.Context, dir, dsn string, initialize func() (State, error)) (*PostgresRepository, error) {
	lock, err := postgresDirectory(dir, false)
	if err != nil {
		return nil, err
	}
	p, err := openPostgres(ctx, dsn, pgOpenOptions{cluster: true, initialize: initialize})
	if err != nil {
		lock.Close()
		return nil, err
	}
	if err = bindPostgres(ctx, dir, p); err != nil {
		p.Close()
		lock.Close()
		return nil, err
	}
	p.directoryLock = lock
	return p, nil
}
func (p *PostgresRepository) validOwner(owner string) bool {
	if p.allowRecovery {
		return true
	}
	if p.cluster {
		return owner == clusterRuntimeOwner
	}
	if p.runtime {
		return owner == p.owner
	}
	return owner == ""
}
func (p *PostgresRepository) ClusterRegister(ctx context.Context, node ClusterNode) (ClusterSnapshot, error) {
	endpoint, e := url.Parse(node.URL)
	if !p.cluster || p.nodeID != "" || !resourceID.MatchString(node.ID) || len(node.Epoch) != 32 || !authHash(node.Fingerprint) || e != nil || endpoint.Scheme != "https" || endpoint.Hostname() == "" || endpoint.User != nil || endpoint.Path != "" || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return ClusterSnapshot{}, ErrClusterLease
	}
	var out ClusterSnapshot
	err := p.authWrite(ctx, func(ctx context.Context, tx *sql.Tx, m pgMetadata, now time.Time) error {
		var alive bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tunnel_state.cluster_nodes WHERE id=$1 AND expires_at>$2)`, node.ID, now).Scan(&alive); err != nil {
			return err
		}
		if alive {
			return ErrClusterBusy
		}
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM tunnel_state.cluster_nodes WHERE expires_at>$1`, now).Scan(&count); err != nil {
			return err
		}
		if count >= 128 {
			return ErrAuthCapacity
		}
		// An expired address can be reused by a new node identity; old claims retain
		// their generation and still reference the old incarnation.
		if _, err := tx.ExecContext(ctx, `UPDATE tunnel_state.cluster_nodes SET endpoint=NULL WHERE endpoint=$1 AND id<>$2 AND expires_at<=$3`, node.URL, node.ID, now); err != nil {
			return err
		}
		node.Expires = now.Add(ClusterNodeTTL)
		_, err := tx.ExecContext(ctx, `INSERT INTO tunnel_state.cluster_nodes(id,epoch,endpoint,fingerprint,expires_at) VALUES($1,$2,$3,$4,$5) ON CONFLICT(id) DO UPDATE SET epoch=excluded.epoch,endpoint=excluded.endpoint,fingerprint=excluded.fingerprint,expires_at=excluded.expires_at`, node.ID, node.Epoch, node.URL, node.Fingerprint, node.Expires)
		if err != nil {
			return err
		}
		out = ClusterSnapshot{InstanceID: m.InstanceID, Now: now, Self: node}
		return nil
	})
	if err != nil {
		return ClusterSnapshot{}, pgSafeError(err)
	}
	p.mu.Lock()
	p.nodeID, p.nodeEpoch = node.ID, node.Epoch
	p.mu.Unlock()
	return out, nil
}
func pgClusterNodes(ctx context.Context, tx *sql.Tx, now time.Time) ([]ClusterNode, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id,epoch,endpoint,fingerprint,expires_at FROM tunnel_state.cluster_nodes WHERE expires_at>$1 AND endpoint IS NOT NULL ORDER BY id`, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ClusterNode{}
	for rows.Next() {
		var n ClusterNode
		if err = rows.Scan(&n.ID, &n.Epoch, &n.URL, &n.Fingerprint, &n.Expires); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// Runtime ownership deliberately has no FK to replaceable configuration rows.
// Every route joins the current credential/disabled state instead, so config
// deletion and token rotation cannot leave an authoritative orphaned route.
const clusterRoutesSQL = `SELECT c.agent_key,c.site_id,c.instance_id,c.generation,c.owner_id,c.owner_epoch,c.credential_hash,c.connected_at,c.remote_ip,c.version,c.policy_version,c.channels,c.ready,n.id,n.epoch,n.endpoint,n.fingerprint,n.expires_at
 FROM tunnel_state.cluster_agents c JOIN tunnel_state.cluster_nodes n ON n.id=c.owner_id AND n.epoch=c.owner_epoch
 JOIN tunnel_state.sites s ON s.id=c.site_id LEFT JOIN tunnel_state.agents a ON a.id=c.instance_id AND a.site_id=c.site_id
 WHERE n.expires_at>$1 AND n.endpoint IS NOT NULL
 AND ((c.instance_id='' AND coalesce(s.payload->>'legacyDisabled','false')='false' AND c.credential_hash=s.payload->>'tokenHash') OR (c.instance_id<>'' AND a.id IS NOT NULL AND coalesce(a.payload->>'disabled','false')='false' AND c.credential_hash=a.payload->>'tokenHash'))`

func pgClusterRoutes(ctx context.Context, tx *sql.Tx, now time.Time, where string, args ...any) ([]ClusterRoute, error) {
	all := []any{now}
	all = append(all, args...)
	rows, err := tx.QueryContext(ctx, clusterRoutesSQL+where+` ORDER BY c.agent_key`, all...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ClusterRoute{}
	for rows.Next() {
		var r ClusterRoute
		a, n := &r.Agent, &r.Node
		if err = rows.Scan(&a.Key, &a.SiteID, &a.InstanceID, &a.Generation, &a.OwnerID, &a.OwnerEpoch, &a.CredentialHash, &a.ConnectedAt, &a.RemoteIP, &a.Version, &a.PolicyVersion, &a.Channels, &a.Ready, &n.ID, &n.Epoch, &n.URL, &n.Fingerprint, &n.Expires); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
func (p *PostgresRepository) ClusterHeartbeat(ctx context.Context, reports []ClusterAgent) (ClusterSnapshot, error) {
	if !p.cluster || p.nodeID == "" || len(reports) > 4096 {
		return ClusterSnapshot{}, ErrClusterLease
	}
	var out ClusterSnapshot
	err := p.authWrite(ctx, func(ctx context.Context, tx *sql.Tx, m pgMetadata, now time.Time) error {
		out = ClusterSnapshot{InstanceID: m.InstanceID, Now: now}
		row := tx.QueryRowContext(ctx, `UPDATE tunnel_state.cluster_nodes SET expires_at=$1 WHERE id=$2 AND epoch=$3 AND expires_at>$4 RETURNING id,epoch,endpoint,fingerprint,expires_at`, now.Add(ClusterNodeTTL), p.nodeID, p.nodeEpoch, now)
		if err := row.Scan(&out.Self.ID, &out.Self.Epoch, &out.Self.URL, &out.Self.Fingerprint, &out.Self.Expires); err != nil {
			if err == sql.ErrNoRows {
				return ErrClusterLease
			}
			return err
		}
		for _, a := range reports {
			if a.Channels < 0 {
				return ErrClusterLease
			}
			if _, err := tx.ExecContext(ctx, `UPDATE tunnel_state.cluster_agents SET channels=$1,ready=$6 AND NOT coalesce((SELECT draining FROM tunnel_state.node_observations WHERE node_id=$4 AND epoch=$5),false) WHERE agent_key=$2 AND generation=$3 AND owner_id=$4 AND owner_epoch=$5`, a.Channels, a.Key, int64(a.Generation), p.nodeID, p.nodeEpoch, a.Ready); err != nil {
				return err
			}
		}
		var err error
		out.Nodes, err = pgClusterNodes(ctx, tx, now)
		if err != nil {
			return err
		}
		out.Routes, err = pgClusterRoutes(ctx, tx, now, ` AND c.owner_id=$2 AND c.owner_epoch=$3`, p.nodeID, p.nodeEpoch)
		return err
	})
	if err != nil {
		return ClusterSnapshot{}, pgSafeError(err)
	}
	return out, nil
}
func (p *PostgresRepository) ClusterRoutes(ctx context.Context, site string) (ClusterSnapshot, error) {
	if !p.cluster || p.nodeID == "" || !resourceID.MatchString(site) {
		return ClusterSnapshot{}, ErrClusterLease
	}
	var out ClusterSnapshot
	err := p.read(ctx, func(ctx context.Context, tx *sql.Tx, m pgMetadata) error {
		if err := p.authPolicy(ctx, tx); err != nil {
			return err
		}
		now, err := pgAuthNow(ctx, tx)
		if err != nil {
			return err
		}
		out.InstanceID, out.Now = m.InstanceID, now
		out.Routes, err = pgClusterRoutes(ctx, tx, now, ` AND c.site_id=$2 AND c.ready`, site)
		return err
	})
	if err != nil {
		return ClusterSnapshot{}, err
	}
	return out, nil
}
func (p *PostgresRepository) ClusterClaimAgent(ctx context.Context, a ClusterAgent) (ClusterRoute, error) {
	if !p.cluster || p.nodeID == "" || !resourceID.MatchString(a.SiteID) || a.InstanceID != "" && !ValidAgentID(a.InstanceID) || !authHash(a.CredentialHash) || len(a.Version) > 128 || len(a.PolicyVersion) > 64 || len(a.RemoteIP) > 128 {
		return ClusterRoute{}, ErrClusterLease
	}
	a.Key = a.SiteID
	if a.InstanceID != "" {
		a.Key += "~" + a.InstanceID
	}
	var out ClusterRoute
	err := p.authWrite(ctx, func(ctx context.Context, tx *sql.Tx, _ pgMetadata, now time.Time) error {
		var draining bool
		if err := tx.QueryRowContext(ctx, `SELECT coalesce((SELECT draining FROM tunnel_state.node_observations WHERE node_id=$1 AND epoch=$2),false)`, p.nodeID, p.nodeEpoch).Scan(&draining); err != nil {
			return err
		}
		if draining {
			return ErrClusterBusy
		}

		v, found, err := pgOne(ctx, tx, "sites", "id=$1", a.SiteID)
		if err != nil {
			return err
		}
		if !found {
			return ErrAuthChanged
		}
		site := v.(Site)
		if a.InstanceID == "" {
			if site.LegacyDisabled || site.TokenHash != a.CredentialHash {
				return ErrAuthChanged
			}
		} else {
			value, ok, e := pgOne(ctx, tx, "agents", "id=$1", a.InstanceID)
			if e != nil {
				return e
			}
			if !ok {
				return ErrAuthChanged
			}
			agent := value.(AgentRegistration)
			if agent.SiteID != a.SiteID || agent.Disabled || agent.TokenHash != a.CredentialHash {
				return ErrAuthChanged
			}
		}
		var busy bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tunnel_state.cluster_agents c JOIN tunnel_state.cluster_nodes n ON n.id=c.owner_id AND n.epoch=c.owner_epoch WHERE c.agent_key=$1 AND n.expires_at>$2)`, a.Key, now).Scan(&busy); err != nil {
			return err
		}
		if busy {
			return ErrClusterBusy
		}
		var count int
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM tunnel_state.cluster_agents WHERE owner_id=$1 AND owner_epoch=$2`, p.nodeID, p.nodeEpoch).Scan(&count); err != nil {
			return err
		}
		if count >= 4096 {
			return ErrAuthCapacity
		}
		a.OwnerID, a.OwnerEpoch, a.ConnectedAt = p.nodeID, p.nodeEpoch, now
		err = tx.QueryRowContext(ctx, `INSERT INTO tunnel_state.cluster_agents(agent_key,site_id,instance_id,generation,owner_id,owner_epoch,credential_hash,connected_at,remote_ip,version,policy_version) VALUES($1,$2,$3,1,$4,$5,$6,$7,$8,$9,$10) ON CONFLICT(agent_key) DO UPDATE SET site_id=excluded.site_id,instance_id=excluded.instance_id,generation=tunnel_state.cluster_agents.generation+1,owner_id=excluded.owner_id,owner_epoch=excluded.owner_epoch,credential_hash=excluded.credential_hash,connected_at=excluded.connected_at,remote_ip=excluded.remote_ip,version=excluded.version,policy_version=excluded.policy_version,channels=0,ready=false RETURNING generation`, a.Key, a.SiteID, a.InstanceID, a.OwnerID, a.OwnerEpoch, a.CredentialHash, a.ConnectedAt, a.RemoteIP, a.Version, a.PolicyVersion).Scan(&a.Generation)
		if err != nil {
			return err
		}
		var node ClusterNode
		if err = tx.QueryRowContext(ctx, `SELECT id,epoch,endpoint,fingerprint,expires_at FROM tunnel_state.cluster_nodes WHERE id=$1 AND epoch=$2`, p.nodeID, p.nodeEpoch).Scan(&node.ID, &node.Epoch, &node.URL, &node.Fingerprint, &node.Expires); err != nil {
			return err
		}
		out = ClusterRoute{Agent: a, Node: node}
		return nil
	})
	if err != nil {
		return ClusterRoute{}, pgSafeError(err)
	}
	return out, nil
}
func (p *PostgresRepository) ClusterReleaseAgent(ctx context.Context, key string, generation uint64) error {
	if !p.cluster || p.nodeID == "" || generation == 0 || generation > 1<<63-1 {
		return ErrClusterLease
	}
	return p.authWrite(ctx, func(ctx context.Context, tx *sql.Tx, _ pgMetadata, _ time.Time) error {
		_, err := tx.ExecContext(ctx, `UPDATE tunnel_state.cluster_agents SET owner_id=NULL,owner_epoch=NULL,channels=0,ready=false WHERE agent_key=$1 AND generation=$2 AND owner_id=$3 AND owner_epoch=$4`, key, int64(generation), p.nodeID, p.nodeEpoch)
		return err
	})
}
func (p *PostgresRepository) ClusterCloseNode(ctx context.Context) error {
	if !p.cluster || p.nodeID == "" {
		return ErrClusterLease
	}
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
		return err
	}
	meta, err := pgReadMeta(ctx, tx, " FOR UPDATE")
	if err != nil {
		return p.failed(err, false)
	}
	if !p.validOwner(meta.Owner) {
		return ErrClusterLease
	}
	if _, err = tx.ExecContext(ctx, `UPDATE tunnel_state.cluster_nodes SET expires_at=clock_timestamp() WHERE id=$1 AND epoch=$2`, p.nodeID, p.nodeEpoch); err != nil {
		return p.failed(err, false)
	}
	if err = p.validatePhysicalLease(ctx, tx); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return p.failed(err, false)
	}
	// The caller has stopped admission and drained flows before relinquishing the
	// database lease. Further use of this repository must fail, even on restart.
	p.fence()
	return nil
}

func (p *PostgresRepository) validateClusterNode(ctx context.Context, tx *sql.Tx) error {
	if p.nodeID == "" {
		return nil
	}
	var live bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tunnel_state.cluster_nodes WHERE id=$1 AND epoch=$2 AND expires_at>clock_timestamp())`, p.nodeID, p.nodeEpoch).Scan(&live); err != nil {
		return err
	}
	if !live {
		p.fence()
		return ErrClusterLease
	}
	return nil
}

var _ ClusterRepository = (*PostgresRepository)(nil)

func (p *PostgresRepository) ClusterMembership(ctx context.Context) (ClusterSnapshot, error) {
	if !p.cluster || p.nodeID == "" {
		return ClusterSnapshot{}, ErrClusterLease
	}
	var out ClusterSnapshot
	err := p.read(ctx, func(ctx context.Context, tx *sql.Tx, m pgMetadata) error {
		if err := p.authPolicy(ctx, tx); err != nil {
			return err
		}
		now, err := pgAuthNow(ctx, tx)
		if err != nil {
			return err
		}
		out.InstanceID, out.Now = m.InstanceID, now
		out.Nodes, err = pgClusterNodes(ctx, tx, now)
		return err
	})
	if err != nil {
		return ClusterSnapshot{}, err
	}
	return out, nil
}
