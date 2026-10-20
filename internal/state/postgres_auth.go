package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"reflect"
	"time"

	"github.com/lib/pq"
)

const authOperationTimeout = 3 * time.Second

func CloneAuthData(a AuthData) (AuthData, error) {
	raw, err := json.Marshal(a)
	if err != nil {
		return AuthData{}, err
	}
	var out AuthData
	if err = decodeStrict(raw, &out); err != nil {
		return AuthData{}, err
	}
	out.normalize()
	return out, nil
}
func (p *PostgresRepository) authWrite(ctx context.Context, fn func(context.Context, *sql.Tx, pgMetadata, time.Time) error) error {
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
	m, err := pgReadMeta(ctx, tx, " FOR UPDATE")
	if err != nil {
		return p.failed(err, false)
	}
	if p.runtime && m.Owner != p.owner || !p.runtime && !p.allowRecovery && m.Owner != "" {
		p.fence()
		return ErrPostgresUnavailable
	}
	if err = p.authPolicy(ctx, tx); err != nil {
		return p.failed(err, false)
	}
	now, err := pgAuthNow(ctx, tx)
	if err != nil {
		return p.failed(err, false)
	}
	if err = fn(ctx, tx, m, now); err != nil {
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
func (p *PostgresRepository) AuthSnapshot(ctx context.Context, withConfig bool) (AuthSnapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, authOperationTimeout)
	defer cancel()
	var out AuthSnapshot
	err := p.read(ctx, func(ctx context.Context, tx *sql.Tx, m pgMetadata) error {
		if err := p.authPolicy(ctx, tx); err != nil {
			return err
		}
		var err error
		out.Now, err = pgAuthNow(ctx, tx)
		if err != nil {
			return err
		}
		out.Data, err = pgAuthLoad(ctx, tx, "expires_at>$1", out.Now)
		if err != nil {
			return err
		}
		out.Data.Clean(out.Now)
		if err = validateAuth(out.Data, out.Now); err != nil {
			return err
		}
		if withConfig {
			out.State, err = pgReadState(ctx, tx)
			if err != nil {
				return err
			}
		}
		out.ConfigRevision = m.Revision
		out.EventID, err = pgAuthWatermark(ctx, tx)
		return err
	})
	if err != nil {
		return AuthSnapshot{}, err
	}
	return out, nil
}
func (p *PostgresRepository) AuthUpdate(ctx context.Context, withConfig bool, fn func(*AuthData, State, time.Time) error) error {
	return p.authWrite(ctx, func(ctx context.Context, tx *sql.Tx, m pgMetadata, now time.Time) error {
		if err := pgAuthCleanup(ctx, tx, now); err != nil {
			return p.failed(err, false)
		}
		data, err := pgAuthLoad(ctx, tx, "true")
		if err != nil {
			return p.failed(err, false)
		}
		data.Clean(now)
		before, err := CloneAuthData(data)
		if err != nil {
			return err
		}
		var config State
		if withConfig {
			config, err = pgReadState(ctx, tx)
			if err != nil {
				return p.failed(err, false)
			}
		}
		if err = fn(&data, config, now); err != nil {
			return err
		}
		data.Clean(now)
		if err = validateAuth(data, now); err != nil {
			return err
		}
		// Session/grant creation must use the locked configuration in this callback.
		if !reflect.DeepEqual(before.Identities, data.Identities) {
			return fmt.Errorf("identity status requires a fenced identity check")
		}
		if !withConfig {
			for k, v := range data.Sessions {
				old, ok := before.Sessions[k]
				if !ok || old.UserID != v.UserID || old.Method != v.Method || old.Issuer != v.Issuer || old.Subject != v.Subject || old.SID != v.SID || old.ID != v.ID || !old.Expires.Equal(v.Expires) {
					return fmt.Errorf("session issuance requires transactional configuration")
				}
			}
			for _, set := range []struct{ Old, Next map[string]AuthGrant }{{before.Tickets, data.Tickets}, {before.Access, data.Access}} {
				for k, v := range set.Next {
					if old, ok := set.Old[k]; !ok || old != v {
						return fmt.Errorf("grant issuance requires transactional configuration")
					}
				}
			}
		}
		if err = pgAuthSave(ctx, tx, before, data); err != nil {
			return p.failed(err, false)
		}
		if authRevoked(before, data) {
			if err = pgAuthEvent(ctx, tx, m.Revision, "revoke"); err != nil {
				return p.failed(err, false)
			}
		}
		return nil
	})
}
func (p *PostgresRepository) AuthRateLimit(ctx context.Context, key string, maximum int, period time.Duration) (bool, error) {
	if key == "" || len(key) > 256 || maximum < 1 || maximum > 4096 || period <= 0 || period > time.Hour {
		return false, fmt.Errorf("invalid shared rate limit")
	}
	allowed := false
	err := p.authWrite(ctx, func(ctx context.Context, tx *sql.Tx, _ pgMetadata, now time.Time) error {
		if err := pgAuthCleanup(ctx, tx, now); err != nil {
			return p.failed(err, false)
		}
		data, err := pgAuthLoad(ctx, tx, "kind='limit' AND key=$1", key)
		if err != nil {
			return p.failed(err, false)
		}
		limit, exists := data.Limits[key]
		if !exists {
			var count int
			if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM tunnel_state.auth_records WHERE kind='limit'`).Scan(&count); err != nil {
				return p.failed(err, false)
			}
			if count >= 256 {
				return nil
			}
			limit = AuthLimit{Until: now.Add(period)}
		}
		if limit.Count >= maximum {
			return nil
		}
		limit.Count++
		raw, err := json.Marshal(limit)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO tunnel_state.auth_records(kind,key,payload,expires_at) VALUES('limit',$1,$2,$3) ON CONFLICT(kind,key) DO UPDATE SET payload=excluded.payload,expires_at=excluded.expires_at`, key, string(raw), limit.Until.UTC().Truncate(time.Microsecond))
		if err != nil {
			return p.failed(err, false)
		}
		allowed = true
		return nil
	})
	return allowed, err
}
func (p *PostgresRepository) AuthBindPolicy(ctx context.Context, policy string) error {
	if !authHash(policy) {
		return fmt.Errorf("invalid authentication policy identity")
	}
	ctx, cancel := context.WithTimeout(ctx, authOperationTimeout)
	defer cancel()
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.check(); err != nil {
		return err
	}
	if p.authPolicyHash != "" && p.authPolicyHash != policy {
		return ErrAuthPolicy
	}
	tx, err := p.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return p.failed(err, false)
	}
	defer tx.Rollback()
	if err = p.validateLease(ctx, tx); err != nil {
		return p.failed(err, false)
	}
	m, err := pgReadMeta(ctx, tx, " FOR UPDATE")
	if err != nil {
		return p.failed(err, false)
	}
	if p.runtime && m.Owner != p.owner || !p.runtime && !p.allowRecovery && m.Owner != "" {
		p.fence()
		return ErrPostgresUnavailable
	}
	var old string
	if err = tx.QueryRowContext(ctx, `SELECT policy_hash FROM tunnel_state.auth_policy WHERE singleton FOR UPDATE`).Scan(&old); err != nil {
		return p.failed(err, false)
	}
	if old != "" && old != policy && !p.runtime {
		return ErrAuthPolicy
	}
	if old != policy {
		if _, err = tx.ExecContext(ctx, `DELETE FROM tunnel_state.auth_records`); err != nil {
			return p.failed(err, false)
		}
		if _, err = tx.ExecContext(ctx, `UPDATE tunnel_state.auth_policy SET policy_hash=$1 WHERE singleton`, policy); err != nil {
			return p.failed(err, false)
		}
		if err = pgAuthEvent(ctx, tx, m.Revision, "policy"); err != nil {
			return p.failed(err, false)
		}
	}
	if err = p.check(); err != nil {
		return err
	}
	if err = p.validateLease(ctx, tx); err != nil {
		return p.failed(err, false)
	}
	if err = tx.Commit(); err != nil {
		return p.failed(err, false)
	}
	p.authPolicyHash = policy
	return nil
}
func (p *PostgresRepository) AuthResolve(ctx context.Context, kind, key string) (AuthResolution, error) {
	if (kind != "session" && kind != "access") || !authHash(key) {
		return AuthResolution{}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, authOperationTimeout)
	defer cancel()
	var out AuthResolution
	err := p.read(ctx, func(ctx context.Context, tx *sql.Tx, meta pgMetadata) error {
		out.ConfigRevision = meta.Revision
		if err := p.authPolicy(ctx, tx); err != nil {
			return err
		}
		var err error
		out.Now, err = pgAuthNow(ctx, tx)
		if err != nil {
			return err
		}
		where := `kind='session' AND key=$1 AND expires_at>$2`
		if kind == "access" {
			where = `((kind='access' AND key=$1) OR (kind='session' AND key=(SELECT parent_key FROM tunnel_state.auth_records WHERE kind='access' AND key=$1))) AND expires_at>$2`
		}
		data, err := pgAuthLoad(ctx, tx, where, key, out.Now)
		if err != nil {
			return err
		}
		sessionKey := key
		if kind == "access" {
			var exists bool
			out.Grant, exists = data.Access[key]
			if !exists {
				return nil
			}
			sessionKey = out.Grant.SessionKey
		}
		var exists bool
		out.Session, exists = data.Sessions[sessionKey]
		if !exists {
			return nil
		}

		if out.Session.IdentityEpoch != "" {
			status, found, e := pgIdentityStatus(ctx, tx, out.Session.UserID)
			if e != nil {
				return e
			}
			if found {
				data.Identities[status.UserID] = status
				out.Identity = status
			}
			data.Clean(out.Now)
			if _, exists = data.Sessions[sessionKey]; !exists {
				return nil
			}
		}
		if err = validateAuth(data, out.Now); err != nil {
			return err
		}
		out.State, exists, err = pgUserContext(ctx, tx, out.Session.UserID)
		if err != nil || !exists {
			return err
		}
		if out.State.Users[0].Disabled {
			return nil
		}
		if kind == "access" {
			v, found, e := pgOne(ctx, tx, "services", "id=$1", out.Grant.ServiceID)
			if e != nil {
				return e
			}
			if !found {
				return nil
			}
			out.State.Services = []Service{v.(Service)}
		}
		out.Found = true
		return nil
	})
	if err != nil {
		return AuthResolution{}, err
	}
	if out.Found && out.Now.Sub(out.Session.LastSeen) >= time.Minute {
		sessionKey := key
		if kind == "access" {
			sessionKey = out.Grant.SessionKey
		}
		if err = p.authTouch(ctx, sessionKey, out.Session.ID); err != nil {
			return AuthResolution{}, err
		}
	}
	return out, nil
}
func (p *PostgresRepository) authTouch(ctx context.Context, key, id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.check(); err != nil {
		return err
	}
	// This is only display metadata: a conditional UPDATE cannot recreate a
	// revoked session and does not need the security-writer revision lock.
	_, err := p.db.ExecContext(ctx, `UPDATE tunnel_state.auth_records SET payload=jsonb_set(payload,'{LastSeen}',to_jsonb(clock_timestamp())) WHERE kind='session' AND key=$1 AND public_id=$2 AND expires_at>clock_timestamp() AND (payload->>'LastSeen')::timestamptz<clock_timestamp()-interval '1 minute'`, key, id)
	return p.failed(err, false)
}
func (p *PostgresRepository) AuthSnapshotForKeys(ctx context.Context, accessKeys, sessionKeys, serviceIDs []string) (AuthSnapshot, error) {
	if len(accessKeys) > 8192 || len(sessionKeys) > 4096 || len(serviceIDs) > 8192 {
		return AuthSnapshot{}, ErrAuthCapacity
	}
	ctx, cancel := context.WithTimeout(ctx, authOperationTimeout)
	defer cancel()
	var out AuthSnapshot
	err := p.read(ctx, func(ctx context.Context, tx *sql.Tx, m pgMetadata) error {
		if err := p.authPolicy(ctx, tx); err != nil {
			return err
		}
		var err error
		out.Now, err = pgAuthNow(ctx, tx)
		if err != nil {
			return err
		}
		out.Data, err = pgAuthLoad(ctx, tx, `expires_at>$3 AND ((kind='access' AND key=ANY($1::text[])) OR (kind='session' AND (key=ANY($2::text[]) OR key IN (SELECT parent_key FROM tunnel_state.auth_records WHERE kind='access' AND key=ANY($1::text[])))))`, pq.Array(append([]string{}, accessKeys...)), pq.Array(append([]string{}, sessionKeys...)), out.Now)
		if err != nil {
			return err
		}
		for _, session := range out.Data.Sessions {
			if session.IdentityEpoch != "" {
				v, found, e := pgIdentityStatus(ctx, tx, session.UserID)
				if e != nil {
					return e
				}
				if found {
					out.Data.Identities[v.UserID] = v
				}
			}
		}
		out.Data.Clean(out.Now)
		users := map[string]bool{}
		roles := map[string]bool{}
		groups := map[string]bool{}
		for _, s := range out.Data.Sessions {
			if users[s.UserID] {
				continue
			}
			projection, found, e := pgUserContext(ctx, tx, s.UserID)
			if e != nil {
				return e
			}
			if !found {
				continue
			}
			users[s.UserID] = true
			out.State.Users = append(out.State.Users, projection.Users...)
			for _, v := range projection.Roles {
				if !roles[v.ID] {
					roles[v.ID] = true
					out.State.Roles = append(out.State.Roles, v)
				}
			}
			for _, v := range projection.Groups {
				if !groups[v.ID] {
					groups[v.ID] = true
					out.State.Groups = append(out.State.Groups, v)
				}
			}
		}
		services := map[string]bool{}
		for _, g := range out.Data.Access {
			services[g.ServiceID] = true
		}
		for _, id := range serviceIDs {
			services[id] = true
		}
		for id := range services {
			v, found, e := pgOne(ctx, tx, "services", "id=$1", id)
			if e != nil {
				return e
			}
			if found {
				out.State.Services = append(out.State.Services, v.(Service))
			}
		}
		out.ConfigRevision = m.Revision
		out.EventID, err = pgAuthWatermark(ctx, tx)
		return err
	})
	if err != nil {
		return AuthSnapshot{}, err
	}
	return out, nil
}
