package popularity

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/signal"
)

// Source reads one tenant's canonical window projections; the engine's hub
// satisfies it. Every reference the Ranker builds carries Source.Tenant().
type Source interface {
	Tenant() string
	Popular(ctx context.Context, contentKind string, opts signal.PopularOptions) ([]signal.PopularHit, error)
	Metrics(ctx context.Context, refs []signal.ContentRef, window signal.Window) (map[signal.ContentKey]signal.ContentMetrics, error)
}

// Catalog is the join entity popularity derives from: which entities (artist,
// series, tag, character, season, ...) content is in. ContentKit never
// records a signal against an entity; an entity ranks by its member contents.
// Memberships returns, per content id, the ids of the entities of groupKind
// effective on the content (it or any version); contents in none are
// absent. *entities.Store implements it.
type Catalog interface {
	Memberships(ctx context.Context, tenant, contentKind, entityKind string, contentIDs []string) (map[string][]contentref.EntityID, error)
}

// CatalogFunc adapts a function to Catalog.
type CatalogFunc func(ctx context.Context, tenant, contentKind, entityKind string, contentIDs []string) (map[string][]contentref.EntityID, error)

func (f CatalogFunc) Memberships(ctx context.Context, tenant, contentKind, entityKind string, contentIDs []string) (map[string][]contentref.EntityID, error) {
	return f(ctx, tenant, contentKind, entityKind, contentIDs)
}

// DefaultEntityCandidateLimit bounds the ranked works an entity listing
// aggregates; see docs/popularity-policy.md for what the bound means.
const DefaultEntityCandidateLimit = 2000

// DefaultMaxDepth is the deepest rank Popular serves by default; it equals
// DefaultEntityCandidateLimit, the deepest prefix Entities reads.
const DefaultMaxDepth = DefaultEntityCandidateLimit

// minReadDepth is the shallowest ranking prefix Popular reads.
const minReadDepth = 64

// DefaultCacheTTL bounds how stale a cached global ranking may be: the rollup
// behind it is daily, so minutes change nothing a reader would notice.
const DefaultCacheTTL = 5 * time.Minute

// Config builds a Ranker. Hosts pass a policy name resolved through ByName or
// a Policy of their own; either is validated.
type Config struct {
	Source Source
	Policy Policy
	// Catalog enables Entities. Optional.
	Catalog Catalog
	// Cache memoizes Popular and Entities under policy-named keys. Optional.
	Cache    Cache
	CacheTTL time.Duration // default DefaultCacheTTL
	// EntityCandidateLimit is the ranked-work bound of Entities (default
	// DefaultEntityCandidateLimit). Part of the cache key.
	EntityCandidateLimit int
	// MaxDepth is the deepest rank Popular serves (default DefaultMaxDepth,
	// or EntityCandidateLimit when larger); a larger limit is clamped, so a
	// listing ends there. It bounds the rows one read returns and one cache
	// entry holds. Must be at least EntityCandidateLimit.
	MaxDepth     int
	Availability AvailabilityProvider
}

// Ranker applies one policy to one tenant's signal plane.
type Ranker struct {
	source              Source
	policy              Policy
	catalog             Catalog
	cache               Cache
	cacheTTL            time.Duration
	entityCandidates    int
	maxDepth            int
	availability        AvailabilityProvider
	scope               signal.ActivityScope
	normalization       *signal.NormalizationOptions
	standingUnavailable bool
}

// New validates the policy and returns the Ranker.
func New(cfg Config) (*Ranker, error) {
	if cfg.Source == nil {
		return nil, fmt.Errorf("popularity: Source is required")
	}
	if err := cfg.Policy.Validate(); err != nil {
		return nil, err
	}
	r := &Ranker{source: cfg.Source, policy: cfg.Policy, catalog: cfg.Catalog, cache: cfg.Cache, cacheTTL: cfg.CacheTTL,
		entityCandidates: cfg.EntityCandidateLimit, maxDepth: cfg.MaxDepth, availability: cfg.Availability}
	if r.cacheTTL <= 0 {
		r.cacheTTL = DefaultCacheTTL
	}
	if r.entityCandidates <= 0 {
		r.entityCandidates = DefaultEntityCandidateLimit
	}
	if r.maxDepth <= 0 {
		r.maxDepth = max(DefaultMaxDepth, r.entityCandidates)
	}
	if r.entityCandidates > r.maxDepth {
		return nil, fmt.Errorf("popularity: GroupCandidateLimit %d exceeds MaxDepth %d", r.entityCandidates, r.maxDepth)
	}
	return r, nil
}

// Policy returns the ranking this Ranker applies.
func (r *Ranker) Policy() Policy { return r.policy }

// MaxDepth is the deepest rank Popular serves.
func (r *Ranker) MaxDepth() int { return r.maxDepth }

// Hit is one ranked work: the policy score next to its raw window metrics.
// Public counts come from the metrics, never from the score.
type Hit struct {
	ContentID string
	Score     float64
	signal.ContentMetrics
}

// MeanEngagement is mean bounded measured quality per distinct qualified reader.
func (h Hit) MeanEngagement() float64 {
	if h.Readers == 0 {
		return 0
	}
	return h.ReaderQualitySum / float64(h.Readers)
}

// EntityHit is one ranked entity, derived from its member works.
type EntityHit struct {
	EntityID     contentref.EntityID
	EntityKind   string
	Score        float64 // summed member scores
	ContentCount uint64  // ranked members inside the candidate bound
	Readers      uint64  // summed member readers (a person engaging with two members counts twice)
	MeanScore    float64 // Score / ContentCount
}

// Popular returns the top limit works of one kind in a window, ranked by the
// policy inside ClickHouse (RankExpr). Only works with qualified readers rank;
// ties break on content id. limit is clamped to MaxDepth. The ranking is
// global and read in power-of-two prefixes, so a few cache entries per window
// serve every reader; hosts page by asking for offset+limit and slicing.
func (r *Ranker) Popular(ctx context.Context, contentKind string, window signal.Window, limit int) ([]Hit, error) {
	result, err := r.PopularResult(ctx, contentKind, window, limit)
	return result.Hits, err
}

func (r *Ranker) popular(ctx context.Context, contentKind string, window signal.Window, limit int) ([]Hit, error) {
	if r.standingUnavailable {
		return []Hit{}, nil
	}
	if strings.TrimSpace(contentKind) == "" {
		return nil, fmt.Errorf("popularity: contentKind is required")
	}
	if err := window.Validate(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 20
	}
	limit = min(limit, r.maxDepth)
	depth := r.readDepth(limit)
	key := r.cacheKey("popular", contentKind, window.String(), strconv.Itoa(depth))
	var out []Hit
	if !r.cached(ctx, key, &out) {
		hits, err := r.source.Popular(ctx, contentKind, signal.PopularOptions{Window: window, Limit: depth, RankExpr: r.policy.RankExpr(), Normalization: r.normalization})
		if err != nil {
			return nil, err
		}
		out = make([]Hit, 0, len(hits))
		for _, h := range hits {
			out = append(out, Hit{ContentID: h.ContentID, Score: h.Score, ContentMetrics: h.ContentMetrics})
		}
		r.store(ctx, key, out)
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// readDepth is the ranking prefix read for limit: the next power of two from
// minReadDepth, at most MaxDepth.
func (r *Ranker) readDepth(limit int) int {
	depth := minReadDepth
	for depth < limit {
		depth *= 2
	}
	return min(depth, r.maxDepth)
}

// Candidates scores a host-selected set of works (an artist's galleries, a
// search page, a tag) in Go over one Metrics read: score descending, content
// id ascending. Candidates without qualified readers in the window are absent,
// exactly as in Popular.
func (r *Ranker) Candidates(ctx context.Context, contentKind string, ids []string, window signal.Window) ([]Hit, error) {
	if strings.TrimSpace(contentKind) == "" {
		return nil, fmt.Errorf("popularity: contentKind is required")
	}
	if err := window.Validate(); err != nil {
		return nil, err
	}
	if r.policy.Normalized() {
		q, window, _, err := r.query(ctx, contentKind, window)
		if err != nil {
			return nil, err
		}
		if q.standingUnavailable {
			return []Hit{}, nil
		}
		if !q.policy.Normalized() {
			return q.Candidates(ctx, contentKind, ids, window)
		}
		wanted := make(map[string]bool, len(ids))
		for _, id := range ids {
			wanted[id] = true
		}
		n := *q.normalization
		n.Availability.Intervals = nil
		for _, interval := range q.normalization.Availability.Intervals {
			if wanted[interval.ContentID] {
				n.Availability.Intervals = append(n.Availability.Intervals, interval)
			}
		}
		hits, err := q.source.Popular(ctx, contentKind, signal.PopularOptions{Window: window, Limit: len(ids), RankExpr: q.policy.RankExpr(), Normalization: &n})
		out := make([]Hit, 0, len(hits))
		for _, h := range hits {
			out = append(out, Hit{ContentID: h.ContentID, Score: h.Score, ContentMetrics: h.ContentMetrics})
		}
		return out, err
	}
	refs := make([]signal.ContentRef, 0, len(ids))
	for _, id := range ids {
		if id = strings.TrimSpace(id); id != "" {
			refs = append(refs, contentref.New(r.source.Tenant(), contentKind, id))
		}
	}
	if len(refs) == 0 {
		return []Hit{}, nil
	}
	metrics, err := r.source.Metrics(ctx, refs, window)
	if err != nil {
		return nil, err
	}
	out := make([]Hit, 0, len(metrics))
	for key, m := range metrics {
		if key.ContentVersionID != "" || m.Readers == 0 {
			continue
		}
		out = append(out, Hit{ContentID: key.ContentID, Score: r.policy.Score(m), ContentMetrics: m})
	}
	sortHits(out)
	return out, nil
}

// Scores is Candidates as content id → score.
func (r *Ranker) Scores(ctx context.Context, contentKind string, ids []string, window signal.Window) (map[string]float64, error) {
	hits, err := r.Candidates(ctx, contentKind, ids, window)
	if err != nil {
		return nil, err
	}
	out := make(map[string]float64, len(hits))
	for _, h := range hits {
		out[h.ContentID] = h.Score
	}
	return out, nil
}

// Entities ranks the entities of one kind by the window popularity
// of their member works, joined through the Catalog over the top
// EntityCandidateLimit ranked works: score = summed member scores. The
// listing is approximate: a record with no member in that slice is absent and
// counts describe only the slice.
func (r *Ranker) Entities(ctx context.Context, contentKind, entityKind string, window signal.Window) ([]EntityHit, error) {
	if r.catalog == nil {
		return nil, fmt.Errorf("popularity: group ranking needs a Catalog")
	}
	if strings.TrimSpace(entityKind) == "" {
		return nil, fmt.Errorf("popularity: groupKind is required")
	}
	key := r.cacheKey("groups", contentKind, entityKind, window.String(), strconv.Itoa(r.entityCandidates))
	var out []EntityHit
	if r.cached(ctx, key, &out) {
		return out, nil
	}
	works, err := r.Popular(ctx, contentKind, window, r.entityCandidates)
	if err != nil {
		return nil, err
	}
	out = []EntityHit{}
	if len(works) > 0 {
		ids := make([]string, 0, len(works))
		for _, w := range works {
			ids = append(ids, w.ContentID)
		}
		memberships, err := r.catalog.Memberships(ctx, r.source.Tenant(), contentKind, entityKind, ids)
		if err != nil {
			return nil, fmt.Errorf("popularity: %s memberships: %w", entityKind, err)
		}
		byID := map[contentref.EntityID]*EntityHit{}
		for _, w := range works {
			seen := map[contentref.EntityID]struct{}{}
			for _, tid := range memberships[w.ContentID] {
				if _, dup := seen[tid]; dup || tid == "" {
					continue
				}
				seen[tid] = struct{}{}
				e := byID[tid]
				if e == nil {
					e = &EntityHit{EntityID: tid, EntityKind: entityKind}
					byID[tid] = e
				}
				e.ContentCount++
				e.Readers += w.Readers
				e.Score += w.Score
			}
		}
		for _, e := range byID {
			e.MeanScore = e.Score / float64(e.ContentCount)
			out = append(out, *e)
		}
		sort.Slice(out, func(i, j int) bool {
			if out[i].Score != out[j].Score {
				return out[i].Score > out[j].Score
			}
			return out[i].EntityID < out[j].EntityID
		})
	}
	r.store(ctx, key, out)
	return out, nil
}

func sortHits(hits []Hit) {
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		return hits[i].ContentID < hits[j].ContentID
	})
}

// cacheKey includes the complete policy and unambiguous component boundaries.
// Tenant identifiers and custom policy names are opaque: a delimiter in one
// must never allow another tenant or policy to reuse its cached results.
func (r *Ranker) cacheKey(parts ...string) string {
	// New validates every float in Policy, so this fixed shape always encodes.
	body, _ := json.Marshal(struct {
		Tenant        string
		Policy        Policy
		Parts         []string
		Scope         signal.ActivityScope
		Normalization *signal.NormalizationOptions
	}{r.source.Tenant(), r.policy, parts, r.scope, r.normalization})
	return fmt.Sprintf("contentkit:measured-popularity:v1:%x", sha256.Sum256(body))
}

func (r *Ranker) cached(ctx context.Context, key string, into any) bool {
	if r.cache == nil {
		return false
	}
	body, ok := r.cache.Get(ctx, key)
	return ok && json.Unmarshal(body, into) == nil
}

func (r *Ranker) store(ctx context.Context, key string, value any) {
	if r.cache == nil {
		return
	}
	if body, err := json.Marshal(value); err == nil {
		r.cache.Set(ctx, key, body, r.cacheTTL)
	}
}
