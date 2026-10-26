package catalog

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/media/versions"
)

// PublicationScheduler admits every content kind through the same durable request.
type PublicationScheduler interface {
	SchedulePublicationTx(context.Context, pgx.Tx, []versions.Target) error
}

// BindPublication installs the engine's publication service independently of media.
// Bind it before accepting content writes.
func (s *Store) BindPublication(p PublicationScheduler) error {
	if p == nil {
		return errors.New("catalog: publication scheduler is required")
	}
	s.publicationMu.Lock()
	defer s.publicationMu.Unlock()
	if s.publication != nil {
		return errors.New("catalog: publication scheduler already bound")
	}
	s.publication = p
	return nil
}

func (s *Store) schedulePublicationTx(ctx context.Context, tx pgx.Tx, targets []versions.Target) error {
	if len(targets) == 0 {
		return nil
	}
	s.publicationMu.RLock()
	p := s.publication
	s.publicationMu.RUnlock()
	if p == nil {
		return errors.New("catalog: publication scheduler is not bound")
	}
	return p.SchedulePublicationTx(ctx, tx, targets)
}

// CompletePublicationTx records actual availability after the publisher has
// committed its completed release in this transaction. Chapter/request
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

// PublicationCompletedTx records a meaningful publication after its
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
