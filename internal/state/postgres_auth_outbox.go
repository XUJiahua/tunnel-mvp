package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

func validConsumer(id string) bool { return resourceID.MatchString(id) }
func (p *PostgresRepository) AuthPoll(ctx context.Context, consumer string, limit int) (AuthEvents, error) {
	if !validConsumer(consumer) || limit < 1 || limit > 200 {
		return AuthEvents{}, fmt.Errorf("invalid authentication consumer")
	}
	ctx, cancel := context.WithTimeout(ctx, authOperationTimeout)
	defer cancel()
	var out AuthEvents
	err := p.read(ctx, func(ctx context.Context, tx *sql.Tx, _ pgMetadata) error {
		if err := p.authPolicy(ctx, tx); err != nil {
			return err
		}
		var cursor uint64
		err := tx.QueryRowContext(ctx, `SELECT cursor FROM tunnel_state.auth_consumers WHERE id=$1`, consumer).Scan(&cursor)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}

		var first uint64
		if err = tx.QueryRowContext(ctx, `SELECT coalesce(min(id),0) FROM tunnel_state.auth_outbox`).Scan(&first); err != nil {
			return err
		}
		last, err := pgAuthWatermark(ctx, tx)
		if err != nil {
			return err
		}
		out.Reset = cursor > last || first > 0 && cursor < first-1 || first == 0 && cursor < last
		if out.Reset {
			return nil
		}
		rows, err := tx.QueryContext(ctx, `SELECT id,revision,kind,created_at FROM tunnel_state.auth_outbox WHERE id>$1 ORDER BY id LIMIT $2`, int64(cursor), limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e AuthEvent
			if err = rows.Scan(&e.ID, &e.Revision, &e.Kind, &e.CreatedAt); err != nil {
				return err
			}
			out.Events = append(out.Events, e)
		}
		return rows.Err()
	})
	if err != nil {
		return AuthEvents{}, err
	}
	return out, nil
}
func (p *PostgresRepository) AuthAcknowledge(ctx context.Context, consumer string, cursor uint64) error {
	if !validConsumer(consumer) || cursor > uint64(1<<63-1) {
		return fmt.Errorf("invalid authentication consumer acknowledgement")
	}
	return p.authWrite(ctx, func(ctx context.Context, tx *sql.Tx, _ pgMetadata, _ time.Time) error {
		watermark, err := pgAuthWatermark(ctx, tx)
		if err != nil {
			return p.failed(err, false)
		}
		if cursor > watermark {
			return fmt.Errorf("cannot acknowledge an unseen authentication event")
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO tunnel_state.auth_consumers(id,cursor) VALUES($1,$2) ON CONFLICT(id) DO UPDATE SET cursor=CASE WHEN tunnel_state.auth_consumers.cursor>$3 THEN excluded.cursor ELSE greatest(tunnel_state.auth_consumers.cursor,excluded.cursor) END,updated_at=clock_timestamp()`, consumer, int64(cursor), int64(watermark))
		return p.failed(err, false)
	})
}
func (p *PostgresRepository) AuthMaintenance(ctx context.Context) error {
	return p.authWrite(ctx, func(ctx context.Context, tx *sql.Tx, _ pgMetadata, now time.Time) error {
		if err := pgAuthCleanup(ctx, tx, now); err != nil {
			return p.failed(err, false)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM tunnel_state.auth_consumers WHERE updated_at<$1`, now.Add(-24*time.Hour)); err != nil {
			return p.failed(err, false)
		}
		// Events contain invalidation hints, never the authority itself. Retention
		// gaps require an authoritative resnapshot; flow leases bound stale access.
		_, err := tx.ExecContext(ctx, `DELETE FROM tunnel_state.auth_outbox WHERE id<(SELECT greatest(0,coalesce(max(id),0)-100000) FROM tunnel_state.auth_outbox) OR (created_at<$1 AND id<=coalesce((SELECT min(cursor) FROM tunnel_state.auth_consumers),(SELECT coalesce(max(id),0) FROM tunnel_state.auth_outbox)))`, now.Add(-24*time.Hour))
		return p.failed(err, false)
	})
}
