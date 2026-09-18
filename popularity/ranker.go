package popularity

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/signal"
)

// Source reads one tenant's canonical window projections; contentkit.Hub
// satisfies it. Every reference the Ranker builds carries Source.Tenant().
type Source interface {
	Tenant() string
	Popular(ctx context.Context, contentKind string, opts signal.PopularOptions) ([]signal.PopularHit, error)
	Metrics(ctx context.Context, refs []signal.ContentRef, window signal.Window) (map[signal.ContentKey]signal.ContentMetrics, error)
}

// Catalog is the host join port taxonomy popularity derives from: which
// taxonomy records (artist, series, tag, creator, character, season, ...) a
// work is assigned to. ContentKit never records a signal against a taxonomy
// id; a taxonomy record ranks by its member works. Assignments returns, per
// work id, the taxonomy ids of taxonomyKind effective on the work (work ∪ its
// versions); ids without assignments are absent.
type Catalog interface {
	Assignments(ctx context.Context, tenant, contentKind, taxonomyKind string, contentIDs []string) (map[string][]contentref.TaxonomyID, error)
}

// CatalogFunc adapts a function to Catalog.
type CatalogFunc func(ctx context.Context, tenant, contentKind, taxonomyKind string, contentIDs []string) (map[string][]contentref.TaxonomyID, error)

func (f CatalogFunc) Assignments(ctx context.Context, tenant, contentKind, taxonomyKind string, contentIDs []string) (map[string][]contentref.TaxonomyID, error) {
	return f(ctx, tenant, contentKind, taxonomyKind, contentIDs)
}

// DefaultTaxonomyCandidateLimit bounds the ranked works a taxonomy listing
// aggregates; see docs/popularity-policy.md for what the bound means.
const DefaultTaxonomyCandidateLimit = 2000

// DefaultCacheTTL bounds how stale a cached global ranking may be: the rollup
// behind it is daily, so minutes change nothing a reader would notice.
const DefaultCacheTTL = 5 * time.Minute

// Config builds a Ranker. Hosts pass a policy name resolved through ByName or
// a Policy of their own; either is validated.
type Config struct {
	Source Source
	Policy Policy
	// Catalog enables Taxonomy. Optional.
	Catalog Catalog
	// Cache memoizes Popular and Taxonomy under policy-named keys. Optional.
	Cache    Cache
	CacheTTL time.Duration // default DefaultCacheTTL
	// TaxonomyCandidateLimit is the ranked-work bound of Taxonomy (default
	// DefaultTaxonomyCandidateLimit). Part of the cache key.
	TaxonomyCandidateLimit int
}

// Ranker applies one policy to one tenant's signal plane.
type Ranker struct {
	source             Source
	policy             Policy
	catalog            Catalog
	cache              Cache
	cacheTTL           time.Duration
	taxonomyCandidates int
}

// New validates the policy and returns the Ranker.
func New(cfg Config) (*Ranker, error) {
	if cfg.Source == nil {
		return nil, fmt.Errorf("popularity: Source is required")
	}
	if err := cfg.Policy.Validate(); err != nil {
		return nil, err
	}
	r := &Ranker{source: cfg.Source, policy: cfg.Policy, catalog: cfg.Catalog, cache: cfg.Cache, cacheTTL: cfg.CacheTTL, taxonomyCandidates: cfg.TaxonomyCandidateLimit}
	if r.cacheTTL <= 0 {
		r.cacheTTL = DefaultCacheTTL
	}
	if r.taxonomyCandidates <= 0 {
		r.taxonomyCandidates = DefaultTaxonomyCandidateLimit
	}
	return r, nil
}

// Policy returns the ranking this Ranker applies.
func (r *Ranker) Policy() Policy { return r.policy }

// Hit is one ranked work: the policy score next to its raw window metrics.
// Public counts come from the metrics, never from the score.
type Hit struct {
	ContentID string
	Score     float64
	signal.ContentMetrics
}

// MeanEngagement is the mean session score in [0, 1]: ScoreSum / (100·Views).
func (h Hit) MeanEngagement() float64 {
	if h.Views == 0 {
		return 0
	}
	return float64(h.ScoreSum) / (100 * float64(h.Views))
}

// TaxonomyHit is one ranked taxonomy record, derived from its member works.
type TaxonomyHit struct {
	TaxonomyID   contentref.TaxonomyID
	TaxonomyKind string
	Score        float64 // summed member scores
	ContentCount uint64  // ranked members inside the candidate bound
	Viewers      uint64  // summed member viewers (a subject viewing two members counts twice)
	MeanScore    float64 // Score / ContentCount
}

// Popular returns the top limit works of one kind in a window, ranked by the
// policy inside ClickHouse (RankExpr). Only works with a view in the window
// rank; ties break on content id. The ranking is global, so one cache entry
// serves every reader; hosts page by asking for offset+limit and slicing.
func (r *Ranker) Popular(ctx context.Context, contentKind string, window signal.Window, limit int) ([]Hit, error) {
	if strings.TrimSpace(contentKind) == "" {
		return nil, fmt.Errorf("popularity: contentKind is required")
	}
	if err := window.Validate(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 20
	}
	key := r.cacheKey("popular", contentKind, window.String(), strconv.Itoa(limit))
	var out []Hit
	if r.cached(ctx, key, &out) {
		return out, nil
	}
	hits, err := r.source.Popular(ctx, contentKind, signal.PopularOptions{Window: window, Limit: limit, RankExpr: r.policy.RankExpr()})
	if err != nil {
		return nil, err
	}
	out = make([]Hit, 0, len(hits))
	for _, h := range hits {
		out = append(out, Hit{ContentID: h.ContentID, Score: h.Score, ContentMetrics: h.ContentMetrics})
	}
	r.store(ctx, key, out)
	return out, nil
}

// Candidates scores a host-selected set of works (an artist's galleries, a
// search page, a tag) in Go over one Metrics read: score descending, content
// id ascending. Candidates without a view in the window are absent, exactly
// as in Popular.
func (r *Ranker) Candidates(ctx context.Context, contentKind string, ids []string, window signal.Window) ([]Hit, error) {
	if strings.TrimSpace(contentKind) == "" {
		return nil, fmt.Errorf("popularity: contentKind is required")
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
		if key.ContentVersionID != "" || m.Views == 0 {
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

// Taxonomy ranks the taxonomy records of one kind by the window popularity
// of their member works, joined through the Catalog over the top
// TaxonomyCandidateLimit ranked works: score = summed member scores. The
// listing is approximate: a record with no member in that slice is absent and
// counts describe only the slice.
func (r *Ranker) Taxonomy(ctx context.Context, contentKind, taxonomyKind string, window signal.Window) ([]TaxonomyHit, error) {
	if r.catalog == nil {
		return nil, fmt.Errorf("popularity: taxonomy ranking needs a Catalog")
	}
	if strings.TrimSpace(taxonomyKind) == "" {
		return nil, fmt.Errorf("popularity: taxonomyKind is required")
	}
	key := r.cacheKey("taxonomy", contentKind, taxonomyKind, window.String(), strconv.Itoa(r.taxonomyCandidates))
	var out []TaxonomyHit
	if r.cached(ctx, key, &out) {
		return out, nil
	}
	works, err := r.Popular(ctx, contentKind, window, r.taxonomyCandidates)
	if err != nil {
		return nil, err
	}
	out = []TaxonomyHit{}
	if len(works) > 0 {
		ids := make([]string, 0, len(works))
		for _, w := range works {
			ids = append(ids, w.ContentID)
		}
		assignments, err := r.catalog.Assignments(ctx, r.source.Tenant(), contentKind, taxonomyKind, ids)
		if err != nil {
			return nil, fmt.Errorf("popularity: %s assignments: %w", taxonomyKind, err)
		}
		byID := map[contentref.TaxonomyID]*TaxonomyHit{}
		for _, w := range works {
			seen := map[contentref.TaxonomyID]struct{}{}
			for _, tid := range assignments[w.ContentID] {
				if _, dup := seen[tid]; dup || tid == "" {
					continue
				}
				seen[tid] = struct{}{}
				e := byID[tid]
				if e == nil {
					e = &TaxonomyHit{TaxonomyID: tid, TaxonomyKind: taxonomyKind}
					byID[tid] = e
				}
				e.ContentCount++
				e.Viewers += w.Viewers
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
			return out[i].TaxonomyID < out[j].TaxonomyID
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

// cacheKey names the tenant and the policy so no other policy, tenant, kind,
// window or bound can read this entry.
func (r *Ranker) cacheKey(parts ...string) string {
	return "contentkit:popularity:" + r.source.Tenant() + ":" + r.policy.Name + ":" + strings.Join(parts, ":")
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
