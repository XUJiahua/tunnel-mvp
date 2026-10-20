package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

func pgIdentityStatus(ctx context.Context, tx *sql.Tx, id string) (AuthIdentityStatus, bool, error) {
	data, err := pgAuthLoad(ctx, tx, "kind='identity' AND key=$1", id)
	if err != nil {
		return AuthIdentityStatus{}, false, err
	}
	value, found := data.Identities[id]
	return value, found, nil
}
func pgIdentitySave(ctx context.Context, tx *sql.Tx, status AuthIdentityStatus) error {
	raw, err := json.Marshal(status)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO tunnel_state.auth_records(kind,key,payload,expires_at) VALUES('identity',$1,$2,$3) ON CONFLICT(kind,key) DO UPDATE SET payload=excluded.payload,expires_at=excluded.expires_at`, status.UserID, string(raw), status.RetainUntil.UTC().Truncate(time.Microsecond))
	return err
}
func pgIdentityUser(ctx context.Context, tx *sql.Tx, id string, identity Identity) error {
	value, found, err := pgOne(ctx, tx, "users", "id=$1", id)
	if err != nil {
		return err
	}
	if !found {
		return ErrAuthChanged
	}
	user := value.(User)
	if user.Disabled || user.Identity == nil || *user.Identity != identity {
		return ErrAuthChanged
	}
	return nil
}
func pgIdentityRevoke(ctx context.Context, tx *sql.Tx, userID string) (int64, error) {
	result, err := tx.ExecContext(ctx, `DELETE FROM tunnel_state.auth_records WHERE kind='session' AND user_id=$1 AND payload->>'Method'='oidc'`, userID)
	if err != nil {
		return 0, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM tunnel_state.auth_records g WHERE g.kind IN ('ticket','access') AND NOT EXISTS(SELECT 1 FROM tunnel_state.auth_records s WHERE s.kind='session' AND s.key=g.parent_key)`)
	return n, err
}
func (p *PostgresRepository) AuthIdentityStart(ctx context.Context, id string, identity Identity) (AuthIdentityStatus, bool, error) {
	if !resourceID.MatchString(id) || ValidateIdentity(identity) != nil {
		return AuthIdentityStatus{}, false, ErrAuthChanged
	}
	var out AuthIdentityStatus
	owner := false
	err := p.authWrite(ctx, func(ctx context.Context, tx *sql.Tx, m pgMetadata, now time.Time) error {
		if err := pgAuthCleanup(ctx, tx, now); err != nil {
			return p.failed(err, false)
		}
		if err := pgIdentityUser(ctx, tx, id, identity); err != nil {
			return p.failed(err, false)
		}
		previous, found, err := pgIdentityStatus(ctx, tx, id)
		if err != nil {
			return p.failed(err, false)
		}
		if !found {
			var count int
			if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM tunnel_state.auth_records WHERE kind='identity'`).Scan(&count); err != nil {
				return p.failed(err, false)
			}
			if count >= 4096 {
				return ErrAuthCapacity
			}
		}
		if found && previous.Issuer == identity.Issuer && previous.Subject == identity.Subject && previous.Pending && now.Before(previous.StartedAt.Add(10*time.Second)) {
			out = previous
			return nil
		}
		status := previous
		if !found || status.Issuer != identity.Issuer || status.Subject != identity.Subject || !now.Before(status.ValidUntil) {
			epoch, err := pgRandomID()
			if err != nil {
				return err
			}
			n, err := pgIdentityRevoke(ctx, tx, id)
			if err != nil {
				return p.failed(err, false)
			}
			if n > 0 {
				if err = pgAuthEvent(ctx, tx, m.Revision, "revoke"); err != nil {
					return p.failed(err, false)
				}
			}
			status = AuthIdentityStatus{UserID: id, Issuer: identity.Issuer, Subject: identity.Subject, Epoch: epoch}
		}
		nonce, err := pgRandomID()
		if err != nil {
			return err
		}
		status.CheckID = nonce
		status.Pending = true
		status.CheckSucceeded = false
		status.StartedAt = now
		status.RetainUntil = now.Add(5 * time.Minute)
		if status.ValidUntil.After(status.RetainUntil) {
			status.RetainUntil = status.ValidUntil
		}
		if err = pgIdentitySave(ctx, tx, status); err != nil {
			return p.failed(err, false)
		}
		out = status
		owner = true
		return nil
	})
	if err != nil {
		return AuthIdentityStatus{}, false, err
	}
	return out, owner, nil
}
func (p *PostgresRepository) AuthIdentityFinish(ctx context.Context, id, checkID string, verified, enabled bool, maxStale time.Duration) (AuthIdentityStatus, bool, error) {
	if !resourceID.MatchString(id) || len(checkID) != 32 || maxStale <= 0 || maxStale > time.Hour {
		return AuthIdentityStatus{}, false, fmt.Errorf("invalid shared identity completion")
	}
	var out AuthIdentityStatus
	applied := false
	var revoked uint64
	err := p.authWrite(ctx, func(ctx context.Context, tx *sql.Tx, m pgMetadata, now time.Time) error {
		status, found, err := pgIdentityStatus(ctx, tx, id)
		if err != nil {
			return p.failed(err, false)
		}
		out = status
		if !found || !status.Pending || status.CheckID != checkID {
			return nil
		}
		if err = pgIdentityUser(ctx, tx, id, Identity{status.Issuer, status.Subject}); err != nil {
			return p.failed(err, false)
		}
		if !status.VerifiedAt.IsZero() && !now.Before(status.ValidUntil) {
			n, e := pgIdentityRevoke(ctx, tx, id)
			if e != nil {
				return p.failed(e, false)
			}
			revoked += uint64(n)
			epoch, e := pgRandomID()
			if e != nil {
				return e
			}
			status.Epoch = epoch
			if n > 0 {
				if e = pgAuthEvent(ctx, tx, m.Revision, "revoke"); e != nil {
					return p.failed(e, false)
				}
			}
		}
		status.Pending = false
		status.CheckSucceeded = verified
		status.CompletedAt = now
		if verified {
			status.Enabled = enabled
			status.VerifiedAt = now
			status.ValidUntil = now.Add(maxStale)
			if !enabled {
				n, e := pgIdentityRevoke(ctx, tx, id)
				if e != nil {
					return p.failed(e, false)
				}
				revoked += uint64(n)
				if n > 0 {
					if e = pgAuthEvent(ctx, tx, m.Revision, "revoke"); e != nil {
						return p.failed(e, false)
					}
				}
			}
		}
		status.RetainUntil = now.Add(5 * time.Minute)
		if status.ValidUntil.After(status.RetainUntil) {
			status.RetainUntil = status.ValidUntil
		}
		if err = pgIdentitySave(ctx, tx, status); err != nil {
			return p.failed(err, false)
		}
		out = status
		out.Revoked = revoked
		applied = true
		return nil
	})
	if err != nil {
		return AuthIdentityStatus{}, false, err
	}
	return out, applied, nil
}
func (p *PostgresRepository) AuthIdentityRead(ctx context.Context, id string) (AuthIdentityStatus, bool, time.Time, error) {
	ctx, cancel := context.WithTimeout(ctx, authOperationTimeout)
	defer cancel()
	var out AuthIdentityStatus
	var found bool
	var now time.Time
	err := p.read(ctx, func(ctx context.Context, tx *sql.Tx, _ pgMetadata) error {
		if err := p.authPolicy(ctx, tx); err != nil {
			return err
		}
		var err error
		now, err = pgAuthNow(ctx, tx)
		if err != nil {
			return err
		}
		out, found, err = pgIdentityStatus(ctx, tx, id)
		if err != nil {
			return err
		}
		if found && !now.Before(out.RetainUntil) {
			found = false
		}
		return nil
	})
	if err != nil {
		return AuthIdentityStatus{}, false, time.Time{}, err
	}
	return out, found, now, nil
}
