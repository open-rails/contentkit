package workqueue

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/contentkit/media"
)

// AdoptVersionImports moves this app's old host ZIP jobs into its external
// ingestion fleet. The caller must stop old producers/workers first and apply
// worker migrations. Inserts and old-row cancellation share one transaction;
// completed history and unrelated apps are never changed.
func (q *Queue) AdoptVersionImports(ctx context.Context, hostSchema, catalogSchema, tenant string) error {
	if hostSchema == "" {
		hostSchema = "public"
	}
	if err := ValidSchema(hostSchema); err != nil {
		return err
	}
	if err := ValidSchema(catalogSchema); err != nil {
		return err
	}
	old := pgx.Identifier{hostSchema, "river_job"}.Sanitize()
	items := pgx.Identifier{catalogSchema, "content_items"}.Sanitize()
	versions := pgx.Identifier{catalogSchema, "content_versions"}.Sanitize()
	var exists bool
	if err := q.pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, old).Scan(&exists); err != nil || !exists {
		return err
	}
	return pgx.BeginFunc(ctx, q.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT j.id,j.state,c.id,c.kind,coalesce(v.id,''),c.state='deleted' OR coalesce(v.state='deleted',true),
 EXISTS(SELECT 1 FROM `+items+` foreign_content WHERE foreign_content.id=c.id AND foreign_content.tenant_id<>c.tenant_id)
 FROM `+old+` j JOIN `+items+` c ON c.id=j.args->>'content' AND c.tenant_id=$1
 LEFT JOIN `+versions+` v ON v.tenant_id=c.tenant_id AND v.content_id=c.id AND v.id=j.args->>'version'
 WHERE j.kind='contentkit_version_import' AND j.state IN ('available','pending','retryable','scheduled','running')
 AND jsonb_typeof(j.args->'content')='string' AND jsonb_typeof(j.args->'version')='string'
 ORDER BY j.id FOR UPDATE OF j`, tenant)
		if err != nil {
			return err
		}
		type request struct {
			id                            int64
			state, content, kind, version string
			gone, ambiguous               bool
		}
		pending, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (request, error) {
			var r request
			err := row.Scan(&r.id, &r.state, &r.content, &r.kind, &r.version, &r.gone, &r.ambiguous)
			return r, err
		})
		if err != nil {
			return err
		}
		// Validate the whole batch before canceling any old work.
		for _, r := range pending {
			if r.state == "running" {
				return fmt.Errorf("media/workqueue: old ZIP import %d is running; stop old workers before migration", r.id)
			}
			if r.ambiguous {
				return fmt.Errorf("media/workqueue: old ZIP import %d has an ambiguous tenant identity; resolve it before migration", r.id)
			}
		}
		for _, r := range pending {
			if !r.gone {
				ref, err := q.kinds.VersionRef(r.kind, r.content, r.version)
				if err != nil {
					return err
				}
				if err = q.EnqueueTx(ctx, tx, media.ProcessJob{Ref: ref, Place: true}); err != nil {
					return err
				}
			}
			if _, err = tx.Exec(ctx, `UPDATE `+old+` SET state='cancelled',finalized_at=clock_timestamp() WHERE id=$1`, r.id); err != nil {
				return err
			}
		}
		return nil
	})
}
