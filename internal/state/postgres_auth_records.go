package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"reflect"
	"time"
)

type authRecord struct {
	Kind, Key string
	Payload   []byte
	Expires   time.Time
}
type authTime struct{ At time.Time }

func authRecords(data AuthData) (map[string]authRecord, error) {
	out := map[string]authRecord{}
	add := func(kind, key string, value any, expires time.Time) error {
		raw, err := json.Marshal(value)
		if err != nil {
			return err
		}
		out[kind+"\x00"+key] = authRecord{kind, key, raw, expires.UTC().Truncate(time.Microsecond)}
		return nil
	}
	for k, v := range data.Identities {
		if e := add("identity", k, v, v.RetainUntil); e != nil {
			return nil, e
		}
	}
	for k, v := range data.Sessions {
		if e := add("session", k, v, v.Expires); e != nil {
			return nil, e
		}
	}
	for _, set := range []struct {
		Kind   string
		Values map[string]AuthGrant
	}{{"ticket", data.Tickets}, {"access", data.Access}} {
		for k, v := range set.Values {
			if e := add(set.Kind, k, v, v.Expires); e != nil {
				return nil, e
			}
		}
	}
	for k, v := range data.Limits {
		if e := add("limit", k, v, v.Until); e != nil {
			return nil, e
		}
	}
	for k, v := range data.Pending {
		if e := add("oidc", k, v, v.Expires); e != nil {
			return nil, e
		}
	}
	for _, set := range []struct {
		Kind      string
		Values    map[string]time.Time
		Retention time.Duration
	}{{"replay", data.Replay, 0}, {"cutoff", data.Cutoff, 6 * time.Minute}} {
		for k, v := range set.Values {
			if e := add(set.Kind, k, authTime{v}, v.Add(set.Retention)); e != nil {
				return nil, e
			}
		}
	}
	return out, nil
}
func authDecode(data *AuthData, record authRecord) error {
	var expires time.Time
	switch record.Kind {
	case "identity":
		var v AuthIdentityStatus
		if err := decodeStrict(record.Payload, &v); err != nil {
			return err
		}
		data.Identities[record.Key] = v
		expires = v.RetainUntil
	case "session":
		var v AuthSession
		if err := decodeStrict(record.Payload, &v); err != nil {
			return err
		}
		data.Sessions[record.Key] = v
		expires = v.Expires
	case "ticket", "access":
		var v AuthGrant
		if err := decodeStrict(record.Payload, &v); err != nil {
			return err
		}
		if record.Kind == "ticket" {
			data.Tickets[record.Key] = v
		} else {
			data.Access[record.Key] = v
		}
		expires = v.Expires
	case "limit":
		var v AuthLimit
		if err := decodeStrict(record.Payload, &v); err != nil {
			return err
		}
		data.Limits[record.Key] = v
		expires = v.Until
	case "oidc":
		var v AuthOIDCAttempt
		if err := decodeStrict(record.Payload, &v); err != nil {
			return err
		}
		data.Pending[record.Key] = v
		expires = v.Expires
	case "replay", "cutoff":
		var v authTime
		if err := decodeStrict(record.Payload, &v); err != nil {
			return err
		}
		if record.Kind == "replay" {
			data.Replay[record.Key] = v.At
			expires = v.At
		} else {
			data.Cutoff[record.Key] = v.At
			expires = v.At.Add(6 * time.Minute)
		}
	default:
		return ErrAuthUnavailable
	}
	if !expires.UTC().Truncate(time.Microsecond).Equal(record.Expires) {
		return fmt.Errorf("invalid shared authentication expiry projection")
	}
	return nil
}
func pgAuthLoad(ctx context.Context, tx *sql.Tx, where string, args ...any) (AuthData, error) {
	data := NewAuthData()
	rows, err := tx.QueryContext(ctx, `SELECT kind,key,payload,expires_at FROM tunnel_state.auth_records WHERE `+where, args...)
	if err != nil {
		return data, err
	}
	defer rows.Close()
	for rows.Next() {
		var record authRecord
		if err = rows.Scan(&record.Kind, &record.Key, &record.Payload, &record.Expires); err != nil {
			return data, err
		}
		if err = authDecode(&data, record); err != nil {
			return data, err
		}
	}
	return data, rows.Err()
}
func pgAuthSave(ctx context.Context, tx *sql.Tx, before, after AuthData) error {
	old, err := authRecords(before)
	if err != nil {
		return err
	}
	next, err := authRecords(after)
	if err != nil {
		return err
	}
	for key, record := range old {
		if _, ok := next[key]; !ok {
			if _, err = tx.ExecContext(ctx, `DELETE FROM tunnel_state.auth_records WHERE kind=$1 AND key=$2`, record.Kind, record.Key); err != nil {
				return err
			}
		}
	}
	for key, record := range next {
		if previous, ok := old[key]; ok && reflect.DeepEqual(record, previous) {
			continue
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO tunnel_state.auth_records(kind,key,payload,expires_at) VALUES($1,$2,$3,$4) ON CONFLICT(kind,key) DO UPDATE SET payload=excluded.payload,expires_at=excluded.expires_at`, record.Kind, record.Key, string(record.Payload), record.Expires); err != nil {
			return err
		}
	}
	return nil
}
func pgAuthNow(ctx context.Context, tx *sql.Tx) (time.Time, error) {
	var now time.Time
	err := tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&now)
	return now, err
}
func pgAuthCleanup(ctx context.Context, tx *sql.Tx, now time.Time) error {
	// A bridge proof epoch can never resurrect an old session after expiry.
	if _, err := tx.ExecContext(ctx, `DELETE FROM tunnel_state.auth_records s WHERE s.kind='session' AND coalesce(s.payload->>'IdentityEpoch','')<>'' AND NOT EXISTS(SELECT 1 FROM tunnel_state.auth_records i WHERE i.kind='identity' AND i.key=s.user_id AND i.payload->>'Epoch'=s.payload->>'IdentityEpoch' AND (i.payload->>'Enabled')::boolean AND (i.payload->>'ValidUntil')::timestamptz>$1)`, now); err != nil {
		return err
	}
	// Remove grants before their expired parent; the FK remains enabled/deferred.
	_, err := tx.ExecContext(ctx, `DELETE FROM tunnel_state.auth_records g WHERE g.kind IN ('ticket','access') AND (g.expires_at<=$1 OR NOT EXISTS(SELECT 1 FROM tunnel_state.auth_records s WHERE s.kind='session' AND s.key=g.parent_key AND s.expires_at>$1))`, now)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM tunnel_state.auth_records WHERE expires_at<=$1`, now)
	return err
}
func pgAuthEvent(ctx context.Context, tx *sql.Tx, revision uint64, kind string) error {
	// The meta writer lock orders allocation and commit of every event.
	var id int64
	if err := tx.QueryRowContext(ctx, `INSERT INTO tunnel_state.auth_outbox(revision,kind) VALUES($1,$2) RETURNING id`, int64(revision), kind).Scan(&id); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE tunnel_state.auth_policy SET last_event_id=$1 WHERE singleton`, id)
	return err
}
func pgAuthWatermark(ctx context.Context, tx *sql.Tx) (uint64, error) {
	var id uint64
	err := tx.QueryRowContext(ctx, `SELECT last_event_id FROM tunnel_state.auth_policy WHERE singleton`).Scan(&id)
	return id, err
}
func (p *PostgresRepository) authPolicy(ctx context.Context, tx *sql.Tx) error {
	if p.authPolicyHash == "" {
		return nil
	}
	var policy string
	if err := tx.QueryRowContext(ctx, `SELECT policy_hash FROM tunnel_state.auth_policy WHERE singleton`).Scan(&policy); err != nil {
		return err
	}
	if policy != p.authPolicyHash {
		p.fence()
		return ErrAuthPolicy
	}
	return nil
}
