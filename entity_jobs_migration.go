package contentkit

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/contentkit/entities"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
)

// Only an explicit tenant and a known historical queue establish ownership.
// Terminal history and other apps' work remain untouched.
const obsoleteEntityJobs = `args->>'tenant'=$2 AND (
 (kind IN ('contentkit_groups','contentkit_entity_projection') AND queue=$1)
 OR (kind IN ('contentkit_group_reconcile','contentkit_entity_reconcile','contentkit_group_sync') AND queue IN ('contentkit_groups','contentkit_entities'))
 OR (kind='contentkit_entity_sync' AND queue='contentkit_groups'))`

const ambiguousEntityJobs = `kind IN ('contentkit_groups','contentkit_entity_projection') AND queue=$1 AND coalesce(args->>'tenant','')=''`

func (e *Engine) migrateEntityJobsTx(ctx context.Context, tx pgx.Tx, jobs string) error {
	if err := e.preflightEntityJobs(ctx, tx, jobs); err != nil {
		return err
	}
	if err := e.admitPendingEntityLifecycles(ctx, tx); err != nil {
		return err
	}
	// Preserve exact old sync work; the broad pollers and derived-count jobs end.
	if _, err := tx.Exec(ctx, `UPDATE `+jobs+` SET kind='contentkit_entity_sync',queue=$3,unique_key=NULL
 WHERE state IN ('available','pending','retryable','scheduled') AND kind IN ('contentkit_group_sync','contentkit_entity_sync') AND (`+obsoleteEntityJobs+`)`, QueueContentKit, e.cfg.Tenant, entities.DefaultQueue); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE `+jobs+` SET state='cancelled',finalized_at=clock_timestamp(),unique_key=NULL
 WHERE state IN ('available','pending','retryable','scheduled') AND (`+obsoleteEntityJobs+`)`, QueueContentKit, e.cfg.Tenant)
	return err
}

// This one-time migration turns existing lifecycle mismatches into exact jobs
// before retiring the old scan. Subsequent writes admit their own work atomically.
func (e *Engine) admitPendingEntityLifecycles(ctx context.Context, tx pgx.Tx) error {
	var scopes, owned []string
	for name, kind := range e.cfg.Content.EntityKinds {
		if kind.AuthScope {
			scopes = append(scopes, name)
		}
		if kind.Owner == "user" {
			owned = append(owned, name)
		}
	}
	table := pgx.Identifier{e.schema, "content_entities"}.Sanitize()
	items := pgx.Identifier{e.schema, "content_items"}.Sanitize()
	rows, err := tx.Query(ctx, `SELECT g.id FROM `+table+` g WHERE g.tenant_id=$1 AND (
 (g.state='active' AND g.auth_scope IS NULL AND g.kind=ANY($2::text[]))
 OR (g.state<>'active' AND g.auth_scope IS NOT NULL)
 OR (g.kind=ANY($3::text[]) AND EXISTS(SELECT 1 FROM `+items+` c WHERE c.tenant_id=g.tenant_id AND (
  (g.state='deleted' AND c.owner_entity_id=g.id AND c.state<>'deleted')
  OR (g.state='active' AND c.deleted_with_entity=g.id AND c.state='deleted'))))) ORDER BY g.id`, e.cfg.Tenant, scopes, owned)
	if err != nil {
		return err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil || len(ids) == 0 {
		return err
	}
	client, err := river.NewClient(riverpgxv5.New(e.pool), &river.Config{Schema: e.riverSchema})
	if err != nil {
		return err
	}
	for len(ids) > 0 {
		n := min(len(ids), 100)
		_, err = client.InsertTx(ctx, tx, entityLifecycleArgs{Tenant: e.cfg.Tenant, Entities: ids[:n]}, &river.InsertOpts{Queue: entities.DefaultQueue, MaxAttempts: 10, UniqueOpts: river.UniqueOpts{ByArgs: true}})
		if err != nil {
			return err
		}
		ids = ids[n:]
	}
	return nil
}

type entityLifecycleArgs struct {
	Tenant   string   `json:"tenant"`
	Entities []string `json:"groups"`
}

func (entityLifecycleArgs) Kind() string { return "contentkit_entity_sync" }

func (e *Engine) validateEntityJobHandoff(ctx context.Context, jobs string) error {
	var pending bool
	if err := e.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM `+jobs+` WHERE state IN ('available','pending','retryable','scheduled','running') AND ((`+obsoleteEntityJobs+`) OR (`+ambiguousEntityJobs+`)))`, QueueContentKit, e.cfg.Tenant).Scan(&pending); err != nil {
		return err
	}
	if pending {
		return fmt.Errorf("contentkit: obsolete entity jobs require the explicit stopped-writer migration")
	}
	return nil
}

// Refuse incompatible active or unattributable work before dropping old schema.
// The transaction-held handoff repeats this check; old writers must stay stopped.
func (e *Engine) preflightEntityJobs(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, jobs string) error {
	var ambiguous, running bool
	if err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM `+jobs+` WHERE state IN ('available','pending','retryable','scheduled','running') AND (`+ambiguousEntityJobs+`))`, QueueContentKit).Scan(&ambiguous); err != nil {
		return err
	}
	if ambiguous {
		return fmt.Errorf("contentkit: stop the shared worker fleet and retire its tenantless entity projection jobs explicitly; their app ownership is ambiguous")
	}
	if err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM `+jobs+` WHERE state='running' AND (`+obsoleteEntityJobs+`))`, QueueContentKit, e.cfg.Tenant).Scan(&running); err != nil {
		return err
	}
	if running {
		return fmt.Errorf("contentkit: stop old entity workers before migrating their jobs")
	}
	return nil
}
