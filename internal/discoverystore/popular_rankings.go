package discoverystore

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	discovery "github.com/open-rails/contentkit/discovery"
	"github.com/open-rails/contentkit/internal/gate"
)

type popularRanking struct{ kind, period, domain, language string }

func (s *Service) rankingFor(ctx context.Context, reader discovery.Reader, kind, id string) (popularRanking, bool) {
	host := reader.Host
	if host == "" {
		host = gate.Domain(ctx)
	}
	domain, _ := s.o.Kinds.DomainFor(host)
	for _, bucket := range s.o.Kinds.Kinds[kind].Popularity {
		if id != "" && bucket.ID != id {
			continue
		}
		if scope, ok := bucket.Scope(domain.Name, reader.Language); ok {
			return popularRanking{kind, bucket.ID, scope.Domain, scope.Language}, true
		}
	}
	return popularRanking{}, false
}

func (s *Service) rankingSnapshot(ctx context.Context, key popularRanking) (*discovery.PopularitySnapshot, error) {
	snapshot := &discovery.PopularitySnapshot{Bucket: key.period}
	err := s.o.Pool.QueryRow(ctx, `SELECT window_from,window_until,covered_from,covered_until,available_from,computed_at FROM `+s.t.rankings+`
 WHERE tenant_id=$1 AND kind=$2 AND period=$3 AND domain=$4 AND language=$5`,
		s.o.Tenant, key.kind, key.period, key.domain, key.language).Scan(&snapshot.WindowFrom, &snapshot.WindowUntil, &snapshot.From, &snapshot.Until, &snapshot.AvailableFrom, &snapshot.ComputedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return snapshot, nil
	}
	// PostgreSQL timestamps may decode in the host's local zone. These public
	// boundaries are UTC calendar dates regardless of the process time zone.
	for _, at := range []*time.Time{snapshot.WindowFrom, snapshot.WindowUntil, snapshot.From, snapshot.Until, snapshot.AvailableFrom, snapshot.ComputedAt} {
		if at != nil {
			*at = at.UTC()
		}
	}
	snapshot.Ready = snapshot.ComputedAt != nil && snapshot.From != nil
	if snapshot.Until != nil && snapshot.ComputedAt != nil {
		snapshot.Provisional = snapshot.Until.Add(24 * time.Hour).After(*snapshot.ComputedAt)
	}
	return snapshot, err
}

// rankingJoin exposes the stored ranking as activity(content_id, score), with
// positive scores decreasing by position, matching rankedScores.
func (s *Service) rankingJoin(key popularRanking) (string, pgx.NamedArgs) {
	return `LEFT JOIN (SELECT r.content_id,1.0/r.ord::float8 AS score FROM ` + s.t.rankings + ` p
 CROSS JOIN LATERAL unnest(p.content_ids) WITH ORDINALITY AS r(content_id,ord)
 WHERE p.tenant_id=@d_rank_tenant AND p.kind=@d_rank_kind AND p.period=@d_rank_period AND p.domain=@d_rank_domain AND p.language=@d_rank_language) activity
 ON activity.content_id=f.content_id `, pgx.NamedArgs{"d_rank_tenant": s.o.Tenant, "d_rank_kind": key.kind, "d_rank_period": key.period,
			"d_rank_domain": key.domain, "d_rank_language": key.language}
}

// topRanked returns up to limit ids of the stored ranking, best first.
func (s *Service) topRanked(ctx context.Context, key popularRanking, limit int) ([]string, error) {
	var ids []string
	err := s.o.Pool.QueryRow(ctx, `SELECT content_ids[1:$6] FROM `+s.t.rankings+` WHERE tenant_id=$1 AND kind=$2 AND period=$3 AND domain=$4 AND language=$5`,
		s.o.Tenant, key.kind, key.period, key.domain, key.language, limit).Scan(&ids)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return ids, err
}

// rankedScores scores ids by their place in the stored ranking, higher
// first; unranked ids are absent.
func (s *Service) rankedScores(ctx context.Context, key popularRanking, ids []string) (map[string]float64, error) {
	rows, err := s.o.Pool.Query(ctx, `SELECT r.content_id,r.ord FROM `+s.t.rankings+` p CROSS JOIN LATERAL unnest(p.content_ids) WITH ORDINALITY AS r(content_id,ord)
 WHERE p.tenant_id=$1 AND p.kind=$2 AND p.period=$3 AND p.domain=$4 AND p.language=$5 AND r.content_id=ANY($6)`,
		s.o.Tenant, key.kind, key.period, key.domain, key.language, ids)
	if err != nil {
		return nil, err
	}
	out := map[string]float64{}
	for rows.Next() {
		var id string
		var ord int64
		if err := rows.Scan(&id, &ord); err != nil {
			rows.Close()
			return nil, err
		}
		out[id] = 1 / float64(ord)
	}
	rows.Close()
	return out, rows.Err()
}
