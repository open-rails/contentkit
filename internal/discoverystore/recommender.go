package discoverystore

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/open-rails/contentkit/contentref"
	discovery "github.com/open-rails/contentkit/discovery"
	signalstore "github.com/open-rails/contentkit/internal/signalstore"
	"github.com/open-rails/contentkit/signal"
)

// Recommender turns co-engagement candidates into tenant-scoped discovery lists.
type Recommender struct {
	Candidates Engagement
	Store      *signalstore.Store
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
func (r Recommender) Similar(ctx context.Context, anchor contentref.ContentRef, opts discovery.SimilarOptions) ([]discovery.Hit, error) {
	if err := anchor.Validate(); err != nil {
		return nil, err
	}
	if anchor.TenantID != r.Tenant {
		return nil, fmt.Errorf("discovery: %s belongs to tenant %q, not %q", anchor, anchor.TenantID, r.Tenant)
	}
	limit := r.limit(opts.Limit)
	kinds := trimAll(opts.ContentKinds)
	q := query{ContentKinds: kinds, Limit: oversample(limit, opts.Admit), Window: opts.Window}
	cands, err := r.Candidates.Similar(ctx, anchor, q)
	if err != nil {
		return nil, err
	}
	f := r.newFilter(limit, kinds, opts.Admit)
	f.exclude[anchor.Content().Key()] = struct{}{}
	if opts.ExcludeSeenFor != nil {
		present := make([]string, 0, len(cands))
		for _, c := range cands {
			present = append(present, c.Ref.ContentKind)
		}
		if err := f.loadSubject(ctx, r, *opts.ExcludeSeenFor, present, kinds, false); err != nil {
			return nil, err
		}
	}
	if err := f.pushAll(ctx, cands); err != nil {
		return nil, err
	}
	return f.out, nil
}

// Recommend returns "for you" works for a subject: excluding already-seen
// (unless IncludeSeen) and always disliked works, filled on cold start or
// thin candidates from popularity of the last 30 days, then of all time, then
// Backfill.
func (r Recommender) Recommend(ctx context.Context, subject signal.Subject, opts discovery.RecommendOptions) ([]discovery.Hit, error) {
	if err := subject.Validate(); err != nil {
		return nil, err
	}
	kinds := trimAll(opts.ContentKinds)
	if len(kinds) == 0 {
		return nil, fmt.Errorf("discovery: RecommendOptions.ContentKinds is required")
	}
	limit := r.limit(opts.Limit)
	q := query{
		ContentKinds:     kinds,
		Limit:            oversample(limit, opts.Admit),
		SeedLimit:        opts.SeedLimit,
		SeedContentKinds: trimAll(opts.SeedContentKinds),
	}
	cands, err := r.Candidates.ForSubject(ctx, subject, q)
	if err != nil {
		return nil, err
	}
	f := r.newFilter(limit, kinds, opts.Admit)
	if err := f.loadSubject(ctx, r, subject, kinds, kinds, opts.IncludeSeen); err != nil {
		return nil, err
	}
	if err := f.pushAll(ctx, cands); err != nil {
		return nil, err
	}

	for _, kind := range kinds {
		if opts.Popular != nil {
			if err := r.fillPopular(ctx, f, opts.Popular, kind, limit); err != nil {
				return nil, err
			}
		}
		if f.full() || opts.Backfill == nil {
			continue
		}
		refs, err := opts.Backfill(ctx, kind, f.excluded(kind), limit-len(f.out))
		if err != nil {
			return nil, err
		}
		fill := make([]candidate, len(refs))
		for i, ref := range refs {
			fill[i] = candidate{Ref: ref, Score: float64(r.fillScore(f, i))}
		}
		if err := f.pushAll(ctx, fill); err != nil {
			return nil, err
		}
	}
	return f.out, nil
}

// fillPopular fills f from kind's default prepared bucket.
func (r Recommender) fillPopular(ctx context.Context, f *filter, popular func(context.Context, string, int) ([]string, error), kind string, limit int) error {
	// Oversample so exclusions still leave enough to fill, deeper while a
	// full read leaves the list short.
	for depth := clampInt((limit-len(f.out))*3, 20, 500); !f.full(); depth *= 4 {
		pop, err := popular(ctx, kind, depth)
		if err != nil {
			return err
		}
		fill := make([]candidate, len(pop))
		for i, id := range pop {
			fill[i] = candidate{Ref: contentref.New(r.Tenant, kind, id), Score: float64(r.fillScore(f, i))}
		}
		if err := f.pushAll(ctx, fill); err != nil {
			return err
		}
		if len(pop) < depth || depth >= maxFillDepth {
			return nil
		}
	}
	return nil
}

// fillScore is the i-th fill's score: fills live on another scale than
// candidates, so they follow the list's tail with decaying scores.
func (r Recommender) fillScore(f *filter, i int) float32 {
	if n := len(f.out); n > 0 {
		return f.out[n-1].Score / 2
	}
	return 1 / float32(r.rrfK()+i+1)
}

// maxFillDepth bounds the popularity read a filtered fill goes to.
const maxFillDepth = 2000

// oversample is the candidate count asked of a source: admission drops some.
func oversample(limit int, admit discovery.Admit) int {
	if admit == nil {
		return limit
	}
	return clampInt(limit*3, limit, 300)
}

// filter applies the shared policy: tenant, kinds, exclusions, admission,
// dedupe, limit.
type filter struct {
	tenant  string
	limit   int
	kinds   map[string]struct{} // nil = any
	exclude map[contentref.ContentKey]struct{}
	seen    map[string]map[string]struct{}
	admit   discovery.Admit
	out     []discovery.Hit
}

func (r Recommender) newFilter(limit int, kinds []string, admit discovery.Admit) *filter {
	f := &filter{tenant: r.Tenant, limit: limit, exclude: map[contentref.ContentKey]struct{}{}, seen: map[string]map[string]struct{}{},
		admit: admit, out: make([]discovery.Hit, 0, limit)}
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

// excluded are kind's content ids the list may no longer show: seen,
// disliked or already in it.
func (f *filter) excluded(kind string) []string {
	out := make([]string, 0, len(f.seen[kind])+len(f.exclude))
	for id := range f.seen[kind] {
		out = append(out, id)
	}
	for key := range f.exclude {
		if key.ContentKind == kind {
			out = append(out, key.ContentID)
		}
	}
	return out
}

// pushAll appends candidates in order until full: those the policy keeps,
// then admitted in one batch.
func (f *filter) pushAll(ctx context.Context, cands []candidate) error {
	if f.full() {
		return nil
	}
	kept := make([]discovery.Hit, 0, len(cands))
	batch := map[contentref.ContentKey]bool{}
	for _, c := range cands {
		if ref, ok := f.keeps(c.Ref); ok && !batch[ref.Key()] {
			batch[ref.Key()] = true
			kept = append(kept, discovery.Hit{ContentRef: ref, Score: float32(c.Score)})
		}
	}
	if f.admit != nil && len(kept) > 0 {
		refs := make([]contentref.ContentRef, len(kept))
		for i, h := range kept {
			refs[i] = h.ContentRef
		}
		admitted, err := f.admit(ctx, refs)
		if err != nil {
			return err
		}
		kept = slices.DeleteFunc(kept, func(h discovery.Hit) bool { return !admitted[h.Key()] })
	}
	for _, h := range kept {
		if f.full() {
			break
		}
		f.exclude[h.Key()] = struct{}{}
		f.out = append(f.out, h)
	}
	return nil
}

// keeps is the policy on one candidate, as its work.
func (f *filter) keeps(ref contentref.ContentRef) (contentref.ContentRef, bool) {
	if ref.TenantID != f.tenant || ref.Validate() != nil {
		return ref, false
	}
	ref = ref.Content()
	if f.kinds != nil {
		if _, ok := f.kinds[ref.ContentKind]; !ok {
			return ref, false
		}
	}
	if _, ok := f.exclude[ref.Key()]; ok {
		return ref, false
	}
	_, seen := f.seen[ref.ContentKind][ref.ContentID]
	return ref, !seen
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
