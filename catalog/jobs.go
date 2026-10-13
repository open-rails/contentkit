package catalog

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"time"

	riverhelpers "github.com/open-rails/helpers/river"
	"github.com/riverqueue/river"

	"github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/media"
)

// PublishDue promotes scheduled content items and versions whose time came:
// it stamps published_at and fires the side effects of going live (facets,
// search documents, media exposure, events). Visibility itself never waits
// for it: a scheduled item is live from its time. It returns how many items
// and versions it promoted.
func (s *Store) PublishDue(ctx context.Context) (int, error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	now := s.now()
	var events []Event
	var ids []string
	rows, err := tx.Query(ctx, `UPDATE `+s.t.items+` SET published_at = publish_at, updated_at = clock_timestamp(), revision = revision + 1
 WHERE id IN (SELECT id FROM `+s.t.items+` WHERE tenant_id = $1 AND state = 'published' AND published_at IS NULL AND publish_at <= $2
  AND moderation = 'approved' ORDER BY publish_at LIMIT 500 FOR UPDATE SKIP LOCKED) AND tenant_id = $1 RETURNING id, kind`, s.tenant, now)
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var id, kind string
		if err := rows.Scan(&id, &kind); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
		events = append(events, Event{Type: EventContentPublished, Kind: kind, ContentID: id})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	rows, err = tx.Query(ctx, `UPDATE `+s.t.versions+` v SET published_at = v.publish_at, updated_at = clock_timestamp(), revision = v.revision + 1
 FROM `+s.t.items+` c WHERE v.id IN (SELECT id FROM `+s.t.versions+` WHERE tenant_id = $1 AND state = 'published' AND published_at IS NULL
  AND publish_at <= $2 ORDER BY publish_at LIMIT 500 FOR UPDATE SKIP LOCKED) AND v.tenant_id = $1 AND c.tenant_id = v.tenant_id AND c.id = v.content_id
 RETURNING v.content_id, v.id, c.kind`, s.tenant, now)
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var id, vid, kind string
		if err := rows.Scan(&id, &vid, &kind); err != nil {
			rows.Close()
			return 0, err
		}
		if !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
		events = append(events, Event{Type: EventVersionPublished, Kind: kind, ContentID: id, VersionID: vid})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(ids) == 0 {
		return 0, nil
	}
	if err := s.RefreshFacets(ctx, tx, ids); err != nil {
		return 0, err
	}
	if err := s.emit(ctx, tx, events...); err != nil {
		return 0, err
	}
	if err := s.exposeTx(ctx, tx, ids); err != nil {
		return 0, err
	}
	return len(events), tx.Commit(ctx)
}

// ReleaseFree runs what follows a free_after time passing: the content items
// whose versions turned free since the last run get their facets and
// documents re-derived and their media, body images included, exposed to
// everyone, as a write would. It returns how many items it released.
func (s *Store) ReleaseFree(ctx context.Context) (int, error) {
	total := 0
	for {
		n, err := s.releaseFree(ctx)
		if total += n; err != nil || n < releaseBatch {
			return total, err
		}
	}
}

const releaseBatch = 200

func (s *Store) releaseFree(ctx context.Context) (int, error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	now := s.now()
	rows, err := tx.Query(ctx, `SELECT c.id, c.kind FROM `+s.t.items+` c WHERE c.tenant_id = $1 AND c.id IN (SELECT content_id FROM `+s.t.facets+`
 WHERE tenant_id = $1 AND NOT free_applied AND access_free_after <= $2) ORDER BY c.id LIMIT `+strconv.Itoa(releaseBatch)+` FOR UPDATE SKIP LOCKED`, s.tenant, now)
	if err != nil {
		return 0, err
	}
	var ids []string
	var events []Event
	for rows.Next() {
		var id, kind string
		if err := rows.Scan(&id, &kind); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
		events = append(events, Event{Type: EventContentUpdated, Kind: kind, ContentID: id})
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(ids) == 0 {
		return 0, err
	}
	if err := s.RefreshFacets(ctx, tx, ids); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `UPDATE `+s.t.facets+` SET free_applied = true WHERE tenant_id = $1 AND content_id = ANY($2) AND access_free_after <= $3`,
		s.tenant, ids, now); err != nil {
		return 0, err
	}
	if err := s.emit(ctx, tx, events...); err != nil {
		return 0, err
	}
	if err := s.exposeTx(ctx, tx, ids); err != nil {
		return 0, err
	}
	return len(ids), tx.Commit(ctx)
}

// RefreshCounts copies each content item's interaction counts into its
// facets: likes, dislikes, favorites and rating (the Wilson lower bound of
// likes over likes and dislikes). It returns how many facet rows changed.
func (s *Store) RefreshCounts(ctx context.Context) (int, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE `+s.t.facets+` f SET likes = n.likes, dislikes = n.dislikes, favorites = n.favorites, rating = n.rating,
 updated_at = clock_timestamp()
 FROM (SELECT f.version_id, coalesce(c.likes, 0) AS likes, coalesce(c.dislikes, 0) AS dislikes, coalesce(c.favorites, 0) AS favorites,
  `+content.RatingSQL("coalesce(c.likes, 0)", "coalesce(c.dislikes, 0)")+` AS rating
  FROM `+s.t.facets+` f LEFT JOIN `+s.t.counts+` c ON c.tenant_id = f.tenant_id AND c.content_kind = f.kind AND c.content_id = f.content_id
   AND c.content_version_id = '' WHERE f.tenant_id = $1) n
 WHERE f.tenant_id = $1 AND f.version_id = n.version_id
  AND (f.likes, f.dislikes, f.favorites, f.rating) IS DISTINCT FROM (n.likes, n.dislikes, n.favorites, n.rating)`, s.tenant)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// PurgeDeleted erases content items deleted longer than the retention that
// were never sold: their media folders and their rows. Sold ones stay for
// their holders. It returns how many it erased.
func (s *Store) PurgeDeleted(ctx context.Context) (int, error) {
	return s.erase(ctx, `SELECT id, kind FROM `+s.t.items+` WHERE tenant_id = $1 AND state = 'deleted' AND NOT ever_sold AND deleted_at < $2
 ORDER BY id LIMIT 200 FOR UPDATE SKIP LOCKED`, s.tenant, s.now().Add(-s.retention))
}

// ExpireDrafts erases the drafts of kinds with a DraftTTL that never went
// live and were created longer than it ago: their media folders and their
// rows. It returns how many it erased.
func (s *Store) ExpireDrafts(ctx context.Context) (int, error) {
	var names []string
	var cutoffs []time.Time
	for _, name := range slices.Sorted(maps.Keys(s.cfg.Kinds)) {
		if ttl := s.cfg.Kinds[name].DraftTTL; ttl > 0 {
			names, cutoffs = append(names, name), append(cutoffs, s.now().Add(-ttl))
		}
	}
	if len(names) == 0 {
		return 0, nil
	}
	return s.erase(ctx, `SELECT c.id, c.kind FROM `+s.t.items+` c JOIN unnest($2::text[], $3::timestamptz[]) AS k(kind, cutoff) ON c.kind = k.kind
 WHERE c.tenant_id = $1 AND c.state = 'draft' AND c.first_published_at IS NULL AND NOT c.ever_sold AND c.created_at < k.cutoff
 ORDER BY c.id LIMIT 200 FOR UPDATE OF c SKIP LOCKED`, s.tenant, names, cutoffs)
}

// erase deletes the content items query selects (id, kind), locked, with
// their media folders and documents.
func (s *Store) erase(ctx context.Context, query string, args ...any) (int, error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	var ids []string
	var events []Event
	for rows.Next() {
		var id, kind string
		if err := rows.Scan(&id, &kind); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
		events = append(events, Event{Type: EventContentPurged, Kind: kind, ContentID: id})
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(ids) == 0 {
		return 0, err
	}
	if s.media != nil {
		if err := s.media.Versions.DeleteTx(ctx, tx, contentTargets(ids)); err != nil {
			return 0, err
		}
	}
	// Their documents go with them.
	if err := s.markDocuments(ctx, tx, ids); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM `+s.t.items+` WHERE tenant_id = $1 AND id = ANY($2)`, s.tenant, ids); err != nil {
		return 0, err
	}
	if err := s.emit(ctx, tx, events...); err != nil {
		return 0, err
	}
	return len(ids), tx.Commit(ctx)
}

// Queue is the River queue of the catalog's jobs.
const Queue = "contentkit_catalog"

type publishDueArgs struct{}

func (publishDueArgs) Kind() string { return "contentkit_catalog_publish_due" }

type deliverEventsArgs struct{}

func (deliverEventsArgs) Kind() string { return "contentkit_catalog_deliver_events" }

type refreshCountsArgs struct{}

func (refreshCountsArgs) Kind() string { return "contentkit_catalog_refresh_counts" }

type purgeDeletedArgs struct{}

func (purgeDeletedArgs) Kind() string { return "contentkit_catalog_purge_deleted" }

type expireDraftsArgs struct{}

func (expireDraftsArgs) Kind() string { return "contentkit_catalog_expire_drafts" }

type releaseFreeArgs struct{}

func (releaseFreeArgs) Kind() string { return "contentkit_catalog_release_free" }

type jobWorker[T river.JobArgs] struct {
	river.WorkerDefaults[T]
	run func(context.Context) (int, error)
}

func (w *jobWorker[T]) Work(ctx context.Context, _ *river.Job[T]) error {
	_, err := w.run(ctx)
	return err
}

// RiverJobs contributes the catalog's periodic jobs to the engine's River
// client: scheduled publication and free_after releases (every 30s), event
// delivery (5s), interaction counts (5m), purging deleted content (1h) and
// expiring drafts (10m).
func (s *Store) RiverJobs() riverhelpers.Contribution {
	return riverhelpers.NewContribution("contentkit-catalog", func(_ context.Context, cfg *river.Config) error {
		if q, ok := cfg.Queues[Queue]; ok && q.MaxWorkers < 1 {
			return fmt.Errorf("catalog: queue %q needs workers", Queue)
		} else if !ok {
			cfg.Queues[Queue] = river.QueueConfig{MaxWorkers: 2}
		}
		periodic := func(id string, every time.Duration, args river.JobArgs) *river.PeriodicJob {
			return river.NewPeriodicJob(river.PeriodicInterval(every), func() (river.JobArgs, *river.InsertOpts) {
				return args, &river.InsertOpts{Queue: Queue, MaxAttempts: 3, UniqueOpts: media.PendingOnce}
			}, &river.PeriodicJobOpts{ID: id, RunOnStart: true})
		}
		for _, add := range []func() error{
			func() error { return river.AddWorkerSafely(cfg.Workers, &jobWorker[publishDueArgs]{run: s.PublishDue}) },
			func() error {
				return river.AddWorkerSafely(cfg.Workers, &jobWorker[deliverEventsArgs]{run: s.DeliverEvents})
			},
			func() error {
				return river.AddWorkerSafely(cfg.Workers, &jobWorker[refreshCountsArgs]{run: s.RefreshCounts})
			},
			func() error {
				return river.AddWorkerSafely(cfg.Workers, &jobWorker[purgeDeletedArgs]{run: s.PurgeDeleted})
			},
			func() error {
				return river.AddWorkerSafely(cfg.Workers, &jobWorker[expireDraftsArgs]{run: s.ExpireDrafts})
			},
			func() error {
				return river.AddWorkerSafely(cfg.Workers, &jobWorker[releaseFreeArgs]{run: s.ReleaseFree})
			},
		} {
			if err := add(); err != nil {
				return err
			}
		}
		cfg.PeriodicJobs = append(cfg.PeriodicJobs,
			periodic("contentkit_catalog_publish_due", 30*time.Second, publishDueArgs{}),
			periodic("contentkit_catalog_deliver_events", 5*time.Second, deliverEventsArgs{}),
			periodic("contentkit_catalog_refresh_counts", 5*time.Minute, refreshCountsArgs{}),
			periodic("contentkit_catalog_purge_deleted", time.Hour, purgeDeletedArgs{}),
			periodic("contentkit_catalog_expire_drafts", 10*time.Minute, expireDraftsArgs{}),
			periodic("contentkit_catalog_release_free", 30*time.Second, releaseFreeArgs{}))
		return nil
	}, nil, nil)
}
