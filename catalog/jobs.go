package catalog

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	riverhelpers "github.com/open-rails/helpers/river"
	"github.com/riverqueue/river"

	"github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/versions"
)

// PublishDue records due publication and audience-release facts and refreshes
// their projections. Reads use publication and audience timestamps directly;
// this recorder never determines when a viewer gains access. The return value
// counts the publication stamps and separately caught-up audience releases.
func (s *Store) PublishDue(ctx context.Context) (int, error) {
	total := 0
	deadline := time.Now().Add(purgeBudget)
	for after := ""; ; {
		n, next, err := s.publishDue(ctx, after)
		total += n
		if err == nil && next == "" && n >= 500 && time.Now().Before(deadline) {
			after = ""
			continue
		}
		if err != nil || next == "" || time.Now().After(deadline) {
			if err != nil {
				return total, err
			}
			for {
				n, err := s.recordDueReleases(ctx)
				total += n
				if err != nil || n < releaseBatch || time.Now().After(deadline) {
					return total, err
				}
			}
		}
		after = next
	}
}

func (s *Store) publishDue(ctx context.Context, after string) (int, string, error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return 0, "", err
	}
	defer tx.Rollback(ctx)
	now := s.now()
	var events []Event
	var ids []string
	var rows pgx.Rows
	// Continue only the version-parent scan after an empty, contended batch;
	// content promotion keeps its original one-batch bound for this job run.
	if after == "" {
		rows, err = tx.Query(ctx, `UPDATE `+s.t.items+` SET published_at = publish_at, updated_at = clock_timestamp(), revision = revision + 1
 WHERE id IN (SELECT id FROM `+s.t.items+` WHERE tenant_id = $1 AND state = 'published' AND published_at IS NULL AND publish_at <= $2
  ORDER BY publish_at LIMIT 500 FOR UPDATE SKIP LOCKED) AND tenant_id = $1 RETURNING id, kind, published_at`, s.tenant, now)
		if err != nil {
			return 0, "", err
		}
		for rows.Next() {
			var id, kind string
			var at time.Time
			if err := rows.Scan(&id, &kind, &at); err != nil {
				rows.Close()
				return 0, "", err
			}
			ids = append(ids, id)
			events = append(events, Event{Type: EventContentPublished, Kind: kind, ContentID: id, OccurredAt: at})
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return 0, "", err
		}
	}
	// Access edits and version mutations take the parent before any version.
	// Claim parents first so deriving facets cannot overwrite a later access
	// edit, and skip parents another writer already owns rather than waiting
	// while this batch holds other item/version locks.
	rows, err = tx.Query(ctx, `SELECT c.id FROM `+s.t.items+` c WHERE c.tenant_id = $1 AND c.id > $3
 AND EXISTS (SELECT 1 FROM `+s.t.versions+` v WHERE v.tenant_id = c.tenant_id AND v.content_id = c.id
  AND v.state = 'published' AND v.published_at IS NULL AND v.publish_at <= $2)
 ORDER BY c.id LIMIT 500 FOR UPDATE OF c SKIP LOCKED`, s.tenant, now, after)
	if err != nil {
		return 0, "", err
	}
	parents, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return 0, "", err
	}
	rows, err = tx.Query(ctx, `UPDATE `+s.t.versions+` v SET published_at = greatest(v.publish_at,coalesce(c.published_at,c.publish_at)), updated_at = clock_timestamp(), revision = v.revision + 1
 FROM `+s.t.items+` c WHERE v.id IN (SELECT id FROM `+s.t.versions+` WHERE tenant_id = $1 AND state = 'published' AND published_at IS NULL
  AND content_id = ANY($3) AND publish_at <= $2 ORDER BY publish_at, id LIMIT 500 FOR UPDATE SKIP LOCKED)
 AND v.tenant_id = $1 AND c.tenant_id = v.tenant_id AND c.id = v.content_id
 AND c.state='published' AND c.published_at IS NOT NULL AND coalesce(c.published_at,c.publish_at)<=$2
 RETURNING v.content_id, v.id, c.kind,v.published_at`, s.tenant, now, parents)
	if err != nil {
		return 0, "", err
	}
	versionCount := 0
	for rows.Next() {
		var id, vid, kind string
		var at time.Time
		if err := rows.Scan(&id, &vid, &kind, &at); err != nil {
			rows.Close()
			return 0, "", err
		}
		if !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
		events = append(events, Event{Type: EventVersionPublished, Kind: kind, ContentID: id, VersionID: vid, OccurredAt: at})
		versionCount++
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, "", err
	}
	next := ""
	if versionCount == 0 && len(parents) == 500 {
		// Version-only locks must not hide work in later parents. Release this
		// bounded set of parent locks before continuing with a fresh snapshot.
		next = parents[len(parents)-1]
	}
	if len(ids) == 0 {
		return 0, next, tx.Commit(ctx)
	}
	if err := s.emit(ctx, tx, events...); err != nil {
		return 0, "", err
	}
	if err := s.RefreshFacets(ctx, tx, ids); err != nil {
		return 0, "", err
	}
	if err := s.exposeTx(ctx, tx, ids); err != nil {
		return 0, "", err
	}
	return len(events), next, tx.Commit(ctx)
}

// ReconcileFacets explicitly refreshes every item's facets and search-document
// marks from current catalog state, recording the current access snapshot.
// Use it to repair stale projections after upgrading a writer; ordinary
// publication does not revisit already-published versions. Each transaction
// locks at most 200 parents in ID order before refreshing them. It returns the
// number of items processed.
func (s *Store) ReconcileFacets(ctx context.Context) (int, error) {
	total, after := 0, ""
	for {
		ids, err := s.reconcileFacets(ctx, after)
		total += len(ids)
		if err != nil || len(ids) < 200 {
			return total, err
		}
		after = ids[len(ids)-1]
	}
}

func (s *Store) reconcileFacets(ctx context.Context, after string) ([]string, error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	// Unlike periodic jobs, an explicit repair must not skip busy parents and
	// then move its cursor past rows that still need reconciliation.
	rows, err := tx.Query(ctx, `SELECT id FROM `+s.t.items+`
 WHERE tenant_id = $1 AND id > $2 ORDER BY id LIMIT 200 FOR UPDATE`, s.tenant, after)
	if err != nil {
		return nil, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil || len(ids) == 0 {
		return nil, err
	}
	if err := s.RefreshFacets(ctx, tx, ids); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return ids, nil
}

// purgeBatch bounds one erasing transaction; purgeBudget bounds a purge run
// and skip-only version scans. Storage erasure has its own worker timeout;
// backlogs still drain in bounded batches.
const (
	purgeBatch  = 200
	purgeBudget = 45 * time.Second
)

// PurgeDeleted erases content and versions deleted longer than the retention
// ago, including purchased content. Billing products and purchase history stay;
// holders lose access to the erased media. It returns the number erased.
func (s *Store) PurgeDeleted(ctx context.Context) (int, error) {
	var cleanupErr error
	if s.media != nil {
		_, cleanupErr = s.media.Versions.ResumeDeletes(ctx, purgeBatch)
	}
	if _, err := s.PruneEvents(ctx); err != nil {
		return 0, errors.Join(cleanupErr, err)
	}
	cutoff := s.now().Add(-s.retention)
	items, err := s.scanPurge(ctx, func(ctx context.Context, after string) (int, string, error) {
		return s.erase(ctx, cutoff, after)
	})
	parentErr := err
	var storage *purgeStorageError
	if err != nil && (!errors.As(err, &storage) || ctx.Err() != nil) {
		return items, errors.Join(cleanupErr, err)
	}
	erased, err := s.scanPurge(ctx, func(ctx context.Context, after string) (int, string, error) {
		return s.eraseVersionBatch(ctx, cutoff, after)
	})
	return items + erased, errors.Join(cleanupErr, parentErr, err)
}

// Only folder-specific erasure failures permit the independent version scan.
// Catalog transaction/SQL failures retain their ordinary abort behavior.
type purgeStorageError struct{ err error }

func (e *purgeStorageError) Error() string { return e.err.Error() }
func (e *purgeStorageError) Unwrap() error { return e.err }

// scanPurge advances past captured candidates, including failed and locked
// rows, so each ID is attempted at most once in this bounded scan. Failed
// storage remains eligible for a later invocation; SQL/context errors abort.
func (s *Store) scanPurge(ctx context.Context, batch func(context.Context, string) (int, string, error)) (int, error) {
	deadline := time.Now().Add(purgeBudget)
	total := 0
	var failures error
	finish := func(err error) (int, error) {
		if err != nil {
			return total, errors.Join(failures, err)
		}
		if failures != nil {
			return total, &purgeStorageError{failures}
		}
		return total, nil
	}
	for after := ""; ; {
		if err := ctx.Err(); err != nil {
			return finish(err)
		}
		n, next, err := batch(ctx, after)
		total += n
		if err != nil {
			var storage *purgeStorageError
			if !errors.As(err, &storage) {
				return finish(err)
			}
			failures = errors.Join(failures, storage.err)
		}
		if err := ctx.Err(); err != nil {
			return finish(err)
		}
		if next == "" || time.Now().After(deadline) {
			return finish(nil)
		}
		if next <= after {
			return finish(errors.New("catalog: purge cursor did not advance"))
		}
		after = next
	}
}

// erase claims expired content under row locks, erases its exact version media
// outside the transaction, then removes the still-eligible database records.
func (s *Store) erase(ctx context.Context, cutoff time.Time, after string) (count int, next string, resultErr error) {
	var storageErr error
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, storageErr)
		} else if storageErr != nil {
			resultErr = &purgeStorageError{storageErr}
		}
	}()
	tx, err := s.begin(ctx)
	if err != nil {
		return 0, next, err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `SELECT id, kind FROM `+s.t.items+` WHERE tenant_id = $1 AND state = 'deleted' AND deleted_at < $2 AND id > $3
 ORDER BY id LIMIT `+strconv.Itoa(purgeBatch)+` FOR UPDATE SKIP LOCKED`, s.tenant, cutoff, after)
	if err != nil {
		return 0, next, err
	}
	var ids, kinds []string
	var events []Event
	for rows.Next() {
		var id, kind string
		if err := rows.Scan(&id, &kind); err != nil {
			rows.Close()
			return 0, next, err
		}
		ids, kinds = append(ids, id), append(kinds, kind)
		events = append(events, Event{Type: EventContentPurged, Kind: kind, ContentID: id})
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(ids) == 0 {
		return 0, next, err
	}
	if len(ids) == purgeBatch {
		next = ids[len(ids)-1]
	}
	if s.media != nil {
		rows, err := tx.Query(ctx, `SELECT content_id,id FROM `+s.t.versions+`
 WHERE tenant_id=$1 AND content_id=ANY($2) ORDER BY id FOR UPDATE`, s.tenant, ids)
		if err != nil {
			return 0, next, err
		}
		targets, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (versions.Target, error) {
			var target versions.Target
			err := row.Scan(&target.Content, &target.Version)
			return target, err
		})
		if err != nil {
			return 0, next, err
		}
		if err := s.media.Versions.DeleteTx(ctx, tx, targets); err != nil {
			return 0, next, err
		}
		if err := tx.Commit(ctx); err != nil {
			return 0, next, err
		}
		byParent := make(map[string][]versions.Target, len(ids))
		for _, target := range targets {
			byParent[target.Content] = append(byParent[target.Content], target)
		}
		succeeded := make([]string, 0, len(ids))
		for _, id := range ids {
			if err := ctx.Err(); err != nil {
				return 0, next, err
			}
			if err := s.media.Versions.Delete(ctx, byParent[id]); err != nil {
				storageErr = errors.Join(storageErr, fmt.Errorf("purge content %s: %w", id, err))
				continue
			}
			succeeded = append(succeeded, id)
		}
		ids = succeeded
		if len(ids) == 0 {
			return 0, next, nil
		}
		tx, err = s.begin(ctx)
		if err != nil {
			return 0, next, err
		}
		defer tx.Rollback(ctx)
		// Never substitute another batch after releasing the claim locks.
		rows, err = tx.Query(ctx, `SELECT id,kind FROM `+s.t.items+`
 WHERE tenant_id=$1 AND id=ANY($3) AND state='deleted' AND deleted_at<$2 ORDER BY id FOR UPDATE SKIP LOCKED`, s.tenant, cutoff, ids)
		if err != nil {
			return 0, next, err
		}
		ids, kinds, events = nil, nil, nil
		for rows.Next() {
			var id, kind string
			if err := rows.Scan(&id, &kind); err != nil {
				rows.Close()
				return 0, next, err
			}
			ids, kinds = append(ids, id), append(kinds, kind)
			events = append(events, Event{Type: EventContentPurged, Kind: kind, ContentID: id})
		}
		rows.Close()
		if err := rows.Err(); err != nil || len(ids) == 0 {
			return 0, next, err
		}
	}
	// Their documents go with them.
	if err := s.markDocuments(ctx, tx, ids); err != nil {
		return 0, next, err
	}
	if err := s.eraseInteractions(ctx, tx, kinds, ids, nil); err != nil {
		return 0, next, err
	}
	// Codes stay: a purged item's URL keeps answering 410.
	if s.references != nil {
		rows, err := tx.Query(ctx, `SELECT id FROM `+s.t.versions+` WHERE tenant_id=$1 AND content_id=ANY($2) ORDER BY id`, s.tenant, ids)
		if err != nil {
			return 0, next, err
		}
		versions, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return 0, next, err
		}
		if len(versions) > 0 {
			if err := s.references.PurgeVersions(ctx, tx, s.tenant, versions); err != nil {
				return 0, next, err
			}
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM `+s.t.items+` WHERE tenant_id = $1 AND id = ANY($2)`, s.tenant, ids); err != nil {
		return 0, next, err
	}
	if err := s.purgeTx(ctx, tx, events); err != nil {
		return 0, next, err
	}
	if err := s.emit(ctx, tx, events...); err != nil {
		return 0, next, err
	}
	return len(ids), next, tx.Commit(ctx)
}

// eraseVersionBatch captures a bounded version-ID page before taking the
// same parent-before-version locks as catalog edits. Rechecks after those locks
// and after storage erasure remain restricted to the captured IDs.
func (s *Store) eraseVersionBatch(ctx context.Context, cutoff time.Time, after string) (count int, next string, resultErr error) {
	var storageErr error
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, storageErr)
		} else if storageErr != nil {
			resultErr = &purgeStorageError{storageErr}
		}
	}()
	tx, err := s.begin(ctx)
	if err != nil {
		return 0, "", err
	}
	defer tx.Rollback(ctx)
	// Candidate reads take no row locks. The version keyset also advances
	// within one parent with more than purgeBatch failed or locked versions.
	rows, err := tx.Query(ctx, `SELECT v.id,v.content_id FROM `+s.t.versions+` v
 JOIN `+s.t.items+` c ON c.tenant_id=v.tenant_id AND c.id=v.content_id
 WHERE v.tenant_id=$1 AND v.id>$3 AND v.state='deleted' AND v.deleted_at<$2
 AND (c.state<>'deleted' OR c.deleted_at IS NULL OR c.deleted_at>=$2)
 ORDER BY v.id LIMIT `+strconv.Itoa(purgeBatch), s.tenant, cutoff, after)
	if err != nil {
		return 0, "", err
	}
	var candidates, parentIDs []string
	for rows.Next() {
		var id, parent string
		if err := rows.Scan(&id, &parent); err != nil {
			rows.Close()
			return 0, "", err
		}
		candidates, parentIDs = append(candidates, id), append(parentIDs, parent)
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(candidates) == 0 {
		return 0, "", err
	}
	if len(candidates) == purgeBatch {
		next = candidates[len(candidates)-1]
	}
	parentIDs = slices.Compact(slices.Sorted(slices.Values(parentIDs)))
	rows, err = tx.Query(ctx, `SELECT id FROM `+s.t.items+`
 WHERE tenant_id=$1 AND id=ANY($3) AND (state<>'deleted' OR deleted_at IS NULL OR deleted_at>=$2)
 ORDER BY id FOR UPDATE SKIP LOCKED`, s.tenant, cutoff, parentIDs)
	if err != nil {
		return 0, "", err
	}
	parents, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return 0, "", err
	}
	if len(parents) == 0 {
		return 0, next, tx.Commit(ctx)
	}
	rows, err = tx.Query(ctx, `SELECT v.id,v.content_id,c.kind FROM `+s.t.versions+` v
 JOIN `+s.t.items+` c ON c.tenant_id=v.tenant_id AND c.id=v.content_id
 WHERE v.tenant_id=$1 AND v.id=ANY($3) AND v.content_id=ANY($4)
 AND v.state='deleted' AND v.deleted_at<$2
 ORDER BY v.id FOR UPDATE OF v SKIP LOCKED`, s.tenant, cutoff, candidates, parents)
	if err != nil {
		return 0, "", err
	}
	var vids, contents, kinds []string
	var targets []versions.Target
	var events []Event
	for rows.Next() {
		var vid, content, kind string
		if err := rows.Scan(&vid, &content, &kind); err != nil {
			rows.Close()
			return 0, "", err
		}
		vids, contents, kinds = append(vids, vid), append(contents, content), append(kinds, kind)
		targets = append(targets, versions.Target{Content: content, Version: vid})
		events = append(events, Event{Type: EventVersionPurged, Kind: kind, ContentID: content, VersionID: vid})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, "", err
	}
	if len(vids) == 0 {
		return 0, next, tx.Commit(ctx)
	}
	if s.media != nil {
		if err := s.media.Versions.DeleteTx(ctx, tx, targets); err != nil {
			return 0, "", err
		}
		if err := tx.Commit(ctx); err != nil {
			return 0, "", err
		}
		succeeded := make([]string, 0, len(vids))
		for _, target := range targets {
			if err := ctx.Err(); err != nil {
				return 0, "", err
			}
			if err := s.media.Versions.Delete(ctx, []versions.Target{target}); err != nil {
				storageErr = errors.Join(storageErr, fmt.Errorf("purge version %s: %w", target.Version, err))
				continue
			}
			succeeded = append(succeeded, target.Version)
		}
		vids = succeeded
		if len(vids) == 0 {
			return 0, next, nil
		}
		tx, err = s.begin(ctx)
		if err != nil {
			return 0, "", err
		}
		defer tx.Rollback(ctx)
		if _, err := tx.Exec(ctx, `SELECT FROM `+s.t.items+` WHERE tenant_id=$1 AND id=ANY($2) ORDER BY id FOR UPDATE`, s.tenant, parents); err != nil {
			return 0, "", err
		}
		rows, err = tx.Query(ctx, `SELECT v.id,v.content_id,c.kind FROM `+s.t.versions+` v
 JOIN `+s.t.items+` c ON c.tenant_id=v.tenant_id AND c.id=v.content_id
 WHERE v.tenant_id=$1 AND v.id=ANY($3) AND v.state='deleted' AND v.deleted_at<$2 ORDER BY v.id FOR UPDATE OF v SKIP LOCKED`, s.tenant, cutoff, vids)
		if err != nil {
			return 0, "", err
		}
		vids, contents, kinds, events = nil, nil, nil, nil
		for rows.Next() {
			var vid, content, kind string
			if err := rows.Scan(&vid, &content, &kind); err != nil {
				rows.Close()
				return 0, "", err
			}
			vids, contents, kinds = append(vids, vid), append(contents, content), append(kinds, kind)
			events = append(events, Event{Type: EventVersionPurged, Kind: kind, ContentID: content, VersionID: vid})
		}
		rows.Close()
		if err := rows.Err(); err != nil || len(vids) == 0 {
			return 0, next, err
		}
	}
	if err := s.markDocuments(ctx, tx, contents); err != nil {
		return 0, "", err
	}
	if err := s.eraseInteractions(ctx, tx, kinds, contents, vids); err != nil {
		return 0, "", err
	}
	if s.references != nil {
		if err := s.references.PurgeVersions(ctx, tx, s.tenant, vids); err != nil {
			return 0, "", err
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM `+s.t.versions+` WHERE tenant_id = $1 AND id = ANY($2)`, s.tenant, vids); err != nil {
		return 0, "", err
	}
	if err := s.purgeTx(ctx, tx, events); err != nil {
		return 0, "", err
	}
	if err := s.emit(ctx, tx, events...); err != nil {
		return 0, "", err
	}
	return len(vids), next, tx.Commit(ctx)
}

// eraseInteractions deletes purged content's comments (with their reactions
// and counts), reactions, favorites, counts and access history: of whole items
// when vids is nil, else of those versions (vids[i] of item ids[i]).
func (s *Store) eraseInteractions(ctx context.Context, tx pgx.Tx, kinds, ids, vids []string) error {
	target := `(content_kind, content_id) IN (SELECT * FROM unnest($2::text[], $3::text[]))`
	args := []any{s.tenant, kinds, ids}
	history, historyArgs := `content_id = ANY($2)`, []any{s.tenant, ids}
	if vids != nil {
		target = `(content_kind, content_id, content_version_id) IN (SELECT * FROM unnest($2::text[], $3::text[], $4::text[]))`
		args = append(args, vids)
		history, historyArgs = `version_id = ANY($2)`, []any{s.tenant, vids}
	}
	for _, q := range []string{
		`DELETE FROM ` + s.t.reactions + ` WHERE tenant_id = $1 AND content_kind = '` + content.KindComment + `' AND content_version_id IS NULL
 AND content_id IN (SELECT id::text FROM ` + s.t.comments + ` WHERE tenant_id = $1 AND ` + target + `)`,
		`DELETE FROM ` + s.t.counts + ` WHERE tenant_id = $1 AND content_kind = '` + content.KindComment + `' AND content_version_id IS NULL
 AND content_id IN (SELECT id::text FROM ` + s.t.comments + ` WHERE tenant_id = $1 AND ` + target + `)`,
		`DELETE FROM ` + s.t.comments + ` WHERE tenant_id = $1 AND ` + target,
		`DELETE FROM ` + s.t.reactions + ` WHERE tenant_id = $1 AND ` + target,
		`DELETE FROM ` + s.t.favorites + ` WHERE tenant_id = $1 AND ` + target,
		`DELETE FROM ` + s.t.counts + ` WHERE tenant_id = $1 AND ` + target,
	} {
		if _, err := tx.Exec(ctx, q, args...); err != nil {
			return err
		}
	}
	_, err := tx.Exec(ctx, `DELETE FROM `+s.t.accessHistory+` WHERE tenant_id = $1 AND `+history, historyArgs...)
	return err
}

// Queue is the River queue of content jobs. Keep its persisted routing name
// stable so jobs queued before the terminology change continue to run.
const Queue = "contentkit_catalog"

type publishDueArgs struct{}

func (publishDueArgs) Kind() string { return "contentkit_content_publish_due" }

// KindAliases preserve queued jobs from before the content terminology change.
// Remove these aliases once those jobs and their retries have drained.
func (publishDueArgs) KindAliases() []string { return []string{"contentkit_catalog_publish_due"} }

type purgeDeletedArgs struct{}

func (purgeDeletedArgs) Kind() string          { return "contentkit_content_purge_deleted" }
func (purgeDeletedArgs) KindAliases() []string { return []string{"contentkit_catalog_purge_deleted"} }

type jobWorker[T river.JobArgs] struct {
	river.WorkerDefaults[T]
	run     func(context.Context) (int, error)
	timeout time.Duration
}

func (w *jobWorker[T]) Timeout(*river.Job[T]) time.Duration { return w.timeout }

func (w *jobWorker[T]) Work(ctx context.Context, _ *river.Job[T]) error {
	_, err := w.run(ctx)
	return err
}

// RiverJobs contributes content's periodic jobs to the engine's River
// client: publication and audience-release facts (every 5m), event
// log retention and permanent purging of deleted content (4h). Restarts only
// catch up overdue work; reads enforce release times independently of this job.
func (s *Store) RiverJobs() riverhelpers.Contribution {
	return riverhelpers.NewContribution("contentkit-content", func(_ context.Context, cfg *river.Config) error {
		if q, ok := cfg.Queues[Queue]; ok && q.MaxWorkers < 1 {
			return fmt.Errorf("catalog: queue %q needs workers", Queue)
		} else if !ok {
			cfg.Queues[Queue] = river.QueueConfig{MaxWorkers: 2}
		}
		periodic := func(id string, every time.Duration, args river.JobArgs) *river.PeriodicJob {
			return riverhelpers.NewPeriodicJob(cfg, every, func() (river.JobArgs, *river.InsertOpts) {
				return args, &river.InsertOpts{Queue: Queue, MaxAttempts: 3, UniqueOpts: media.PendingOnce}
			}, &river.PeriodicJobOpts{ID: id, RunOnStart: true})
		}
		for _, add := range []func() error{
			func() error { return river.AddWorkerSafely(cfg.Workers, &jobWorker[publishDueArgs]{run: s.PublishDue}) },
			func() error {
				return river.AddWorkerSafely(cfg.Workers, &jobWorker[purgeDeletedArgs]{run: s.PurgeDeleted, timeout: 15 * time.Minute})
			},
		} {
			if err := add(); err != nil {
				return err
			}
		}
		cfg.PeriodicJobs = append(cfg.PeriodicJobs,
			periodic("contentkit_content_publish_due", 5*time.Minute, publishDueArgs{}),
			periodic("contentkit_content_purge_deleted", 4*time.Hour, purgeDeletedArgs{}))
		return nil
	}, nil, nil)
}
