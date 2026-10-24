package media

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	eventpg "github.com/open-rails/helpers/eventlog/postgres"

	"github.com/open-rails/contentkit/contentref"
)

// scheduleCleanupTx merges one folder's cleanup obligations. Deletion keeps
// every exact operation. New work runs at the earliest due deadline; each
// receipt retains its own late-write deadline. Migration alone may extend it.
func (j *PGJournal) scheduleCleanupTx(ctx context.Context, tx pgx.Tx, ref contentref.ContentRef, prefix string, due time.Time, operations []uuid.UUID, migrating bool) error {
	j = j.forTenant(ref.TenantID)
	if operations == nil {
		operations = []uuid.UUID{}
	}
	_, err := tx.Exec(ctx, `INSERT INTO `+j.cleanup+` AS c
(tenant_id, folder_prefix, content_kind, due_at, deletion_operations)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (tenant_id, folder_prefix) DO UPDATE SET
due_at = CASE WHEN $6 THEN greatest(c.due_at,EXCLUDED.due_at) ELSE least(c.due_at,EXCLUDED.due_at) END,
deletion_operations = ARRAY(SELECT DISTINCT op FROM unnest(c.deletion_operations || EXCLUDED.deletion_operations) AS op ORDER BY op),
revision = gen_random_uuid()`, ref.TenantID, prefix, ref.ContentKind, due, operations, migrating)
	return err
}

type purgeContextKey struct{}

type cleanupRecord struct {
	prefix     string
	revision   uuid.UUID
	operations []uuid.UUID
}

// PurgeRetired is storage work within the existing content-deletion sweep.
// It finishes exact durable intents and expired allocations; it never discovers
// objects by scanning the bucket. Failed records remain due for that same sweep.
func (j *Jobs) PurgeRetired(ctx context.Context, limit int) (int, error) {
	ctx = context.WithValue(ctx, purgeContextKey{}, true)
	if limit <= 0 {
		return 0, errors.New("media: purge limit must be positive")
	}
	processed := 0
	var failures error
	for _, tenant := range j.cfg.Registry.Namespaces() {
		if processed >= limit {
			break
		}
		jr := j.cfg.Journal.forTenant(tenant)
		if jr != j.cfg.Journal {
			log, err := eventpg.New(jr.pool, eventpg.Config{Schema: jr.schema, Source: "contentkit", Scope: tenant})
			if err != nil {
				return processed, errors.Join(failures, err)
			}
			if _, err := log.Prune(ctx, limit); err != nil {
				failures = errors.Join(failures, err)
			}
		}
		rows, err := jr.pool.Query(ctx, `SELECT folder_prefix,revision,deletion_operations FROM `+jr.cleanup+`
WHERE tenant_id=$1 AND content_kind=ANY($2) AND due_at<=$3 ORDER BY due_at,folder_prefix LIMIT $4`, tenant, j.cfg.Registry.cleanupKinds(tenant), j.cfg.Now(), limit-processed)
		if err != nil {
			return processed, errors.Join(failures, err)
		}
		records, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (cleanupRecord, error) {
			var c cleanupRecord
			err := row.Scan(&c.prefix, &c.revision, &c.operations)
			return c, err
		})
		if err != nil {
			return processed, errors.Join(failures, err)
		}
		for _, c := range records {
			if err := ctx.Err(); err != nil {
				return processed, errors.Join(failures, err)
			}
			processed++
			item, legacy, err := jr.resolveFolder(ctx, jr.pool, j.cfg.Registry, c.prefix)
			var wait time.Duration
			if err == nil && item.Ref().TenantID != tenant {
				err = ErrCommitIdentity
			}
			if err == nil {
				if legacy {
					err = j.cleanLegacyPublication(ctx, item, c.operations)
				} else {
					wait, err = j.cleanupFolder(ctx, item, c.operations)
				}
			}
			if err != nil {
				failures = errors.Join(failures, fmt.Errorf("purge media %s: %w", c.prefix, err))
				continue
			}
			if wait > 0 {
				_, err = jr.pool.Exec(ctx, `UPDATE `+jr.cleanup+` SET due_at=$4,revision=gen_random_uuid()
WHERE tenant_id=$1 AND folder_prefix=$2 AND revision=$3`, tenant, c.prefix, c.revision, j.cfg.Now().Add(wait))
			} else {
				_, err = jr.pool.Exec(ctx, `DELETE FROM `+jr.cleanup+` WHERE tenant_id=$1 AND folder_prefix=$2 AND revision=$3`, tenant, c.prefix, c.revision)
			}
			if err != nil {
				return processed, errors.Join(failures, err)
			}
		}
	}
	return processed, failures
}

func (j *Jobs) cleanupFolder(ctx context.Context, item Item, operations []uuid.UUID) (time.Duration, error) {
	if item.Ref().Version() != "" && item.Prefix() != item.Kind().prefix(item.Ref().ContentID, item.Ref().Version()) {
		return 0, j.cleanVersionStorage(ctx, item, operations)
	}
	result, err := j.sweep(ctx, item.Prefix())
	if err != nil {
		return 0, err
	}
	// Expiring allocations records exact retirement in this same sweep. Read
	// those receipts before deleting so first-pass bytes do not wait a day.
	jr := j.cfg.Journal.forTenant(item.Ref().TenantID)
	var current []uuid.UUID
	err = jr.pool.QueryRow(ctx, `SELECT deletion_operations FROM `+jr.cleanup+` WHERE tenant_id=$1 AND folder_prefix=$2`, item.Ref().TenantID, item.Prefix()).Scan(&current)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return 0, err
	}
	for _, id := range current {
		if !slices.Contains(operations, id) {
			operations = append(operations, id)
		}
	}
	var wait time.Duration
	for _, operation := range operations {
		var intent manifestCommit
		jr := j.cfg.Journal.forTenant(item.Ref().TenantID)
		err := pgx.BeginFunc(ctx, jr.pool, func(tx pgx.Tx) error {
			var err error
			intent, err = jr.load(ctx, tx, item.Ref().TenantID, operation)
			return err
		})
		if err != nil {
			return 0, err
		}
		if intent.Effects.FolderDelete != nil {
			err = j.retireFolder(ctx, item, intent)
		} else {
			err = j.repeatDeletion(ctx, item, operation)
		}
		if err != nil {
			return 0, err
		}
		wait = max(wait, intent.Effects.CleanupAt.Sub(j.cfg.Now()))
	}
	return max(wait, result.Wait), nil
}
