package discoverystore

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/discovery"
	"github.com/open-rails/contentkit/internal/entitystore"
	signalstore "github.com/open-rails/contentkit/internal/signalstore"
	"github.com/open-rails/contentkit/kinds"
	"github.com/open-rails/contentkit/signal"
)

type entityRanking struct{ kind, period, domain, language string }

// entityRankingEvery is how often an entity kind's ranking is rebuilt.
const entityRankingEvery = 24 * time.Hour

// refreshEntityRankings ranks each popular entity kind daily in every scope of
// its bucket.
func (s *Service) refreshEntityRankings(ctx context.Context, now time.Time) error {
	var failures []error
	for _, kind := range slices.Sorted(maps.Keys(s.o.Kinds.EntityKinds)) {
		p := s.o.Kinds.EntityKinds[kind].Popular
		if p == nil {
			continue
		}
		bucket, _ := s.o.Kinds.PopularBucket(*p)
		from, until := bucket.Window(now)
		for _, scope := range bucket.ActivityScopes() {
			key := entityRanking{kind, bucket.ID, scope.Domain, scope.Language}
			var computed, wFrom, wUntil *time.Time
			err := s.o.Pool.QueryRow(ctx, `SELECT computed_at,window_from,window_until FROM `+s.t.entityRankings+`
 WHERE tenant_id=$1 AND entity_kind=$2 AND period=$3 AND domain=$4 AND language=$5 LIMIT 1`,
				s.o.Tenant, key.kind, key.period, key.domain, key.language).Scan(&computed, &wFrom, &wUntil)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				failures = append(failures, err)
				continue
			}
			if computed != nil && computed.Add(entityRankingEvery).After(now) && sameBound(wFrom, from) && sameBound(wUntil, until) {
				continue
			}
			if err := s.buildEntityRanking(ctx, key, *p, signalstore.Between(from, until), now); err != nil {
				failures = append(failures, err)
			}
		}
	}
	return errors.Join(failures...)
}

// buildEntityRanking places a kind's active entities by the summed scores of
// their live content in the window, then by that content's count, then by id.
func (s *Service) buildEntityRanking(ctx context.Context, key entityRanking, p kinds.EntityPopularity, window signal.Window, computed time.Time) error {
	hits, err := s.o.Hub.DailyPopular(ctx, p.ContentKind, window, signal.ActivityScope{Domain: key.domain, Language: key.language}, discovery.PopularityDepth)
	if err != nil {
		return fmt.Errorf("discovery: rank %s entities %s: %w", key.kind, key.period, err)
	}
	ids, scores := make([]string, 0, len(hits)), make([]float64, 0, len(hits))
	for _, h := range hits {
		if h.Score > 0 {
			ids, scores = append(ids, h.ContentID), append(scores, h.Score)
		}
	}
	args := pgx.NamedArgs{"ids": ids, "scores": scores, "tenant": s.o.Tenant, "content_kind": p.ContentKind, "entity_kind": key.kind,
		"period": key.period, "domain": key.domain, "language": key.language, "computed": computed,
		"from": optionalBound(window.From), "until": optionalBound(window.To)}
	err = pgx.BeginFunc(ctx, s.o.Pool, func(tx pgx.Tx) error {
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
INSERT INTO `+s.t.entityRankings+` (tenant_id,entity_kind,period,domain,language,entity_id,place,computed_at,window_from,window_until)
SELECT @tenant,@entity_kind,@period,@domain,@language,id,row_number() OVER (ORDER BY score DESC, n DESC, id),@computed,@from,@until FROM ranked`, args)
		return err
	})
	if err != nil {
		return fmt.Errorf("discovery: store %s entity ranking %s: %w", key.kind, key.period, err)
	}
	return nil
}
