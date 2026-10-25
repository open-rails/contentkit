package contentkit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/signal"
)

// forgetPurgedTx records required signal erasure with the catalog mutation.
// The ordinary content purge sweep drains these records after deleting content.
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
	if tx == nil {
		return errors.New("contentkit: signal erasure requires the catalog transaction")
	}
	table := pgx.Identifier{e.schema, "content_signal_purges"}.Sanitize()
	for _, ref := range contents {
		if strings.TrimSpace(ref.Kind) == "" || strings.TrimSpace(ref.ContentID) == "" {
			return errors.New("contentkit: signal erasure needs a kind and content ID")
		}
		if _, err := tx.Exec(ctx, `INSERT INTO `+table+`
 (tenant_id,content_kind,content_id,content_version_id) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, e.cfg.Tenant, ref.Kind, ref.ContentID, ref.VersionID); err != nil {
			return err
		}
	}
	return nil
}

// purgeSignals is part of the content-deletion sweep, including when media is
// disabled. Rows survive the content itself and remain due until ClickHouse
// confirms erasure. Only these cleanup rows are locked over the remote call;
// a signal outage does not prevent the sweep from purging other content.
func (e *Engine) purgeSignals(ctx context.Context) error {
	if e.hub == nil || !e.hub.SignalsConfigured() {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	table := pgx.Identifier{e.schema, "content_signal_purges"}.Sanitize()
	for {
		count := 0
		err := pgx.BeginFunc(ctx, e.pool, func(tx pgx.Tx) error {
			rows, err := tx.Query(ctx, `SELECT content_kind,content_id,content_version_id FROM `+table+`
 WHERE tenant_id=$1 ORDER BY content_kind,content_id,content_version_id LIMIT 200 FOR UPDATE SKIP LOCKED`, e.cfg.Tenant)
			if err != nil {
				return err
			}
			refs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (signal.ForgottenContent, error) {
				var ref signal.ForgottenContent
				err := row.Scan(&ref.Kind, &ref.ContentID, &ref.VersionID)
				return ref, err
			})
			if err != nil || len(refs) == 0 {
				return err
			}
			count = len(refs)
			if err := e.hub.ForgetContents(ctx, refs); err != nil {
				return err
			}
			kinds, contents, versions := make([]string, count), make([]string, count), make([]string, count)
			for i, ref := range refs {
				kinds[i], contents[i], versions[i] = ref.Kind, ref.ContentID, ref.VersionID
			}
			_, err = tx.Exec(ctx, `DELETE FROM `+table+` p USING unnest($2::text[],$3::text[],$4::text[]) refs(kind,content,version)
 WHERE p.tenant_id=$1 AND p.content_kind=refs.kind AND p.content_id=refs.content AND p.content_version_id=refs.version`, e.cfg.Tenant, kinds, contents, versions)
			return err
		})
		if err != nil || count < 200 {
			return err
		}
	}
}

// Older engines recorded exact cleanup batches in this engine's River schema.
// Keep other queues, explicit foreign tenants and terminal job history intact.
const oldForgetJob = `kind='contentkit_forget_contents' AND queue=$1 AND coalesce(args->>'tenant',$2)=$2`

func (e *Engine) migrateForgottenJobsTx(ctx context.Context, tx pgx.Tx, jobs string) error {
	var running bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM `+jobs+` WHERE state='running' AND (`+oldForgetJob+`))`, QueueContentKit, e.cfg.Tenant).Scan(&running); err != nil {
		return err
	}
	if running {
		return errors.New("contentkit: stop old signal-erasure workers before migrating their work")
	}
	rows, err := tx.Query(ctx, `SELECT id,args,state='discarded' FROM `+jobs+` WHERE state IN ('available','pending','retryable','scheduled','discarded') AND (`+oldForgetJob+`) ORDER BY id FOR UPDATE`, QueueContentKit, e.cfg.Tenant)
	if err != nil {
		return err
	}
	type pending struct {
		id        int64
		args      []byte
		discarded bool
	}
	work, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (pending, error) {
		var p pending
		err := row.Scan(&p.id, &p.args, &p.discarded)
		return p, err
	})
	if err != nil {
		return err
	}
	for _, job := range work {
		refs, owner, err := e.legacyForgottenOwner(ctx, tx, job.args)
		if err != nil {
			return fmt.Errorf("contentkit: attribute signal-erasure job %d: %w", job.id, err)
		}
		if owner != e.cfg.Tenant {
			continue
		}
		if err := e.insertForgottenTx(ctx, tx, refs); err != nil {
			return fmt.Errorf("contentkit: retain signal-erasure job %d: %w", job.id, err)
		}
		// A discarded job may still owe erasure. Copy its obligation while
		// preserving the failed terminal row as history.
		if !job.discarded {
			if _, err := tx.Exec(ctx, `UPDATE `+jobs+` SET state='cancelled',finalized_at=clock_timestamp() WHERE id=$1`, job.id); err != nil {
				return err
			}
		}
	}
	return nil
}

// Old batches lack an app key. Infer it from each exact retained purge fact
// (or remaining content row), never from whichever engine migrates first.
// Unattributable batches need operator repair instead of cross-app erasure.
func (e *Engine) legacyForgottenOwner(ctx context.Context, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}, raw []byte) ([]signal.ForgottenContent, string, error) {
	var args struct {
		Tenant   string                    `json:"tenant"`
		Contents []signal.ForgottenContent `json:"contents"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, "", err
	}
	if args.Contents == nil {
		return nil, "", errors.New("old signal-erasure job lacks its contents batch")
	}
	if args.Tenant != "" {
		return args.Contents, args.Tenant, nil
	}
	owner := ""
	for _, ref := range args.Contents {
		rows, err := q.Query(ctx, `SELECT tenant_id FROM `+pgx.Identifier{e.schema, "content_items"}.Sanitize()+` WHERE kind=$1 AND id=$2
 UNION SELECT tenant_id FROM `+pgx.Identifier{e.schema, "content_events"}.Sanitize()+` WHERE type IN ('content.purged','version.purged')
 AND payload->>'kind'=$1 AND payload->>'content_id'=$2 AND coalesce(payload->>'version_id','')=$3`, ref.Kind, ref.ContentID, ref.VersionID)
		if err != nil {
			return nil, "", err
		}
		owners, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return nil, "", err
		}
		if len(owners) != 1 || owner != "" && owner != owners[0] {
			return nil, "", fmt.Errorf("cannot determine one app for old cleanup of %s/%s; supply its original tenant in the job arguments before migration", ref.Kind, ref.ContentID)
		}
		owner = owners[0]
	}
	if len(args.Contents) == 0 {
		owner = e.cfg.Tenant // an empty batch has no cleanup obligation
	}
	return args.Contents, owner, nil
}

func (e *Engine) validateForgottenJobHandoff(ctx context.Context, jobs string) error {
	rows, err := e.pool.Query(ctx, `SELECT args FROM `+jobs+` WHERE state IN ('available','pending','retryable','scheduled','running') AND (`+oldForgetJob+`)`, QueueContentKit, e.cfg.Tenant)
	if err != nil {
		return err
	}
	batches, err := pgx.CollectRows(rows, pgx.RowTo[[]byte])
	if err != nil {
		return err
	}
	for _, raw := range batches {
		_, owner, err := e.legacyForgottenOwner(ctx, e.pool, raw)
		if err != nil {
			return err
		}
		if owner == e.cfg.Tenant {
			return errors.New("contentkit: old signal-erasure jobs require the explicit stopped-writer migration")
		}
	}
	return nil
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
	if err := e.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM `+jobs+` WHERE state='running' AND (`+oldForgetJob+`))`, QueueContentKit, e.cfg.Tenant).Scan(&running); err != nil {
		return err
	}
	if running {
		return errors.New("contentkit: stop old signal-erasure workers before migrating their work")
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
		if err := e.migrateForgottenJobsTx(ctx, tx, jobs); err != nil {
			return err
		}
		if err := e.migrateEntityJobsTx(ctx, tx, jobs); err != nil {
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
			// the sweep retains obligations while a plane is intentionally disabled.
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
		if err := e.validateForgottenJobHandoff(ctx, jobs); err != nil {
			return err
		}
		if err := e.validateEntityJobHandoff(ctx, jobs); err != nil {
			return err
		}
		if err := e.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM `+jobs+` WHERE kind=ANY($1) AND state IN ('available','pending','retryable','scheduled','running'))`, []string{"contentkit_catalog_deliver_events", "contentkit_openrails_notifications"}).Scan(&pending); err != nil {
			return err
		}
		if pending {
			return errors.New("contentkit: old event delivery jobs remain; run the full explicit PostgreSQL migration")
		}
	}
	return nil
}
