package discoverystore

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/internal/entitystore"
	"github.com/open-rails/contentkit/signal"
)

// popularEntityKinds are the entity kinds ranked by this content ranking.
// Entities rank per domain over every language, so only scopes without a
// language have any.
func (s *Service) popularEntityKinds(key popularRanking) []string {
	if key.language != "" {
		return nil
	}
	var out []string
	for _, kind := range slices.Sorted(maps.Keys(s.o.Kinds.EntityKinds)) {
		if p := s.o.Kinds.EntityKinds[kind].Popular; p != nil && p.ContentKind == key.kind && p.Bucket == key.period {
			out = append(out, kind)
		}
	}
	return out
}

// entityRankingsStale reports an entity kind with active entities but no
// places from the content ranking computed then: none built yet, or a build
// that failed after the content ranking was stored.
func (s *Service) entityRankingsStale(ctx context.Context, key popularRanking, computed time.Time) (bool, error) {
	for _, kind := range s.popularEntityKinds(key) {
		var stale bool
		if err := s.o.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM `+s.t.entities+` WHERE tenant_id=$1 AND kind=$2 AND state='active')
 AND NOT EXISTS (SELECT 1 FROM `+s.t.entityRankings+` WHERE tenant_id=$1 AND entity_kind=$2 AND period=$3 AND domain=$4 AND language='' AND computed_at>=$5)`,
			s.o.Tenant, kind, key.period, key.domain, computed).Scan(&stale); err != nil || stale {
			return stale, err
		}
	}
	return false, nil
}

// buildEntityRankings places the active entities of each kind ranked by this
// content ranking: by the summed scores of their live content, then by that
// content's count, then by id. It runs with every rebuild of the content
// ranking and reuses its scores.
func (s *Service) buildEntityRankings(ctx context.Context, key popularRanking, hits []signal.PopularHit, computed time.Time) error {
	kinds := s.popularEntityKinds(key)
	if len(kinds) == 0 {
		return nil
	}
	ids, scores := make([]string, len(hits)), make([]float64, len(hits))
	for i, h := range hits {
		ids[i], scores[i] = h.ContentID, h.Score
	}
	var failures []error
	for _, kind := range kinds {
		args := pgx.NamedArgs{"ids": ids, "scores": scores, "tenant": s.o.Tenant, "content_kind": key.kind, "entity_kind": kind,
			"period": key.period, "domain": key.domain, "language": key.language, "computed": computed}
		err := pgx.BeginFunc(ctx, s.o.Pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `DELETE FROM `+s.t.entityRankings+` WHERE tenant_id=@tenant AND entity_kind=@entity_kind
 AND period=@period AND domain=@domain AND language=@language`, args); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `WITH scores AS (SELECT content_id, score FROM unnest(@ids::text[], @scores::float8[]) AS s(content_id, score)),
members AS (SELECT m.entity_id, f.content_id FROM `+s.t.facets+` f
 CROSS JOIN LATERAL unnest(array_append(f.entity_ids, f.owner_entity_id)) m(entity_id)
 WHERE f.tenant_id=@tenant AND f.kind=@content_kind AND m.entity_id IS NOT NULL AND `+entitystore.LiveListed+`
 GROUP BY m.entity_id, f.content_id),
ranked AS (SELECT e.id, round(coalesce(sum(s.score), 0)::numeric, 12) AS score, count(mb.content_id) AS n
 FROM `+s.t.entities+` e LEFT JOIN members mb ON mb.entity_id=e.id LEFT JOIN scores s ON s.content_id=mb.content_id
 WHERE e.tenant_id=@tenant AND e.kind=@entity_kind AND e.state='active' GROUP BY e.id)
INSERT INTO `+s.t.entityRankings+` (tenant_id,entity_kind,period,domain,language,entity_id,place,computed_at)
SELECT @tenant,@entity_kind,@period,@domain,@language,id,row_number() OVER (ORDER BY score DESC, n DESC, id),@computed FROM ranked
ON CONFLICT (tenant_id,entity_kind,period,domain,language,entity_id) DO UPDATE SET place=excluded.place,computed_at=excluded.computed_at`, args)
			return err
		})
		if err != nil {
			failures = append(failures, fmt.Errorf("discovery: store %s entity ranking %s: %w", kind, key.period, err))
		}
	}
	return errors.Join(failures...)
}
