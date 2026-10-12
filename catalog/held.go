package catalog

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/contentref"
)

// Held pages held content items oldest first, for content's review queue
// (content.HeldSource under HeldKind): the text shown is the site's first
// language's, else any.
func (s *Store) Held(ctx context.Context, after time.Time, afterID string, limit int) ([]content.HeldItem, error) {
	rows, err := s.pool.Query(ctx, `SELECT c.id, c.kind, coalesce(c.created_by, ''), coalesce(t.title, ''), coalesce(t.body, ''),
 coalesce(c.moderation_reason, ''), c.moderation_verdict, coalesce(c.moderated_at, c.updated_at), c.created_at, c.moderation_revision
 FROM `+s.t.items+` c LEFT JOIN LATERAL (SELECT title, trim(description || E'\n' || body) AS body FROM `+s.t.text+` x
  WHERE x.tenant_id = c.tenant_id AND x.content_id = c.id ORDER BY x.language = $5 DESC, x.language LIMIT 1) t ON true
 WHERE c.tenant_id = $1 AND c.moderation = 'held' AND c.state <> 'deleted' AND (c.created_at, c.id) > ($2, $3)
 ORDER BY c.created_at, c.id LIMIT $4`, s.tenant, after, afterID, limit, s.defaultLanguage())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []content.HeldItem
	for rows.Next() {
		it := content.HeldItem{Kind: HeldKind}
		var kind string
		var verdict []byte
		if err := rows.Scan(&it.ID, &kind, &it.AuthorID, &it.Title, &it.Body, &it.Reason, &verdict, &it.HeldAt, &it.CreatedAt, &it.Revision); err != nil {
			return nil, err
		}
		it.Ref = contentref.New(s.tenant, kind, it.ID)
		it.SetVerdict(verdict)
		out = append(out, it)
	}
	return out, rows.Err()
}

// ResolveHeld applies a reviewer's decision to a held content item at the
// revision the queue listed: approval publishes it (or its edit), rejection
// keeps it from going live (an edit of a live item leaves its last approved
// text public).
func (s *Store) ResolveHeld(ctx context.Context, id string, d content.ReviewDecision) error {
	state := content.ModerationApproved
	if d.Decision == content.DecisionReject {
		state = content.ModerationRejected
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	now := s.now()
	var kind string
	var live bool
	err = tx.QueryRow(ctx, `UPDATE `+s.t.items+` SET moderation = $3,
 moderation_reason = CASE WHEN $3 = 'approved' THEN NULL ELSE coalesce(nullif($4, ''), moderation_reason) END,
 published_snapshot = CASE WHEN $3 = 'approved' THEN NULL ELSE published_snapshot END,
 publish_at = CASE WHEN $3 = 'approved' AND state = 'published' AND published_at IS NULL AND publish_at <= $7 THEN $7 ELSE publish_at END,
 published_at = CASE WHEN $3 = 'approved' AND state = 'published' AND published_at IS NULL AND publish_at <= $7 THEN $7 ELSE published_at END,
 moderated_by = $5, moderated_at = $7, moderation_revision = moderation_revision + 1, revision = revision + 1, updated_at = clock_timestamp()
 WHERE tenant_id = $1 AND id = $2 AND moderation = 'held' AND state <> 'deleted' AND moderation_revision = $6
 RETURNING kind, published_at = $7`, s.tenant, id, state, d.Reason, d.Reviewer, d.Revision, now).Scan(&kind, &live)
	if errors.Is(err, pgx.ErrNoRows) {
		return content.ErrNotFound
	}
	if err != nil {
		return err
	}
	events := []Event{{Type: EventContentUpdated, Kind: kind, ContentID: id}}
	if live {
		events = append(events, Event{Type: EventContentPublished, Kind: kind, ContentID: id})
	}
	if err := s.RefreshFacets(ctx, tx, []string{id}); err != nil {
		return err
	}
	if err := s.putCodes(ctx, tx, []string{id}); err != nil {
		return err
	}
	if err := s.emit(ctx, tx, events...); err != nil {
		return err
	}
	if err := s.exposeTx(ctx, tx, []string{id}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

var _ content.HeldSource = (*Store)(nil)
