package discovery

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/internal/gate"
	"github.com/open-rails/contentkit/popularity"
	"github.com/open-rails/contentkit/signal"
)

// Ranking a catalog at real reading volume takes minutes, so popular lists,
// recommendation fill and personalized search read rankings built by a job.
const (
	shortRankingEvery = time.Hour
	longRankingEvery  = 24 * time.Hour
	// customRankingKept bounds how long a reader-chosen range is served.
	customRankingKept = 24 * time.Hour
	// maxCustomRankings bounds the reader-chosen ranges requested or kept, so
	// arbitrary ranges cannot queue unbounded ranking work.
	maxCustomRankings = 50
)

// popularRanking names one stored ranking: a preset period or a reader-chosen
// "YYYY-MM-DD/YYYY-MM-DD" range, for one reader scope.
type popularRanking struct {
	kind, period, domain, language string
}

func customPeriod(w signal.Window) string {
	return w.From.UTC().Format(time.DateOnly) + "/" + w.To.UTC().Format(time.DateOnly)
}

func (k popularRanking) preset() bool { return !strings.Contains(k.period, "/") }

func (k popularRanking) window(now time.Time) (signal.Window, error) {
	from, until, custom := strings.Cut(k.period, "/")
	if !custom {
		return popularity.WindowForPeriod(k.period, now)
	}
	start, err := time.Parse(time.DateOnly, from)
	if err != nil {
		return signal.Window{}, err
	}
	end, err := time.Parse(time.DateOnly, until)
	if err != nil {
		return signal.Window{}, err
	}
	w := signal.Between(start, end)
	return w, w.Validate()
}

func (s *Service) rankingFor(ctx context.Context, reader Reader, kind, period string) popularRanking {
	host := reader.Host
	if host == "" {
		host = gate.Domain(ctx)
	}
	domain, _ := s.o.Kinds.DomainFor(host)
	return popularRanking{kind: kind, period: period, domain: domain.Name, language: reader.Language}
}

// requestRanking records a wanted ranking for the job to build. Scopes outside
// the configured kinds and languages are never built.
func (s *Service) requestRanking(ctx context.Context, key popularRanking) error {
	var exists bool
	err := s.o.Pool.QueryRow(ctx, `SELECT true FROM `+s.t.rankings+` WHERE tenant_id=$1 AND kind=$2 AND period=$3 AND domain=$4 AND language=$5`,
		s.o.Tenant, key.kind, key.period, key.domain, key.language).Scan(&exists)
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if _, ok := s.o.Kinds.Kinds[key.kind]; !ok || key.language != "" && !slices.Contains(s.o.Kinds.Languages, key.language) {
		return nil
	}
	_, err = s.o.Pool.Exec(ctx, `INSERT INTO `+s.t.rankings+` (tenant_id,kind,period,domain,language,preset,requested_at)
 SELECT $1,$2,$3,$4,$5,$6,$7 WHERE $6 OR (SELECT count(*) FROM `+s.t.rankings+` WHERE tenant_id=$1 AND NOT preset)<$8
 ON CONFLICT DO NOTHING`, s.o.Tenant, key.kind, key.period, key.domain, key.language, key.preset(), s.now(), maxCustomRankings)
	return err
}

// rankingJoin exposes the stored ranking as activity(content_id, score), best
// first, scores in (1, 2) so they sort above any Wilson lower bound in [0, 1).
func (s *Service) rankingJoin(key popularRanking) (string, pgx.NamedArgs) {
	return `LEFT JOIN (SELECT r.content_id,2-r.ord::float8/(cardinality(p.content_ids)+1) AS score FROM ` + s.t.rankings + ` p
 CROSS JOIN LATERAL unnest(p.content_ids) WITH ORDINALITY AS r(content_id,ord)
 WHERE p.tenant_id=@d_rank_tenant AND p.kind=@d_rank_kind AND p.period=@d_rank_period AND p.domain=@d_rank_domain AND p.language=@d_rank_language) activity
 ON activity.content_id=f.content_id `, pgx.NamedArgs{"d_rank_tenant": s.o.Tenant, "d_rank_kind": key.kind, "d_rank_period": key.period,
			"d_rank_domain": key.domain, "d_rank_language": key.language}
}

// topRanked returns up to limit ids of the stored ranking, best first.
func (s *Service) topRanked(ctx context.Context, key popularRanking, limit int) ([]string, error) {
	if err := s.requestRanking(ctx, key); err != nil {
		return nil, err
	}
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
	if err := s.requestRanking(ctx, key); err != nil {
		return nil, err
	}
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

// RefreshPopularRankings drops expired reader-chosen ranges, then builds
// every requested ranking and every preset past its refresh interval,
// requested ones first. Each build is stored as it completes; one failing
// ranking does not hold back the rest.
func (s *Service) RefreshPopularRankings(ctx context.Context) error {
	if err := s.signals(); err != nil {
		return err
	}
	now := s.now()
	if _, err := s.o.Pool.Exec(ctx, `DELETE FROM `+s.t.rankings+` WHERE tenant_id=$1 AND NOT preset AND coalesce(computed_at,requested_at)<$2`,
		s.o.Tenant, now.Add(-customRankingKept)); err != nil {
		return err
	}
	// The slack keeps an hourly refresh from slipping a period behind its run.
	slack := 5 * time.Minute
	rows, err := s.o.Pool.Query(ctx, `SELECT kind,period,domain,language FROM `+s.t.rankings+`
 WHERE tenant_id=$1 AND (computed_at IS NULL OR preset AND computed_at<CASE WHEN period IN ('7d','30d') THEN $2::timestamptz ELSE $3::timestamptz END)
 ORDER BY computed_at NULLS FIRST`, s.o.Tenant, now.Add(slack-shortRankingEvery), now.Add(slack-longRankingEvery))
	if err != nil {
		return err
	}
	due, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (popularRanking, error) {
		var k popularRanking
		return k, row.Scan(&k.kind, &k.period, &k.domain, &k.language)
	})
	if err != nil {
		return err
	}
	var errs []error
	for _, key := range due {
		if err := s.buildRanking(ctx, key); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// buildRanking ranks key's window and stores it; without trusted audience
// history the ranking is stored empty. A key that no longer names a
// configured kind or a valid window is removed.
func (s *Service) buildRanking(ctx context.Context, key popularRanking) error {
	w, err := key.window(s.now())
	if _, ok := s.o.Kinds.Kinds[key.kind]; err != nil || !ok {
		_, err := s.o.Pool.Exec(ctx, `DELETE FROM `+s.t.rankings+` WHERE tenant_id=$1 AND kind=$2 AND period=$3 AND domain=$4 AND language=$5`,
			s.o.Tenant, key.kind, key.period, key.domain, key.language)
		return err
	}
	computed := s.now()
	ids, err := popularity.Rank(ctx, s.o.Hub, s, key.kind, w, signal.ActivityScope{Domain: key.domain, Language: key.language}, PopularityDepth)
	if err != nil {
		return fmt.Errorf("discovery: rank %s %s: %w", key.kind, key.period, err)
	}
	if ids == nil {
		ids = []string{}
	}
	_, err = s.o.Pool.Exec(ctx, `INSERT INTO `+s.t.rankings+` (tenant_id,kind,period,domain,language,preset,requested_at,computed_at,content_ids)
 VALUES ($1,$2,$3,$4,$5,$6,$7,$7,$8)
 ON CONFLICT (tenant_id,kind,period,domain,language) DO UPDATE SET computed_at=excluded.computed_at,content_ids=excluded.content_ids`,
		s.o.Tenant, key.kind, key.period, key.domain, key.language, key.preset(), computed, ids)
	return err
}
