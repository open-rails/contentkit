package catalog

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/versions"
)

type publicationScheduler interface {
	SchedulePublicationTx(context.Context, pgx.Tx, []versions.Target) error
}

// Media binding is application-wide, but storage is declared per kind. A text
// kind in the same application still uses the catalog's durable text worker.
// A declared media kind never falls back after a processing/storage error.
func (s *Store) mediaPublicationKind(kind string) bool {
	if s.media == nil {
		return false
	}
	if s.media.Registry == nil {
		return true // legacy/custom bindings do not declare a text-only boundary
	}
	_, err := s.media.Registry.Kind(kind)
	return !errors.Is(err, media.ErrUnknownKind)
}

func (s *Store) textPublicationKinds() []string {
	kinds := []string{}
	for kind := range s.cfg.Kinds {
		if !s.mediaPublicationKind(kind) {
			kinds = append(kinds, kind)
		}
	}
	slices.Sort(kinds)
	return kinds
}

func (s *Store) schedulePublicationTx(ctx context.Context, tx pgx.Tx, targets []versions.Target) error {
	if s.media == nil {
		return s.scheduleTextPublicationTx(ctx, tx, targets)
	}
	targets = slices.CompactFunc(slices.SortedFunc(slices.Values(targets), func(a, b versions.Target) int {
		if a.Content < b.Content || a.Content == b.Content && a.Version < b.Version {
			return -1
		}
		if a == b {
			return 0
		}
		return 1
	}), func(a, b versions.Target) bool { return a == b })
	ids := make([]string, len(targets))
	for i, target := range targets {
		ids[i] = target.Content
	}
	rows, err := tx.Query(ctx, `SELECT id,kind FROM `+s.t.items+` WHERE tenant_id=$1 AND id=ANY($2)`, s.tenant, ids)
	if err != nil {
		return err
	}
	kinds := map[string]string{}
	for rows.Next() {
		var id, kind string
		if err := rows.Scan(&id, &kind); err != nil {
			rows.Close()
			return err
		}
		kinds[id] = kind
	}
	if err := rows.Err(); err != nil {
		return err
	}
	var text, stored []versions.Target
	for _, target := range targets {
		kind, exists := kinds[target.Content]
		if !exists {
			continue
		}
		if s.mediaPublicationKind(kind) {
			stored = append(stored, target)
		} else {
			text = append(text, target)
		}
	}
	if len(text) > 0 {
		if err := s.scheduleTextPublicationTx(ctx, tx, text); err != nil {
			return err
		}
	}
	if len(stored) == 0 {
		return nil
	}
	scheduler, ok := s.media.Versions.(publicationScheduler)
	if !ok {
		return errors.New("catalog: media does not support durable publication")
	}
	return scheduler.SchedulePublicationTx(ctx, tx, stored)
}

// Text-only catalogs use the existing publication worker. Intent and a
// scheduled River row commit together, even when no local worker is running.
func (s *Store) scheduleTextPublicationTx(ctx context.Context, tx pgx.Tx, targets []versions.Target) error {
	s.publicationMu.RLock()
	client := s.publicationJobs
	s.publicationMu.RUnlock()
	contents, ids := make([]string, len(targets)), make([]string, len(targets))
	for i, target := range targets {
		contents[i], ids[i] = target.Content, target.Version
	}
	rows, err := tx.Query(ctx, `SELECT DISTINCT v.content_id,v.id,
 CASE WHEN NOT v.released AND c.state='published' AND v.state='published'
      THEN greatest(coalesce(c.publish_at,$4),coalesce(v.publish_at,$4)) ELSE $4 END
 FROM `+s.t.versions+` v JOIN `+s.t.items+` c ON c.tenant_id=v.tenant_id AND c.id=v.content_id
 JOIN unnest($2::text[],$3::text[]) AS target(content_id,version_id)
 ON target.content_id=v.content_id AND (target.version_id='' OR target.version_id=v.id)
 WHERE v.tenant_id=$1 AND ((c.state='published' AND v.state='published') OR v.released)
 ORDER BY v.content_id,v.id`, s.tenant, contents, ids, s.now())
	if err != nil {
		return err
	}
	type pending struct {
		content, version string
		at               time.Time
	}
	requests, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (pending, error) {
		var p pending
		err := row.Scan(&p.content, &p.version, &p.at)
		return p, err
	})
	if err != nil {
		return err
	}
	if len(requests) == 0 {
		return nil
	}
	if client == nil {
		return errors.New("catalog: durable publication jobs are not composed into a River client")
	}
	for _, request := range requests {
		_, err := client.InsertTx(ctx, tx, publishDueArgs{Content: request.content, Version: request.version, Due: request.at},
			&river.InsertOpts{Queue: QueueFor(s.tenant), ScheduledAt: request.at, UniqueOpts: media.PendingOnce})
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) publishTextDue(ctx context.Context) (int, error) {
	kinds := s.textPublicationKinds()
	if len(kinds) == 0 {
		return 0, nil
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	at := s.now()
	rows, err := tx.Query(ctx, `SELECT c.id FROM `+s.t.items+` c
 WHERE c.tenant_id=$1 AND c.kind=ANY($3) AND EXISTS (SELECT 1 FROM `+s.t.versions+` v WHERE v.tenant_id=c.tenant_id AND v.content_id=c.id
 AND ((c.state='published' AND v.state='published' AND (
       (NOT v.released AND coalesce(c.publish_at,$2)<=$2 AND coalesce(v.publish_at,$2)<=$2)
       OR (v.released AND v.released_access IS DISTINCT FROM `+s.qs+`.content_frozen_access(v.access,$2))))
   OR (v.released AND (c.state<>'published' OR v.state<>'published'))))
 ORDER BY c.id LIMIT 200 FOR UPDATE SKIP LOCKED`, s.tenant, at, kinds)
	if err != nil {
		return 0, err
	}
	parents, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return 0, err
	}
	rows, err = tx.Query(ctx, `SELECT v.content_id,v.id,c.state='published' AND v.state='published'
 FROM `+s.t.versions+` v JOIN `+s.t.items+` c ON c.tenant_id=v.tenant_id AND c.id=v.content_id
 WHERE v.tenant_id=$1 AND v.content_id=ANY($2)
 AND ((c.state='published' AND v.state='published' AND (
       (NOT v.released AND coalesce(c.publish_at,$3)<=$3 AND coalesce(v.publish_at,$3)<=$3)
       OR (v.released AND v.released_access IS DISTINCT FROM `+s.qs+`.content_frozen_access(v.access,$3))))
   OR (v.released AND (c.state<>'published' OR v.state<>'published')))
 ORDER BY v.content_id,v.id FOR UPDATE OF v`, s.tenant, parents, at)
	if err != nil {
		return 0, err
	}
	type transition struct {
		target versions.Target
		open   bool
	}
	work, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (transition, error) {
		var t transition
		err := row.Scan(&t.target.Content, &t.target.Version, &t.open)
		return t, err
	})
	if err != nil {
		return 0, err
	}
	for _, item := range work {
		if item.open {
			err = s.CompletePublicationTx(ctx, tx, item.target, at)
		} else {
			err = s.WithdrawPublicationTx(ctx, tx, item.target, false)
		}
		if err != nil {
			return 0, err
		}
	}
	return len(work), tx.Commit(ctx)
}

// CompletePublicationTx records actual availability after the publisher has
// committed its active released-file set in this transaction. Chapter/request
// idempotency belongs to that set; this method records first-publication facts
// only on the corresponding row transition and refreshes discovery atomically.
func (s *Store) CompletePublicationTx(ctx context.Context, tx pgx.Tx, target versions.Target, at time.Time) error {
	if tx == nil || target.Content == "" || target.Version == "" || at.IsZero() || at.After(s.now()) {
		return invalid("publication completion needs a transaction, version and actual completion time")
	}
	var kind, parentState, state string
	var released bool
	var parentAt, versionAt, parentPublished, versionPublished *time.Time
	if err := tx.QueryRow(ctx, `SELECT kind,state,publish_at,published_at FROM `+s.t.items+`
 WHERE tenant_id=$1 AND id=$2 FOR UPDATE`, s.tenant, target.Content).
		Scan(&kind, &parentState, &parentAt, &parentPublished); err != nil {
		return missing(err)
	}
	if err := tx.QueryRow(ctx, `SELECT state,publish_at,published_at,released FROM `+s.t.versions+`
 WHERE tenant_id=$1 AND content_id=$2 AND id=$3 FOR UPDATE`, s.tenant, target.Content, target.Version).
		Scan(&state, &versionAt, &versionPublished, &released); err != nil {
		return missing(err)
	}
	if parentState != StatePublished || state != StatePublished ||
		!released && (parentAt != nil && parentAt.After(at) || versionAt != nil && versionAt.After(at)) {
		return ErrConflict
	}
	if !s.mediaPublicationKind(kind) {
		if _, err := tx.Exec(ctx, `UPDATE `+s.t.versions+` SET released_access=`+s.qs+`.content_frozen_access(access,$4)
 WHERE tenant_id=$1 AND content_id=$2 AND id=$3`, s.tenant, target.Content, target.Version, at); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE `+s.t.versions+` SET released=true,published_at=coalesce(published_at,$4),
 revision=revision+1,updated_at=clock_timestamp() WHERE tenant_id=$1 AND content_id=$2 AND id=$3`,
		s.tenant, target.Content, target.Version, at); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE `+s.t.items+` SET published_at=coalesce(published_at,$3),
 revision=revision+1,updated_at=clock_timestamp() WHERE tenant_id=$1 AND id=$2`, s.tenant, target.Content, at); err != nil {
		return err
	}
	var events []Event
	if parentPublished == nil {
		events = append(events, Event{Type: EventContentPublished, Kind: kind, ContentID: target.Content, OccurredAt: at})
	}
	if versionPublished == nil {
		events = append(events, Event{Type: EventVersionPublished, Kind: kind, ContentID: target.Content, VersionID: target.Version, OccurredAt: at})
	}
	if err := s.emit(ctx, tx, events...); err != nil {
		return err
	}
	if err := s.RefreshFacets(ctx, tx, []string{target.Content}); err != nil {
		return err
	}
	if !s.mediaPublicationKind(kind) {
		var next *time.Time
		if err := tx.QueryRow(ctx, `SELECT min((release->>'at')::timestamptz) FROM `+s.t.versions+` v,
 LATERAL jsonb_array_elements(coalesce(v.access->'releases','[]'::jsonb)) release
 WHERE v.tenant_id=$1 AND v.id=$2 AND (release->>'at')::timestamptz>$3`, s.tenant, target.Version, at).Scan(&next); err != nil {
			return err
		}
		if next != nil {
			s.publicationMu.RLock()
			client := s.publicationJobs
			s.publicationMu.RUnlock()
			if client == nil {
				return errors.New("catalog: durable publication jobs are not composed into a River client")
			}
			_, err := client.InsertTx(ctx, tx, publishDueArgs{Content: target.Content, Version: target.Version, Due: *next},
				&river.InsertOpts{Queue: QueueFor(s.tenant), ScheduledAt: *next, UniqueOpts: media.PendingOnce})
			return err
		}
	}
	return nil
}

// WithdrawPublicationTx updates current availability after the publisher has
// withdrawn files and committed the remaining active release set. A chapter
// withdrawal leaves other released chapters visible. Historical dates remain.
func (s *Store) WithdrawPublicationTx(ctx context.Context, tx pgx.Tx, target versions.Target, hasReleased bool) error {
	if tx == nil || target.Content == "" || target.Version == "" {
		return invalid("publication withdrawal needs a transaction and version")
	}
	var parent string
	if err := tx.QueryRow(ctx, `SELECT id FROM `+s.t.items+` WHERE tenant_id=$1 AND id=$2 FOR UPDATE`, s.tenant, target.Content).Scan(&parent); err != nil {
		return missing(err)
	}
	tag, err := tx.Exec(ctx, `UPDATE `+s.t.versions+` SET released=$4,revision=revision+1,updated_at=clock_timestamp()
 WHERE tenant_id=$1 AND content_id=$2 AND id=$3`, s.tenant, target.Content, target.Version, hasReleased)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return s.RefreshFacets(ctx, tx, []string{target.Content})
}

func missing(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// PublicationCompletedTx records a meaningful storage publication after its
// final copy/retirement work. The caller owns the locked retiring->completed
// transition and calls this once in that transaction, never for a no-op or an
// already completed retry. Request history supplies deduplication provenance;
// no consumer receipt or second operation table is created.
func (s *Store) PublicationCompletedTx(ctx context.Context, tx pgx.Tx, target versions.Target, requestID string, chapters []string, released bool, at time.Time) error {
	id, err := uuid.Parse(requestID)
	if tx == nil || err != nil || id == uuid.Nil || id.String() != requestID || at.IsZero() || at.After(s.now()) {
		return invalid("publication fact requires the completed request transaction and actual time")
	}
	var kind string
	var actual time.Time
	var available bool
	err = tx.QueryRow(ctx, `SELECT c.kind,r.completed_at,coalesce((r.result->>'released')::boolean,false)
 FROM `+s.qs+`.content_publication_requests r JOIN `+s.t.items+` c ON c.tenant_id=r.tenant_id AND c.id=r.content_id
 WHERE r.tenant_id=$1 AND r.id=$2 AND r.content_id=$3 AND r.version_id=$4
 AND r.state='completed' AND r.completed_at IS NOT NULL AND r.result IS NOT NULL FOR UPDATE OF r`,
		s.tenant, id, target.Content, target.Version).Scan(&kind, &actual, &available)
	if err != nil {
		return missing(err)
	}
	if available != released {
		return ErrConflict
	}
	return s.emit(ctx, tx, Event{Type: EventPublicationCompleted, Kind: kind, ContentID: target.Content,
		VersionID: target.Version, OccurredAt: actual, PublicationID: requestID, ChapterIDs: slices.Clone(chapters), Released: &released})
}
