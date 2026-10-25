package openrails

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrInboxHandoffRequired means old source facts remain beyond the saved inbox
// cursor. Explicitly Consume those pages before retiring the old poller.
var ErrInboxHandoffRequired = errors.New("openrails notifications: finish the explicit inbox handoff before retiring polling jobs")

// RetireInboxSync cancels only this source's unfinished legacy polling jobs.
// Call during an explicit stopped-writer migration after the final Consume pass
// and before enabling direct producer publication. It is not a runtime worker.
// Terminal history, other sources, general events and inbox receipts survive.
func (s *Source) RetireInboxSync(ctx context.Context, pool *pgxpool.Pool, riverSchema, namespace string, destination Inbox) error {
	if pool == nil || riverSchema == "" || namespace == "" || destination == nil {
		return errors.New("openrails notifications: pool, River schema, namespace and destination are required")
	}
	key := fmt.Sprintf("%x", sha256.Sum256([]byte(namespace+"\x00"+s.name+"\x00"+s.merchant)))
	table := pgx.Identifier{riverSchema, "river_job"}.Sanitize()
	var exists bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, table).Scan(&exists); err != nil {
		return err
	}
	var running, pending bool
	if exists {
		if err := pool.QueryRow(ctx, `SELECT coalesce(bool_or(state='running'),false),count(*)>0 FROM `+table+`
		WHERE kind='contentkit_openrails_inbox_sync' AND args->>'key'=$1
		AND state IN ('available','pending','retryable','scheduled','running')`, key).Scan(&running, &pending); err != nil {
			return err
		}
	}
	if running {
		return errors.New("openrails notifications: stop the running legacy inbox worker before retirement")
	}
	// Read the destination and source before taking a pool connection for the
	// retirement transaction, including when the destination shares a size-one pool.
	after, err := destination.EventCursor(ctx, s.Origin())
	if err != nil {
		return err
	}
	page, err := s.read(ctx, after, 1)
	if err != nil {
		return err
	}
	if len(page.Events) != 0 || page.HasMore {
		return ErrInboxHandoffRequired
	}
	// Between periodic ticks or after a discarded job there may be no live
	// job at all. That does not prove the source cursor has caught up.
	if !pending {
		return nil
	}
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		// Old instances must already be stopped. This lock closes the remaining
		// claim/check race; it cannot fence a future old deployment.
		if _, err := tx.Exec(ctx, `LOCK TABLE `+table+` IN SHARE ROW EXCLUSIVE MODE`); err != nil {
			return err
		}
		var running, pending bool
		if err := tx.QueryRow(ctx, `SELECT coalesce(bool_or(state='running'),false),count(*)>0 FROM `+table+`
		WHERE kind='contentkit_openrails_inbox_sync' AND args->>'key'=$1
		AND state IN ('available','pending','retryable','scheduled','running')`, key).Scan(&running, &pending); err != nil {
			return err
		}
		if running {
			return errors.New("openrails notifications: stop the running legacy inbox worker before retirement")
		}
		if !pending {
			return nil
		}
		_, err := tx.Exec(ctx, `UPDATE `+table+` SET state='cancelled',finalized_at=clock_timestamp()
		WHERE kind='contentkit_openrails_inbox_sync' AND args->>'key'=$1
		AND state IN ('available','pending','retryable','scheduled')`, key)
		return err
	})
}
