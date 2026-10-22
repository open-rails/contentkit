package media

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertype"
)

const legacyCleanupWhere = `state NOT IN ('completed', 'cancelled', 'discarded') AND (
kind IN ('contentkit_media_sweep', 'contentkit_media_sweep_pass') OR
kind = 'contentkit_media_delete_folder')`

type legacyCleanup struct {
	id          int64
	kind        string
	args        []byte
	due         time.Time
	state       string
	prefix      string
	operation   uuid.UUID
	incarnation string
}

func legacyCleanupTable(schema string) string {
	if schema == "" {
		return pgx.Identifier{"river_job"}.Sanitize()
	}
	return pgx.Identifier{schema, "river_job"}.Sanitize()
}

func (job *legacyCleanup) owned(ctx context.Context, q folderQuerier, reg *Registry, journal *PGJournal) (bool, error) {
	if job.kind == "contentkit_media_sweep_pass" {
		return true, nil
	}
	var args struct {
		Prefix, Operation, Incarnation string
		Final                          bool
	}
	if err := json.Unmarshal(job.args, &args); err != nil {
		return false, err
	}
	item, retired, err := journal.resolveFolder(ctx, q, reg, args.Prefix)
	if err != nil {
		return false, nil // another registry using this River schema
	}
	job.prefix = item.Prefix()
	if job.kind == "contentkit_media_delete_folder" {
		if !args.Final {
			if !retired {
				return false, nil
			} // current first-pass I/O remains queued
			if _, err := uuid.Parse(args.Incarnation); err != nil {
				return false, ErrCommitIdentity
			}
			job.incarnation = args.Incarnation
			return true, nil
		}
		job.operation, err = uuid.Parse(args.Operation)
		if err != nil || job.operation == uuid.Nil {
			return false, fmt.Errorf("media: legacy deletion %d has no valid operation", job.id)
		}
	}
	return true, nil
}

// MigrateCleanup transfers pending legacy cleanup timers to the routed
// PostgreSQL ledger and cancels each timer in that same transaction. Stop old
// media hosts/workers first: a running legacy cleanup must finish before this
// migration, and an old producer must not create new timers afterward.
// grace must match JobsConfig.Grace (zero selects the same 24-hour default).
func MigrateCleanup(ctx context.Context, pool *pgxpool.Pool, reg *Registry, journal *PGJournal, riverSchema string, grace time.Duration) error {
	if pool == nil || reg == nil || journal == nil || journal.pool != pool {
		return errors.New("media: cleanup migration needs one pool, registry and journal")
	}
	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{Schema: riverSchema})
	if err != nil {
		return err
	}
	if grace <= 0 {
		grace = 24 * time.Hour
	}
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		for _, tenant := range reg.Namespaces() {
			kinds := reg.cleanupKinds(tenant)
			jr := journal.forTenant(tenant)
			if _, err := tx.Exec(ctx, `WITH initialized AS (
 UPDATE `+jr.table+` SET effects=jsonb_set(effects,'{legacy_publication,initialized}','true')
 WHERE tenant_id=$1 AND effects->'legacy_publication'->>'initialized'='false'
 RETURNING folder_prefix,(effects->'legacy_publication'->>'previous_due')::timestamptz AS previous_due)
 UPDATE `+jr.cleanup+` cleanup SET due_at=greatest($2::timestamptz,initialized.previous_due),revision=gen_random_uuid()
 FROM initialized WHERE cleanup.tenant_id=$1 AND cleanup.folder_prefix=initialized.folder_prefix`, tenant, time.Now().Add(grace)); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, "UPDATE "+jr.cleanup+" SET due_at=$3, revision=gen_random_uuid() WHERE tenant_id=$1 AND content_kind=ANY($2) AND due_at='infinity'::timestamptz", tenant, kinds, time.Now().Add(grace)); err != nil {
				return err
			}
		}
		var after int64
		for {
			rows, err := tx.Query(ctx, `SELECT id, kind, args, scheduled_at, state
FROM `+legacyCleanupTable(riverSchema)+` WHERE `+legacyCleanupWhere+` AND id > $1
ORDER BY id LIMIT 100 FOR UPDATE`, after)
			if err != nil {
				return err
			}
			jobs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (legacyCleanup, error) {
				var job legacyCleanup
				err := row.Scan(&job.id, &job.kind, &job.args, &job.due, &job.state)
				return job, err
			})
			if err != nil {
				return err
			}
			if len(jobs) == 0 {
				return prepareOrphanedLegacyCleanup(ctx, tx, reg, journal, grace)
			}
			for _, job := range jobs {
				after = job.id
				owned, err := job.owned(ctx, tx, reg, journal)
				if err != nil {
					return err
				}
				if !owned {
					continue
				}
				if job.state == string(rivertype.JobStateRunning) {
					return fmt.Errorf("media: stop legacy cleanup workers before migrating running job %d", job.id)
				}
				if job.prefix != "" {
					item, retired, err := journal.resolveFolder(ctx, tx, reg, job.prefix)
					if err != nil {
						return err
					}
					var operations []uuid.UUID
					due := job.due
					if retired {
						due = maxTime(due, time.Now().Add(grace))
						id, err := journal.prepareLegacyRetirementTx(ctx, tx, reg, job.prefix, job.id, job.incarnation, due)
						if err != nil {
							return err
						}
						operations = append(operations, id)
					}
					if job.operation != uuid.Nil {
						commit, err := journal.load(ctx, tx, item.Ref().TenantID, job.operation)
						if err != nil {
							return fmt.Errorf("media: legacy deletion %d has no canonical operation: %w", job.id, err)
						}
						if !commit.Ref.Equal(item.Ref()) || commit.Folder != item.Prefix() || commit.State != "applied" || !commit.Effects.Deletion {
							return fmt.Errorf("media: legacy deletion %d does not match an applied deletion", job.id)
						}
						operations = append(operations, job.operation)
					}
					if err := journal.scheduleCleanupTx(ctx, tx, item.Ref(), job.prefix, due, operations, true); err != nil {
						return err
					}
				}
				if _, err := client.JobCancelTx(ctx, tx, job.id); err != nil {
					return err
				}
			}
		}
	})
}

// ValidateCleanupMigration is read-only. A stopped deployment must transfer
// old timers explicitly instead of silently dropping their cleanup targets.
func ValidateCleanupMigration(ctx context.Context, pool *pgxpool.Pool, reg *Registry, journal *PGJournal, riverSchema string) error {
	if pool == nil || reg == nil || journal == nil || journal.pool != pool {
		return errors.New("media: cleanup validation needs one pool, registry and journal")
	}
	rows, err := pool.Query(ctx, `SELECT id, kind, args FROM `+legacyCleanupTable(riverSchema)+` WHERE `+legacyCleanupWhere)
	if err != nil {
		return err
	}
	jobs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (legacyCleanup, error) {
		var job legacyCleanup
		err := row.Scan(&job.id, &job.kind, &job.args)
		return job, err
	})
	if err != nil {
		return err
	}
	for _, job := range jobs {
		owned, err := job.owned(ctx, pool, reg, journal)
		if err != nil {
			return err
		}
		if owned {
			return fmt.Errorf("media: legacy cleanup job %d requires MigrateCleanup before startup", job.id)
		}
	}
	for _, tenant := range reg.Namespaces() {
		kinds := reg.cleanupKinds(tenant)
		var uninitialized bool
		if err := pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM "+journal.forTenant(tenant).cleanup+" WHERE tenant_id=$1 AND content_kind=ANY($2) AND due_at='infinity'::timestamptz)", tenant, kinds).Scan(&uninitialized); err != nil {
			return err
		}
		if uninitialized {
			return errors.New("media: cleanup bootstrap requires MigrateCleanup before startup")
		}
	}
	return nil
}
