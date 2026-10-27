package discovery

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/internal/gate"
	"github.com/open-rails/contentkit/kinds"
	"github.com/open-rails/contentkit/popularity"
	"github.com/open-rails/contentkit/signal"
)

// PopularitySnapshot describes the prepared ranking actually served. A pending
// bucket has Ready=false; a successfully prepared empty ranking has Ready=true.
type PopularitySnapshot struct {
	Bucket string `json:"bucket"`
	Ready  bool   `json:"ready"`
	// WindowFrom/WindowUntil are the resolved bucket requested at preparation.
	WindowFrom  *time.Time `json:"window_from,omitempty"`
	WindowUntil *time.Time `json:"window_until,omitempty"`
	// From/Until are the covered whole UTC days actually ranked.
	From          *time.Time `json:"from,omitempty"`
	Until         *time.Time `json:"until,omitempty"`
	AvailableFrom *time.Time `json:"available_from,omitempty"`
	ComputedAt    *time.Time `json:"computed_at,omitempty"`
	// Provisional marks recent days still accepting measurements. Absence does
	// not certify that every older day has completed its final preparation.
	Provisional bool `json:"provisional,omitempty"`
}

type popularRanking struct{ kind, period, domain, language string }

func (s *Service) rankingFor(ctx context.Context, reader Reader, kind, id string) (popularRanking, bool) {
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

func (s *Service) rankingSnapshot(ctx context.Context, key popularRanking) (*PopularitySnapshot, error) {
	snapshot := &PopularitySnapshot{Bucket: key.period}
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

// RefreshPopularRankings prepares the host's declared choices, including on an
// empty installation with no public traffic. A reader never creates work.
func (s *Service) RefreshPopularRankings(ctx context.Context) error {
	if err := s.signals(); err != nil {
		return err
	}
	now := s.now()
	type scopeKey struct {
		kind  string
		scope kinds.PopularityScope
	}
	type pending struct {
		key    popularRanking
		window signal.Window
	}
	groups := map[scopeKey][]pending{}
	for kind, config := range s.o.Kinds.Kinds {
		for _, bucket := range config.Popularity {
			from, until := bucket.Window(now)
			window := signal.Between(from, until)
			for _, scope := range bucket.ActivityScopes() {
				group := scopeKey{kind, scope}
				key := popularRanking{kind, bucket.ID, scope.Domain, scope.Language}
				snapshot, err := s.rankingSnapshot(ctx, key)
				if err != nil {
					return err
				}
				if snapshot.ComputedAt != nil && snapshot.ComputedAt.Add(bucket.Every()).After(now) && sameBound(snapshot.WindowFrom, from) && sameBound(snapshot.WindowUntil, until) {
					continue
				}
				groups[group] = append(groups[group], pending{key, window})
			}
		}
	}
	var failures []error
	for group, due := range groups {
		scope := signal.ActivityScope{Domain: group.scope.Domain, Language: group.scope.Language}
		coverage, err := popularity.Prepare(ctx, s.o.Hub, s, group.kind, now, scope)
		if err != nil {
			failures = append(failures, fmt.Errorf("discovery: prepare popularity %s: %w", group.kind, err))
			continue
		}
		for _, p := range due {
			if err := s.buildRanking(ctx, p.key, p.window, coverage, now); err != nil {
				failures = append(failures, err)
			}
		}
	}
	return errors.Join(failures...)
}

func sameBound(stored *time.Time, actual time.Time) bool {
	return stored == nil && actual.IsZero() || stored != nil && stored.Equal(actual)
}

func optionalBound(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	return &value
}

func (s *Service) buildRanking(ctx context.Context, key popularRanking, window, coverage signal.Window, computed time.Time) error {
	ids, err := popularity.Rank(ctx, s.o.Hub, s, key.kind, window, signal.ActivityScope{Domain: key.domain, Language: key.language}, PopularityDepth)
	if err != nil {
		return fmt.Errorf("discovery: rank %s %s: %w", key.kind, key.period, err)
	}
	if ids == nil {
		ids = []string{}
	}
	covered := coverage
	if window.From.After(covered.From) {
		covered.From = window.From
	}
	if !window.To.IsZero() && (covered.To.IsZero() || window.To.Before(covered.To)) {
		covered.To = window.To
	}
	if coverage.From.IsZero() || !covered.From.Before(covered.To) {
		covered = signal.Window{}
	}
	_, err = s.o.Pool.Exec(ctx, `INSERT INTO `+s.t.rankings+` (tenant_id,kind,period,domain,language,window_from,window_until,computed_at,content_ids,covered_from,covered_until,available_from)
 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
 ON CONFLICT (tenant_id,kind,period,domain,language) DO UPDATE SET window_from=excluded.window_from,window_until=excluded.window_until,computed_at=excluded.computed_at,content_ids=excluded.content_ids,covered_from=excluded.covered_from,covered_until=excluded.covered_until,available_from=excluded.available_from`,
		s.o.Tenant, key.kind, key.period, key.domain, key.language, optionalBound(window.From), optionalBound(window.To), computed, ids, optionalBound(covered.From), optionalBound(covered.To), optionalBound(coverage.From))
	return err
}
