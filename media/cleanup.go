package media

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/open-rails/contentkit/contentref"
)

// scheduleCleanupTx merges one folder's cleanup obligations. Deletion keeps
// every exact operation and waits for the latest late-upload deadline; ordinary
// edits cannot postpone garbage collection indefinitely. Migration alone may
// extend an ordinary deadline to preserve an old timer's promised grace.
func (j *PGJournal) scheduleCleanupTx(ctx context.Context, tx pgx.Tx, ref contentref.ContentRef, prefix string, due time.Time, operations []uuid.UUID, migrating bool) error {
	j = j.forTenant(ref.TenantID)
	if operations == nil {
		operations = []uuid.UUID{}
	}
	_, err := tx.Exec(ctx, `INSERT INTO `+j.cleanup+` AS c
(tenant_id, folder_prefix, content_kind, due_at, deletion_operations)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (tenant_id, folder_prefix) DO UPDATE SET
due_at = CASE WHEN $6 OR cardinality(EXCLUDED.deletion_operations) > 0 THEN greatest(c.due_at, EXCLUDED.due_at)
              WHEN cardinality(c.deletion_operations) > 0 THEN c.due_at
              ELSE least(c.due_at, EXCLUDED.due_at) END,
deletion_operations = ARRAY(SELECT DISTINCT op FROM unnest(c.deletion_operations || EXCLUDED.deletion_operations) AS op ORDER BY op),
revision = gen_random_uuid()`, ref.TenantID, prefix, ref.ContentKind, due, operations, migrating)
	return err
}

type cleanupRecord struct {
	prefix     string
	revision   uuid.UUID
	operations []uuid.UUID
}

// CleanupDue processes at most limit due folders. The existing manifest lease
// fences each storage operation; a fresh UUID revision prevents acknowledging
// work added by a concurrent edit, even after a row was deleted and recreated.
// No database transaction is held across storage
// I/O, and a failure in one folder does not block other folders.
func (j *Jobs) CleanupDue(ctx context.Context, limit int) (int, error) {
	return j.cleanupDue(ctx, limit, false)
}

// CleanupPurgedVersions repeats exact late-write receipts for permanently
// erased versions. The content purge operation calls this, not media's reaper.
func (j *Jobs) CleanupPurgedVersions(ctx context.Context, limit int) (int, error) {
	return j.cleanupDue(ctx, limit, true)
}

func (j *Jobs) cleanupDue(ctx context.Context, limit int, purgedVersions bool) (int, error) {
	if limit <= 0 {
		return 0, errors.New("media: cleanup limit must be positive")
	}
	processed := 0
	var failures []error
	for _, tenant := range j.cfg.Registry.Namespaces() {
		if processed >= limit {
			break
		}
		kinds := j.cfg.Registry.cleanupKinds(tenant)
		journal := j.cfg.Journal.forTenant(tenant)
		rows, err := journal.pool.Query(ctx, `SELECT c.folder_prefix,c.revision,c.deletion_operations
FROM `+journal.cleanup+` c WHERE c.tenant_id=$1 AND c.content_kind=ANY($2) AND c.due_at<=$3
AND EXISTS(SELECT 1 FROM `+journal.table+` m WHERE m.tenant_id=c.tenant_id
 AND m.operation_id=ANY(c.deletion_operations) AND m.content_version_id IS NOT NULL AND m.effects->>'erasure'='true')=$5
ORDER BY c.due_at,c.folder_prefix LIMIT $4`, tenant, kinds, j.cfg.Now(), limit-processed, purgedVersions)
		if err != nil {
			return processed, errors.Join(append(failures, err)...)
		}
		records, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (cleanupRecord, error) {
			var record cleanupRecord
			err := row.Scan(&record.prefix, &record.revision, &record.operations)
			return record, err
		})
		if err != nil {
			return processed, errors.Join(append(failures, err)...)
		}
		for _, record := range records {
			if err := ctx.Err(); err != nil {
				return processed, errors.Join(append(failures, err)...)
			}
			processed++
			item, legacy, err := journal.resolveFolder(ctx, journal.pool, j.cfg.Registry, record.prefix)
			if err == nil && item.Ref().TenantID != tenant {
				err = errors.New("media: cleanup prefix does not belong to its tenant")
			}
			var wait time.Duration
			if err == nil {
				workCtx, cancel := context.WithTimeout(ctx, 15*time.Minute)
				if legacy {
					err = j.cleanLegacyPublication(workCtx, item, record.operations)
				} else {
					wait, err = j.cleanupFolder(workCtx, item, record.operations)
				}
				cancel()
			}
			if err != nil {
				j.cfg.Logger.WarnContext(ctx, "media: due cleanup failed", "prefix", record.prefix, "error", err)
				if len(failures) < 10 {
					failures = append(failures, fmt.Errorf("%s: %w", record.prefix, err))
				}
				_, updateErr := journal.pool.Exec(ctx, `UPDATE `+journal.cleanup+` SET due_at = $4
WHERE tenant_id = $1 AND folder_prefix = $2 AND revision = $3`, tenant, record.prefix, record.revision, j.cfg.Now().Add(j.cfg.CleanupInterval))
				if updateErr != nil {
					return processed, errors.Join(append(failures, updateErr)...)
				}
				continue
			}
			if wait > 0 {
				_, err = journal.pool.Exec(ctx, `UPDATE `+journal.cleanup+`
SET due_at = $4, deletion_operations = CASE WHEN $5 THEN deletion_operations ELSE '{}'::uuid[] END, revision = gen_random_uuid()
WHERE tenant_id = $1 AND folder_prefix = $2 AND revision = $3`, tenant, record.prefix, record.revision, j.cfg.Now().Add(wait), purgedVersions)
			} else {
				_, err = journal.pool.Exec(ctx, `DELETE FROM `+journal.cleanup+`
WHERE tenant_id = $1 AND folder_prefix = $2 AND revision = $3`, tenant, record.prefix, record.revision)
			}
			if err != nil {
				return processed, errors.Join(append(failures, err)...)
			}
		}
	}
	return processed, errors.Join(failures...)
}

func (j *Jobs) cleanupFolder(ctx context.Context, item Item, operations []uuid.UUID) (time.Duration, error) {
	if item.Ref().Version() != "" && item.Prefix() != item.Kind().prefix(item.Ref().ContentID, item.Ref().Version()) {
		return 0, j.cleanVersionStorage(ctx, item, operations)
	}
	for _, operation := range operations {
		if err := j.repeatDeletion(ctx, item, operation); err != nil {
			return 0, err
		}
	}
	result, err := j.sweep(ctx, item.Prefix())
	return result.Wait, err
}

type cleanupDueArgs struct{}

func (cleanupDueArgs) Kind() string { return "contentkit_media_cleanup_due" }

func (cleanupDueArgs) InsertOpts() river.InsertOpts {
	unique := PendingOnce
	unique.ByQueue = true
	return river.InsertOpts{Queue: DefaultQueue, UniqueOpts: unique}
}

type cleanupDueWorker struct {
	river.WorkerDefaults[cleanupDueArgs]
	j *Jobs
}

func (w *cleanupDueWorker) Timeout(*river.Job[cleanupDueArgs]) time.Duration { return 6 * time.Hour }

func (w *cleanupDueWorker) Work(ctx context.Context, job *river.Job[cleanupDueArgs]) (err error) {
	defer func() { err = SnoozeUnavailable(ctx, w.j.cfg.Store, job.JobRow, err) }()
	for {
		n, err := w.j.CleanupDue(ctx, 1000)
		if err != nil || n < 1000 {
			return err
		}
	}
}
