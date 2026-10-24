package media

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/contentkit/contentref"
)

// MigrateProcessing retires exposure/readiness jobs under the stopped-writer
// upgrade contract. Existing terminal history and other namespaces are kept.
// Real metadata work and ordinary processing replace old pending work in the
// same transaction. Stop every old host/worker before this migration and restart
// only the matching new cohort after it succeeds.
func MigrateProcessing(ctx context.Context, pool *pgxpool.Pool, reg *Registry, journal *PGJournal, riverSchema string, process TransactionalProcessQueue, publish func(context.Context, pgx.Tx, contentref.ContentRef) error) error {
	if pool == nil || reg == nil || journal == nil || process == nil || journal.pool != pool {
		return errors.New("media: processing migration needs one pool, registry, journal and processing queue")
	}
	if riverSchema == "" {
		riverSchema = "public"
	}
	jobs := pgx.Identifier{riverSchema, "river_job"}.Sanitize()
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		// The lock prevents an old producer from slipping a row in while its queue
		// is inspected. The operator must keep old writers stopped throughout.
		if _, err := tx.Exec(ctx, `LOCK TABLE `+jobs+` IN SHARE ROW EXCLUSIVE MODE`); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT id,state,args FROM `+jobs+` WHERE kind IN ('contentkit_media_ready','contentkit_media_expose') AND state NOT IN ('completed','cancelled','discarded') AND args->'ref'->>'tenant_id'=ANY($1) ORDER BY id FOR UPDATE`, reg.Namespaces())
		if err != nil {
			return err
		}
		type oldJob struct {
			id    int64
			state string
			args  []byte
		}
		old, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (oldJob, error) {
			var job oldJob
			err := row.Scan(&job.id, &job.state, &job.args)
			return job, err
		})
		if err != nil {
			return err
		}
		for _, job := range old {
			if job.state == "running" {
				return fmt.Errorf("media: stop old workers before migrating running lifecycle job %d", job.id)
			}
			var args struct {
				Ref contentref.ContentRef `json:"ref"`
			}
			if err := json.Unmarshal(job.args, &args); err != nil {
				return err
			}
			if _, err := reg.Item(args.Ref); err != nil {
				return fmt.Errorf("media: cannot identify old lifecycle job %d: %w", job.id, err)
			}
			if process != nil {
				if err := process.EnqueueTx(ctx, tx, ProcessJob{Ref: args.Ref}); err != nil {
					return err
				}
			}
			if publish != nil {
				if err := publish(ctx, tx, args.Ref); err != nil {
					return err
				}
			}
			if _, err := tx.Exec(ctx, `UPDATE `+jobs+` SET state='cancelled',finalized_at=clock_timestamp() WHERE id=$1`, job.id); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	// Existing drafts may have had all public presets deleted and no pending
	// exposure job. Enqueue one ordinary image pass per journal-owned item.
	for _, namespace := range reg.Namespaces() {
		jr := journal.forTenant(namespace)
		after := ""
		for {
			finished := false
			err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
				rows, err := tx.Query(ctx, `SELECT DISTINCT ON(folder_prefix) folder_prefix,content_kind,content_id,coalesce(content_version_id,'') FROM `+jr.table+` WHERE tenant_id=$1 AND folder_prefix>$2 AND state='applied' ORDER BY folder_prefix,updated_at DESC LIMIT 100`, namespace, after)
				if err != nil {
					return err
				}
				type owned struct{ folder, kind, id, version string }
				batch, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (owned, error) {
					var x owned
					err := row.Scan(&x.folder, &x.kind, &x.id, &x.version)
					return x, err
				})
				if err != nil {
					return err
				}
				for _, x := range batch {
					ref := contentref.New(namespace, x.kind, x.id)
					if x.version != "" {
						ref = ref.WithVersion(x.version)
					}
					item, err := reg.Item(ref)
					if err == nil && item.Prefix() == x.folder && process != nil {
						operation := uuid.NewSHA1(uuid.NameSpaceURL, []byte("contentkit:public-presets-v1:"+x.folder))
						fingerprint := sha256.Sum256([]byte("public-presets-v1"))
						receipt, err := tx.Exec(ctx, `INSERT INTO `+jr.table+` (tenant_id,operation_id,content_kind,content_id,content_version_id,folder_prefix,actor_id,fingerprint,lease_id,state,effects)
VALUES($1,$2,$3,$4,NULLIF($5,''),$6,'migration:public-presets-v1',$7,$8,'applied','{}') ON CONFLICT(tenant_id,operation_id) DO NOTHING`, namespace, operation, x.kind, x.id, x.version, x.folder, fingerprint[:], uuid.New())
						if err != nil {
							return err
						}
						if receipt.RowsAffected() > 0 {
							if err := process.EnqueueTx(ctx, tx, ProcessJob{Ref: ref}); err != nil {
								return err
							}
						}
					}
					after = x.folder
				}
				finished = len(batch) < 100
				return nil
			})
			if err != nil {
				return err
			}
			if finished {
				break
			}
		}
	}
	return nil
}

// ValidateProcessingMigration is read-only startup validation. A leftover
// owned lifecycle job requires the explicit stopped-writer migration.
func ValidateProcessingMigration(ctx context.Context, pool *pgxpool.Pool, reg *Registry, riverSchema string) error {
	var pending bool
	err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM `+legacyCleanupTable(riverSchema)+` WHERE kind IN ('contentkit_media_ready','contentkit_media_expose') AND state NOT IN ('completed','cancelled','discarded') AND args->'ref'->>'tenant_id'=ANY($1))`, reg.Namespaces()).Scan(&pending)
	if err != nil {
		return err
	}
	if pending {
		return fmt.Errorf("media: obsolete lifecycle jobs require MigrateProcessing with old writers stopped")
	}
	return nil
}
