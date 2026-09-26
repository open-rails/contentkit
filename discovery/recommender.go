package discovery

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/signal"
)

// Hit is one ranked work of a discovery list.
type Hit struct {
	contentref.ContentRef
	Score float32
}

// SimilarOptions controls Similar ("more like this").
type SimilarOptions struct {
	Limit int
	// ContentKinds limits result kinds (default: any).
	ContentKinds []string
	// Window bounds the candidate evidence (default all time).
	Window signal.Window

	// ExcludeSeenFor drops works this subject has already seen (and always
	// drops works they negatively reacted to).
	ExcludeSeenFor *signal.Subject
}

// RecommendOptions controls Recommend ("for you": subject → works).
type RecommendOptions struct {
	// ContentKinds are the candidate kinds to recommend. Required.
	ContentKinds []string

	// Limit caps results (default: Recommender.DefaultLimit).
	Limit int

	// SeedLimit is how many of the subject's highest-signal works inform
	// candidates (source default; Engagement: 5).
	SeedLimit int

	// SeedContentKinds limits which kinds may seed (default: any).
	SeedContentKinds []string

	// IncludeSeen keeps already-seen works in results (default: excluded).
	IncludeSeen bool

	// PopularWindow is the popularity window used to fill out results on
	// cold start or thin candidate sets (zero = all time, like every Window).
	PopularWindow signal.Window
}

// Recommender turns a Candidates source into tenant-scoped discovery lists.
type Recommender struct {
	Candidates Candidates
	Store      *signal.Store
	Tenant     string
	// DefaultLimit applies when options leave Limit zero (default 20).
	DefaultLimit int
	// RRFK scales popularity-fill tail scores (default 60).
	RRFK int
}

func (r Recommender) limit(n int) int {
	switch {
	case n > 0:
		return n
	case r.DefaultLimit > 0:
		return r.DefaultLimit
	}
	return 20
}

func (r Recommender) rrfK() int {
	if r.RRFK > 0 {
		return r.RRFK
	}
	return 60
}

// Similar returns works like the anchor, never the anchor itself.
func (r Recommender) Similar(ctx context.Context, anchor contentref.ContentRef, opts SimilarOptions) ([]Hit, error) {
	if err := anchor.Validate(); err != nil {
		return nil, err
	}
	if anchor.TenantID != r.Tenant {
		return nil, fmt.Errorf("discovery: %s belongs to tenant %q, not %q", anchor, anchor.TenantID, r.Tenant)
	}
	limit := r.limit(opts.Limit)
	kinds := trimAll(opts.ContentKinds)
	cands, err := r.Candidates.Similar(ctx, anchor, Query{ContentKinds: kinds, Limit: limit, Window: opts.Window})
	if err != nil {
		return nil, err
	}
	f := r.newFilter(limit, kinds)
	f.exclude[anchor.Content().Key()] = struct{}{}
	if opts.ExcludeSeenFor != nil {
		present := []string{}
		for _, c := range cands {
			present = append(present, c.Ref.ContentKind)
		}
		if err := f.loadSubject(ctx, r, *opts.ExcludeSeenFor, present, kinds, false); err != nil {
			return nil, err
		}
	}
	for _, c := range cands {
		f.push(c.Ref, float32(c.Score))
	}
	return f.out, nil
}

// Recommend returns "for you" works for a subject: excluding already-seen
// (unless IncludeSeen) and always disliked works, filled from popularity on
// cold start or thin candidates.
func (r Recommender) Recommend(ctx context.Context, subject signal.Subject, opts RecommendOptions) ([]Hit, error) {
	if err := subject.Validate(); err != nil {
		return nil, err
	}
	kinds := trimAll(opts.ContentKinds)
	if len(kinds) == 0 {
		return nil, fmt.Errorf("discovery: RecommendOptions.ContentKinds is required")
	}
	limit := r.limit(opts.Limit)
	cands, err := r.Candidates.ForSubject(ctx, subject, Query{
		ContentKinds:     kinds,
		Limit:            limit,
		SeedLimit:        opts.SeedLimit,
		SeedContentKinds: trimAll(opts.SeedContentKinds),
	})
	if err != nil {
		return nil, err
	}
	f := r.newFilter(limit, kinds)
	if err := f.loadSubject(ctx, r, subject, kinds, kinds, opts.IncludeSeen); err != nil {
		return nil, err
	}
	for _, c := range cands {
		f.push(c.Ref, float32(c.Score))
	}

	// Popular scores live on another scale, so fill hits follow the
	// candidates with decaying tail scores.
	for _, kind := range kinds {
		if f.full() {
			break
		}
		// Oversample so exclusions still leave enough to fill.
		pop, err := r.Store.Popular(ctx, r.Tenant, kind, signal.PopularOptions{Window: opts.PopularWindow, Limit: clampInt((limit-len(f.out))*3, 20, 500)})
		if err != nil {
			return nil, err
		}
		var tail float32
		if n := len(f.out); n > 0 {
			tail = f.out[n-1].Score
		}
		for i, p := range pop {
			if f.full() {
				break
			}
			score := tail / 2
			if tail == 0 {
				score = 1 / float32(r.rrfK()+i+1)
			}
			f.push(p.ContentRef, score)
		}
	}
	return f.out, nil
}

// filter applies the shared policy: tenant, kinds, exclusions, dedupe, limit.
type filter struct {
	tenant  string
	limit   int
	kinds   map[string]struct{} // nil = any
	exclude map[contentref.ContentKey]struct{}
	seen    map[string]map[string]struct{}
	out     []Hit
}

func (r Recommender) newFilter(limit int, kinds []string) *filter {
	f := &filter{tenant: r.Tenant, limit: limit, exclude: map[contentref.ContentKey]struct{}{}, seen: map[string]map[string]struct{}{}, out: make([]Hit, 0, limit)}
	if len(kinds) > 0 {
		f.kinds = make(map[string]struct{}, len(kinds))
		for _, k := range kinds {
			f.kinds[k] = struct{}{}
		}
	}
	return f
}

// loadSubject excludes the subject's disliked works of negativeKinds and,
// unless includeSeen, their seen works of seenKinds.
func (f *filter) loadSubject(ctx context.Context, r Recommender, subject signal.Subject, seenKinds, negativeKinds []string, includeSeen bool) error {
	if !includeSeen {
		if err := f.loadSeen(ctx, r, subject, seenKinds); err != nil {
			return err
		}
	}
	negative, err := r.Store.NegativeIDs(ctx, r.Tenant, subject, negativeKinds)
	if err != nil {
		return err
	}
	for key := range negative {
		f.exclude[key] = struct{}{}
	}
	return nil
}

func (f *filter) loadSeen(ctx context.Context, r Recommender, subject signal.Subject, kinds []string) error {
	for _, kind := range kinds {
		if _, ok := f.seen[kind]; ok {
			continue
		}
		seen, err := r.Store.SeenIDs(ctx, r.Tenant, subject, kind)
		if err != nil {
			return err
		}
		f.seen[kind] = seen
	}
	return nil
}

func (f *filter) full() bool { return len(f.out) >= f.limit }

func (f *filter) push(ref contentref.ContentRef, score float32) {
	if f.full() || ref.TenantID != f.tenant || ref.Validate() != nil {
		return
	}
	ref = ref.Content()
	if f.kinds != nil {
		if _, ok := f.kinds[ref.ContentKind]; !ok {
			return
		}
	}
	key := ref.Key()
	if _, ok := f.exclude[key]; ok {
		return
	}
	if _, ok := f.seen[ref.ContentKind][ref.ContentID]; ok {
		return
	}
	f.exclude[key] = struct{}{}
	f.out = append(f.out, Hit{ContentRef: ref, Score: score})
}

// trimAll trims, drops empties and deduplicates.
func trimAll(values []string) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		if v = strings.TrimSpace(v); v != "" && !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	return out
}
