package contentkit

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/hub"
	"github.com/open-rails/contentkit/signal"
)

type forgetContentsArgs struct {
	Contents []signal.ForgottenContent `json:"contents"`
}

func (forgetContentsArgs) Kind() string { return "contentkit_forget_contents" }

// forgetContentsWorker erases purged content's signals; it retries while the
// signal plane is down and does nothing without one.
type forgetContentsWorker struct {
	river.WorkerDefaults[forgetContentsArgs]
	e *Engine
}

func (w *forgetContentsWorker) Timeout(*river.Job[forgetContentsArgs]) time.Duration {
	return 30 * time.Minute
}

func (w *forgetContentsWorker) Work(ctx context.Context, job *river.Job[forgetContentsArgs]) error {
	err := w.e.hub.ForgetContents(ctx, job.Args.Contents)
	if errors.Is(err, hub.ErrSignalPlaneDown) {
		return err // configured but unreachable: retry rather than drop the erasure
	}
	return ignoreDisabled(err)
}

// forgetPurgedTx records required signal erasure with the catalog mutation.
// An absent running fleet still uses an insert-only producer.
func (e *Engine) forgetPurgedTx(ctx context.Context, tx pgx.Tx, refs []contentref.ContentRef) error {
	if !e.purgeSignalsConfigured() || len(refs) == 0 {
		return nil
	}
	contents := make([]signal.ForgottenContent, 0, len(refs))
	for _, ref := range refs {
		if ref.TenantID != e.cfg.Tenant {
			return errors.New("contentkit: purge cleanup crossed catalog scope")
		}
		contents = append(contents, signal.ForgottenContent{Kind: ref.ContentKind, ContentID: ref.ContentID, VersionID: ref.Version()})
	}
	return e.insertForgottenTx(ctx, tx, contents)
}

func (e *Engine) purgeSignalsConfigured() bool {
	if e.hub != nil {
		return e.hub.SignalsConfigured()
	}
	// Explicit migrations have no Hub/Deps. Database also covers an injected
	// ClickHouse connection, whose required database remains in Config.
	return strings.TrimSpace(e.cfg.ClickHouse.Database) != "" || strings.TrimSpace(e.cfg.ClickHouse.Addr) != ""
}

func (e *Engine) insertForgottenTx(ctx context.Context, tx pgx.Tx, contents []signal.ForgottenContent) error {
	if len(contents) == 0 {
		return nil
	}
	if tx == nil || e.pool == nil {
		return errors.New("contentkit: signal erasure requires the catalog transaction and pool")
	}
	var client *river.Client[pgx.Tx]
	if e.jobs != nil {
		client = e.jobs.bound()
	}
	if client == nil {
		var err error
		client, err = river.NewClient(riverpgxv5.New(e.pool), &river.Config{Schema: e.riverSchema})
		if err != nil {
			return err
		}
	}
	_, err := client.InsertTx(ctx, tx, forgetContentsArgs{Contents: contents}, &river.InsertOpts{Queue: QueueContentKit, MaxAttempts: 25})
	return err
}

// preflightCatalogEventMigration refuses a live old delivery worker before SQL
// changes its columns. Operators must stop old writers throughout migration;
// this read-only check does not claim to fence future worker starts.
func (e *Engine) preflightCatalogEventMigration(ctx context.Context) error {
	jobs := pgx.Identifier{e.riverSchema, "river_job"}.Sanitize()
	var exists bool
	if err := e.pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, jobs).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return nil
	}
	var running bool
	if err := e.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM `+jobs+` WHERE kind=ANY($1) AND state='running')`, []string{"contentkit_catalog_deliver_events", "contentkit_openrails_notifications"}).Scan(&running); err != nil {
		return err
	}
	if running {
		return errors.New("contentkit: stop old catalog and billing notification workers before migrating event logs")
	}
	return nil
}

// migrateCatalogEvents runs only after River and SQL32 migrations. It retires
// the old transport, then transfers each retained purge fact to ordinary
// durable domain work. This app cannot start while its handoff is incomplete.
func (e *Engine) migrateCatalogEvents(ctx context.Context) error {
	jobs := pgx.Identifier{e.riverSchema, "river_job"}.Sanitize()
	if err := pgx.BeginFunc(ctx, e.pool, func(tx pgx.Tx) error {
		// Exclude claims/inserts while checking the retiring workers.
		if _, err := tx.Exec(ctx, `LOCK TABLE `+jobs+` IN SHARE ROW EXCLUSIVE MODE`); err != nil {
			return err
		}
		var running bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM `+jobs+` WHERE kind=ANY($1) AND state='running')`, []string{"contentkit_catalog_deliver_events", "contentkit_openrails_notifications"}).Scan(&running); err != nil {
			return err
		}
		if running {
			return errors.New("contentkit: stop old catalog and billing notification workers before migrating event logs")
		}
		_, err := tx.Exec(ctx, `UPDATE `+jobs+` SET state='cancelled',finalized_at=clock_timestamp() WHERE kind=ANY($1) AND state IN ('available','pending','retryable','scheduled')`, []string{"contentkit_catalog_deliver_events", "contentkit_openrails_notifications"})
		return err
	}); err != nil {
		return err
	}
	table := pgx.Identifier{e.schema, "content_event_purge_handoff"}.Sanitize()
	for {
		count := 0
		err := pgx.BeginFunc(ctx, e.pool, func(tx pgx.Tx) error {
			rows, err := tx.Query(ctx, `SELECT event_id,content_kind,content_id,coalesce(content_version_id,'') FROM `+table+` WHERE tenant_id=$1 ORDER BY event_id LIMIT 200 FOR UPDATE`, e.cfg.Tenant)
			if err != nil {
				return err
			}
			var ids []int64
			var contents []signal.ForgottenContent
			for rows.Next() {
				var id int64
				var ref signal.ForgottenContent
				if err := rows.Scan(&id, &ref.Kind, &ref.ContentID, &ref.VersionID); err != nil {
					rows.Close()
					return err
				}
				ids, contents = append(ids, id), append(contents, ref)
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return err
			}
			count = len(ids)
			if count == 0 {
				return nil
			}
			// Migration has no Deps and cannot prove whether the old deployment
			// injected a signal connection. Preserve all legacy obligations;
			// the existing worker handles an intentionally disabled plane.
			if err := e.insertForgottenTx(ctx, tx, contents); err != nil {
				return err
			}
			_, err = tx.Exec(ctx, `DELETE FROM `+table+` WHERE tenant_id=$1 AND event_id=ANY($2)`, e.cfg.Tenant, ids)
			return err
		})
		if err != nil {
			return fmt.Errorf("contentkit: migrate catalog purge work: %w", err)
		}
		if count < 200 {
			return e.validateCatalogEventHandoff(ctx)
		}
	}
}

func (e *Engine) validateCatalogEventHandoff(ctx context.Context) error {
	var pending bool
	if err := e.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM `+pgx.Identifier{e.schema, "content_event_purge_handoff"}.Sanitize()+` WHERE tenant_id=$1)`, e.cfg.Tenant).Scan(&pending); err != nil {
		return err
	}
	if pending {
		return errors.New("contentkit: catalog purge handoff is incomplete; run the full explicit PostgreSQL migration")
	}
	jobs := pgx.Identifier{e.riverSchema, "river_job"}.Sanitize()
	var exists bool
	if err := e.pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, jobs).Scan(&exists); err != nil {
		return err
	}
	if exists {
		if err := e.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM `+jobs+` WHERE kind=ANY($1) AND state IN ('available','pending','retryable','scheduled','running'))`, []string{"contentkit_catalog_deliver_events", "contentkit_openrails_notifications"}).Scan(&pending); err != nil {
			return err
		}
		if pending {
			return errors.New("contentkit: old event delivery jobs remain; run the full explicit PostgreSQL migration")
		}
	}
	return nil
}
