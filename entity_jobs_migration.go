package contentkit

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/contentkit/entities"
)

// These are exact prior producer envelopes in this Engine's owned River
// schema. Explicitly scoped foreign-tenant work and terminal history stay put.
const obsoleteEntityJobs = `(kind='contentkit_groups' AND queue=$1 AND coalesce(args->>'tenant',$2)=$2)
 OR (kind IN ('contentkit_group_sync','contentkit_group_reconcile','contentkit_entity_sync','contentkit_entity_reconcile')
 AND args->>'tenant'=$2 AND (kind IN ('contentkit_group_sync','contentkit_group_reconcile') OR queue='contentkit_groups'))`

func (e *Engine) migrateEntityJobsTx(ctx context.Context, tx pgx.Tx, jobs string) error {
	var running bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM `+jobs+` WHERE state='running' AND (`+obsoleteEntityJobs+`))`, QueueContentKit, e.cfg.Tenant).Scan(&running); err != nil {
		return err
	}
	if running {
		return fmt.Errorf("contentkit: stop old entity workers before migrating their jobs")
	}
	if _, err := tx.Exec(ctx, `UPDATE `+jobs+` SET state='cancelled',finalized_at=clock_timestamp() WHERE state IN ('available','pending','retryable','scheduled') AND kind='contentkit_groups' AND (`+obsoleteEntityJobs+`)`, QueueContentKit, e.cfg.Tenant); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE `+jobs+` SET kind=CASE kind WHEN 'contentkit_group_sync' THEN 'contentkit_entity_sync' WHEN 'contentkit_group_reconcile' THEN 'contentkit_entity_reconcile' ELSE kind END,
queue=$3,unique_key=NULL WHERE state IN ('available','pending','retryable','scheduled') AND kind<>'contentkit_groups' AND (`+obsoleteEntityJobs+`)`, QueueContentKit, e.cfg.Tenant, entities.DefaultQueue)
	return err
}

func (e *Engine) validateEntityJobHandoff(ctx context.Context, jobs string) error {
	var pending bool
	if err := e.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM `+jobs+` WHERE state IN ('available','pending','retryable','scheduled','running') AND (`+obsoleteEntityJobs+`))`, QueueContentKit, e.cfg.Tenant).Scan(&pending); err != nil {
		return err
	}
	if pending {
		return fmt.Errorf("contentkit: obsolete entity jobs require the explicit stopped-writer migration")
	}
	return nil
}
