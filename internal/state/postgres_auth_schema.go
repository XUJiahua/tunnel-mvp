package state

import (
	"context"
	"database/sql"
)

func pgCreateAuthSchema(ctx context.Context, tx *sql.Tx) error {
	for _, query := range []string{
		`CREATE TABLE IF NOT EXISTS tunnel_state.auth_policy(singleton BOOLEAN PRIMARY KEY CHECK(singleton),policy_hash TEXT NOT NULL,last_event_id BIGINT NOT NULL DEFAULT 0 CHECK(last_event_id>=0))`,
		`INSERT INTO tunnel_state.auth_policy(singleton,policy_hash) VALUES(true,'') ON CONFLICT DO NOTHING`,
		`CREATE TABLE IF NOT EXISTS tunnel_state.auth_records(
 kind TEXT NOT NULL CHECK(kind IN ('session','ticket','access','limit','oidc','replay','cutoff','identity')),
 key TEXT NOT NULL,payload JSONB NOT NULL CHECK(jsonb_typeof(payload)='object'),expires_at TIMESTAMPTZ NOT NULL,
 user_id TEXT GENERATED ALWAYS AS (CASE WHEN kind IN ('session','identity') THEN payload->>'UserID' ELSE NULL END) STORED REFERENCES tunnel_state.users(id) DEFERRABLE INITIALLY DEFERRED,
 parent_kind TEXT GENERATED ALWAYS AS (CASE WHEN kind IN ('ticket','access') THEN 'session' ELSE NULL END) STORED,
 parent_key TEXT GENERATED ALWAYS AS (CASE WHEN kind IN ('ticket','access') THEN payload->>'SessionKey' ELSE NULL END) STORED,
 public_id TEXT GENERATED ALWAYS AS (CASE WHEN kind='session' THEN payload->>'ID' ELSE NULL END) STORED,
 PRIMARY KEY(kind,key),UNIQUE(kind,public_id) DEFERRABLE INITIALLY DEFERRED,
 FOREIGN KEY(parent_kind,parent_key) REFERENCES tunnel_state.auth_records(kind,key) DEFERRABLE INITIALLY DEFERRED)`,
		`CREATE INDEX IF NOT EXISTS auth_records_expiry ON tunnel_state.auth_records(kind,expires_at)`,
		`CREATE INDEX IF NOT EXISTS auth_records_user ON tunnel_state.auth_records(user_id) WHERE kind='session'`,
		`CREATE INDEX IF NOT EXISTS auth_records_parent ON tunnel_state.auth_records(parent_key) WHERE kind IN ('ticket','access')`,
		`CREATE TABLE IF NOT EXISTS tunnel_state.auth_outbox(id BIGSERIAL PRIMARY KEY,revision BIGINT NOT NULL CHECK(revision>=0),kind TEXT NOT NULL CHECK(kind IN ('config','revoke','policy')),created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp())`,
		`CREATE TABLE IF NOT EXISTS tunnel_state.auth_consumers(id TEXT PRIMARY KEY,cursor BIGINT NOT NULL CHECK(cursor>=0),updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp())`,
	} {
		if _, err := tx.ExecContext(ctx, query); err != nil {
			return err
		}
	}
	return nil
}
