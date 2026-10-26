package contentkit

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// These are previously shipped job identities, recognized only by the explicit
// stopped-writer migration. No runtime worker or alias is registered for them.
const obsoleteEntityJobs = `args->>'tenant'=$2 AND (
 (kind IN ('contentkit_groups','contentkit_entity_projection') AND queue=$1)
 OR (kind IN ('contentkit_group_reconcile','contentkit_entity_reconcile','contentkit_group_sync','contentkit_entity_sync') AND queue IN ('contentkit_groups','contentkit_entities')))`

const ambiguousEntityJobs = `coalesce(args->>'tenant','')='' AND (
 (kind IN ('contentkit_groups','contentkit_entity_projection') AND queue=$1)
 OR (kind IN ('contentkit_group_reconcile','contentkit_entity_reconcile','contentkit_group_sync','contentkit_entity_sync') AND queue IN ('contentkit_groups','contentkit_entities')))`

func (e *Engine) migrateEntityJobsTx(ctx context.Context, tx pgx.Tx, jobs string) error {
	if err := e.preflightEntityJobs(ctx, tx, jobs); err != nil {
		return err
	}
	// The stored channel state and cascade prove there is no unfinished work.
	// Keep terminal history and foreign app jobs exactly as they stand.
	_, err := tx.Exec(ctx, `UPDATE `+jobs+` SET state='cancelled',finalized_at=clock_timestamp(),unique_key=NULL
 WHERE state IN ('available','pending','retryable','scheduled') AND (`+obsoleteEntityJobs+`)`, QueueContentKit, e.cfg.Tenant)
	return err
}

func (e *Engine) validateEntityJobHandoff(ctx context.Context, jobs string) error {
	var pending bool
	err := e.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM `+jobs+` WHERE state IN ('available','pending','retryable','scheduled','running') AND ((`+obsoleteEntityJobs+`) OR (`+ambiguousEntityJobs+`)))`, QueueContentKit, e.cfg.Tenant).Scan(&pending)
	if err != nil {
		return err
	}
	if pending {
		return fmt.Errorf("contentkit: retired channel jobs require the explicit stopped-writer migration")
	}
	return nil
}

// preflightEntityJobs refuses work that still needs the old implementation.
// Complete it with the old version before stopping writers and upgrading. The
// new version never reimplements a retired worker inside migration or startup.
func (e *Engine) preflightEntityJobs(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, jobs string) error {
	var ambiguous, running bool
	if err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM `+jobs+` WHERE state IN ('available','pending','retryable','scheduled','running') AND (`+ambiguousEntityJobs+`))`, QueueContentKit).Scan(&ambiguous); err != nil {
		return err
	}
	if ambiguous {
		return fmt.Errorf("contentkit: tenantless legacy entity jobs have ambiguous ownership; resolve them under the old version before stopping its writers")
	}
	if err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM `+jobs+` WHERE state='running' AND (`+obsoleteEntityJobs+`))`, QueueContentKit, e.cfg.Tenant).Scan(&running); err != nil {
		return err
	}
	if running {
		return fmt.Errorf("contentkit: finish and stop old channel lifecycle workers before migration")
	}
	// Only legacy obligations invoke this drain requirement. A new synchronous
	// request may legitimately leave a pending channel for its caller to retry.
	var legacy bool
	if err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM `+jobs+` WHERE state IN ('available','pending','retryable','scheduled') AND (`+obsoleteEntityJobs+`))`, QueueContentKit, e.cfg.Tenant).Scan(&legacy); err != nil {
		return err
	}
	if !legacy {
		return nil
	}
	table := pgx.Identifier{e.schema, "content_entities"}.Sanitize()
	items := pgx.Identifier{e.schema, "content_items"}.Sanitize()
	var exists bool
	if err := q.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL AND to_regclass($2) IS NOT NULL", table, items).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return nil
	}
	var unresolved string
	err := q.QueryRow(ctx, `SELECT coalesce((SELECT g.id FROM `+table+` g WHERE g.tenant_id=$1 AND g.kind='channel' AND (
  (g.state='active' AND g.auth_scope IS NULL)
  OR (g.state<>'active' AND g.auth_scope IS NOT NULL)
  OR EXISTS(SELECT 1 FROM `+items+` c WHERE c.tenant_id=g.tenant_id AND (
   (g.state='deleted' AND c.owner_entity_id=g.id AND c.state<>'deleted')
   OR (g.state='active' AND c.deleted_with_entity=g.id AND c.state='deleted')))) ORDER BY g.id LIMIT 1),'')`, e.cfg.Tenant).Scan(&unresolved)
	if err != nil {
		return err
	}
	if unresolved != "" {
		return fmt.Errorf("contentkit: channel %s has unfinished authorization or content lifecycle work; finish it under the old version, stop writers, then retry migration", unresolved)
	}
	return nil
}
