package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/contentkit/access"
)

const releaseBatch = 200

// recordReleases records effective audience transitions under parent locks.
// Receipts are independent of retained-log pruning and consumer cursors.
func (s *Store) recordReleases(ctx context.Context, tx pgx.Tx, ids []string) (int, error) {
	rows, err := tx.Query(ctx, `SELECT f.content_id,f.version_id,f.kind,f.live_from,release,`+s.qs+`.content_release_key(release),availability.reopened
 FROM `+s.t.facets+` f CROSS JOIN LATERAL jsonb_array_elements(f.access_releases) release
 LEFT JOIN LATERAL (SELECT h.occurred_at AS reopened FROM `+s.qs+`.content_access_history h
 WHERE h.tenant_id=f.tenant_id AND h.content_id=f.content_id AND h.version_id=f.version_id AND h.id>(
 SELECT max(prior.id) FROM `+s.qs+`.content_access_history prior WHERE prior.tenant_id=f.tenant_id AND prior.content_id=f.content_id AND prior.version_id=f.version_id
 AND (coalesce((prior.snapshot->>'deleted')::boolean,false) OR coalesce((prior.snapshot->>'unplayable')::boolean,false) OR prior.snapshot->>'live_from' IS NULL
 OR NOT EXISTS (SELECT 1 FROM jsonb_array_elements(coalesce(prior.snapshot->'releases','[]'::jsonb)) previous_release
 WHERE `+s.qs+`.content_release_key(previous_release)=`+s.qs+`.content_release_key(release))))
 ORDER BY h.id LIMIT 1) availability ON true
 WHERE f.tenant_id=$1 AND f.content_id=ANY($2) AND NOT f.deleted AND NOT f.unplayable
 AND f.live_from<=$3 AND (release->>'at')::timestamptz<=$3
 ORDER BY f.content_id,f.version_id,(release->>'at')::timestamptz`, s.tenant, ids, s.now())
	if err != nil {
		return 0, err
	}
	type candidate struct {
		content, version, kind, key string
		live                        time.Time
		reopened                    *time.Time
		release                     access.Release
	}
	var candidates []candidate
	for rows.Next() {
		var c candidate
		var raw []byte
		if err := rows.Scan(&c.content, &c.version, &c.kind, &c.live, &raw, &c.key, &c.reopened); err != nil {
			rows.Close()
			return 0, err
		}
		if err := json.Unmarshal(raw, &c.release); err != nil {
			rows.Close()
			return 0, err
		}
		candidates = append(candidates, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	var events []Event
	for _, c := range candidates {
		at := c.release.At
		if c.live.After(at) {
			at = c.live
		}
		if c.reopened != nil && c.reopened.After(at) {
			at = *c.reopened
		}
		tag, err := tx.Exec(ctx, `INSERT INTO `+s.qs+`.content_release_receipts(tenant_id,content_id,version_id,release_key,live_from,occurred_at) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT DO NOTHING`, s.tenant, c.content, c.version, c.key, c.live, at)
		if err != nil {
			return 0, err
		}
		if tag.RowsAffected() == 0 {
			continue
		}
		release := c.release
		events = append(events, Event{Type: EventAudienceReleased, Kind: c.kind, ContentID: c.content, VersionID: c.version, OccurredAt: at, Release: &release})
	}
	if err := s.emit(ctx, tx, events...); err != nil {
		return 0, err
	}
	return len(events), s.refreshNextRelease(ctx, tx, ids)
}

func (s *Store) recordDueReleases(ctx context.Context) (int, error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	// Rows are claimed in the same order used by content edits. SKIP LOCKED
	// allows another pass to catch a busy writer without blocking the batch.
	rows, err := tx.Query(ctx, `SELECT c.id FROM `+s.t.items+` c WHERE c.tenant_id=$1 AND EXISTS (
 SELECT 1 FROM `+s.t.facets+` f WHERE f.tenant_id=c.tenant_id AND f.content_id=c.id AND f.next_release_at<=$2)
 ORDER BY c.id LIMIT `+fmt.Sprint(releaseBatch)+` FOR UPDATE SKIP LOCKED`, s.tenant, s.now())
	if err != nil {
		return 0, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return 0, err
	}
	n, err := s.recordReleases(ctx, tx, ids)
	if err != nil {
		return 0, err
	}
	if n > 0 {
		if err = s.exposeTx(ctx, tx, ids); err != nil {
			return 0, err
		}
	}
	return n, tx.Commit(ctx)
}

// A later edit must not erase a release that happened while the recorder slept.
func (s *Store) recordBeforeEdit(ctx context.Context, q Querier, ids []string, owned map[string]owned) error {
	tx, ok := q.(pgx.Tx)
	if !ok {
		return nil
	}
	rows, err := tx.Query(ctx, `UPDATE `+s.t.items+` SET published_at=publish_at
 WHERE tenant_id=$1 AND id=ANY($2) AND state='published' AND moderation='approved' AND published_at IS NULL AND publish_at<=$3 RETURNING id,kind,published_at`, s.tenant, ids, s.now())
	if err != nil {
		return err
	}
	var events []Event
	for rows.Next() {
		var id, kind string
		var at time.Time
		if err := rows.Scan(&id, &kind, &at); err != nil {
			rows.Close()
			return err
		}
		o := owned[id]
		o.publishedAt = true
		owned[id] = o
		events = append(events, Event{Type: EventContentPublished, Kind: kind, ContentID: id, OccurredAt: at})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	rows, err = tx.Query(ctx, `UPDATE `+s.t.versions+` v SET published_at=greatest(v.publish_at,coalesce(c.published_at,c.publish_at)) FROM `+s.t.items+` c
 WHERE c.tenant_id=v.tenant_id AND c.id=v.content_id AND v.tenant_id=$1 AND v.content_id=ANY($2)
 AND c.state='published' AND c.moderation='approved' AND coalesce(c.published_at,c.publish_at)<=$3
 AND v.state='published' AND v.published_at IS NULL AND v.publish_at<=$3 RETURNING v.content_id,v.id,c.kind,v.published_at`, s.tenant, ids, s.now())
	if err != nil {
		return err
	}
	for rows.Next() {
		var id, version, kind string
		var at time.Time
		if err := rows.Scan(&id, &version, &kind, &at); err != nil {
			rows.Close()
			return err
		}
		events = append(events, Event{Type: EventVersionPublished, Kind: kind, ContentID: id, VersionID: version, OccurredAt: at})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if err := s.emit(ctx, tx, events...); err != nil {
		return err
	}
	_, err = s.recordReleases(ctx, tx, ids)
	return err
}

// next_release_at is an indexed recorder projection, never an access decision.
func (s *Store) refreshNextRelease(ctx context.Context, tx pgx.Tx, ids []string) error {
	_, err := tx.Exec(ctx, `UPDATE `+s.t.facets+` f SET next_release_at=CASE WHEN NOT f.deleted AND NOT f.unplayable AND f.live_from IS NOT NULL THEN (
 SELECT min(greatest(f.live_from,(release->>'at')::timestamptz)) FROM jsonb_array_elements(f.access_releases) release
 WHERE NOT EXISTS (SELECT 1 FROM `+s.qs+`.content_release_receipts receipt WHERE receipt.tenant_id=f.tenant_id AND receipt.version_id=f.version_id
 AND receipt.live_from=f.live_from AND receipt.release_key=`+s.qs+`.content_release_key(release))) END
 WHERE f.tenant_id=$1 AND f.content_id=ANY($2)`, s.tenant, ids)
	return err
}
