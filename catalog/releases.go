package catalog

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/contentkit/access"
)

// recordReleases records effective audience transitions under parent locks.
// Receipts are independent of retained-log pruning and consumer cursors.
func (s *Store) recordReleases(ctx context.Context, tx pgx.Tx, ids []string) (int, error) {
	rows, err := tx.Query(ctx, `SELECT f.content_id,f.version_id,f.kind,f.live_from,release,`+s.qs+`.content_release_key(release),availability.reopened
 FROM `+s.t.facets+` f CROSS JOIN LATERAL jsonb_array_elements(f.access_releases) release
 LEFT JOIN LATERAL (SELECT h.occurred_at AS reopened FROM `+s.qs+`.content_access_history h
 WHERE h.tenant_id=f.tenant_id AND h.content_id=f.content_id AND h.version_id=f.version_id AND h.id>(
 SELECT max(prior.id) FROM `+s.qs+`.content_access_history prior WHERE prior.tenant_id=f.tenant_id AND prior.content_id=f.content_id AND prior.version_id=f.version_id
 AND (coalesce((prior.snapshot->>'deleted')::boolean,false) OR prior.snapshot->>'live_from' IS NULL
 OR NOT EXISTS (SELECT 1 FROM jsonb_array_elements(coalesce(prior.snapshot->'releases','[]'::jsonb)) previous_release
 WHERE `+s.qs+`.content_release_key(previous_release)=`+s.qs+`.content_release_key(release))))
 ORDER BY h.id LIMIT 1) availability ON true
 WHERE f.tenant_id=$1 AND f.content_id=ANY($2) AND NOT f.deleted
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
	return len(events), nil
}

// Retain already-effective audience facts before changing their policy. A
// passed publication date cannot mark unfinished storage publication complete.
func (s *Store) recordBeforeEdit(ctx context.Context, q Querier, ids []string, _ map[string]owned) error {
	tx, ok := q.(pgx.Tx)
	if !ok {
		return nil
	}
	_, err := s.recordReleases(ctx, tx, ids)
	return err
}
