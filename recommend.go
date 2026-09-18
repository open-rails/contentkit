package contentkit

import (
	"context"
	"fmt"

	"golang.org/x/sync/errgroup"

	"github.com/open-rails/contentkit/search"
	"github.com/open-rails/contentkit/signal"
)

// recommendSeedConcurrency bounds how many seed queries are in flight at once.
// One recommendation must not take the whole ClickHouse pool: the seeds are
// few, and the win is already in not waiting for each in turn.
const recommendSeedConcurrency = 4

// RecommendOptions controls Recommend ("for you": subject → works).
type RecommendOptions struct {
	// ContentKinds are the candidate kinds to recommend. Required.
	ContentKinds []string

	// Limit caps results (default: client default limit).
	Limit int

	// SeedLimit is how many of the subject's highest-signal works seed
	// co-engagement candidates (default 5).
	SeedLimit int

	// SeedContentKinds limits which kinds may seed (default: any).
	SeedContentKinds []string

	// IncludeSeen keeps already-seen works in results (default: excluded).
	IncludeSeen bool

	// PopularWindow is the popularity window used to fill out results on
	// cold start or thin candidate sets (zero = all time, like every Window).
	PopularWindow signal.Window
}

// Recommend returns "for you" recommendations for a subject: co-engagement
// seeded from the subject's high-signal works ("subjects who engaged with your
// favorites also engaged with..."), fused across seeds (RRF), excluding
// already-seen, with a popularity fallback for cold start. Returns ranked
// references; the host hydrates.
func (h *EmbeddedHub) Recommend(ctx context.Context, subject signal.Subject, opts RecommendOptions) ([]RecHit, error) {
	store, err := h.requireStore()
	if err != nil {
		return nil, err
	}
	if err := subject.Validate(); err != nil {
		return nil, err
	}
	kinds := cloneAndTrim(opts.ContentKinds)
	if len(kinds) == 0 {
		return nil, fmt.Errorf("contentkit: RecommendOptions.ContentKinds is required")
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = h.client.defaultLimit
	}
	seedLimit := opts.SeedLimit
	if seedLimit <= 0 {
		seedLimit = 5
	}

	seeds, err := store.TopStates(ctx, h.tenant, subject, signal.TopStatesOptions{
		ContentKinds:    opts.SeedContentKinds,
		ExcludeNegative: true, // disliked works must not seed recommendations
		Limit:           seedLimit,
	})
	if err != nil {
		return nil, err
	}
	perSeed := clampInt(limit, 20, 100)
	seedSet := map[ContentKey]struct{}{}
	for _, seed := range seeds {
		seedSet[seed.Key()] = struct{}{}
	}

	// Each seed contributes one list; they run together and land in per-seed
	// slots so the fused order stays deterministic (RRF weights are positional).
	lists := make([][]search.RRFKey, len(seeds))
	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(recommendSeedConcurrency)
	for i, seed := range seeds {
		group.Go(func() error {
			co, err := store.CoEngaged(groupCtx, h.tenant, seed.ContentRef, signal.CoEngagedOptions{ContentKinds: kinds, Limit: perSeed})
			if err != nil {
				return err
			}
			keys := make([]search.RRFKey, 0, len(co))
			for _, c := range co {
				keys = append(keys, search.RRFKey{ContentKey: c.Key()})
			}
			lists[i] = keys
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return nil, err
	}
	fused := []search.RRFHit{}
	if len(lists) > 0 {
		fused = search.FuseRRF(lists, search.RRFOptions{K: h.defaultRRFK})
	}

	// Exclusions: seeds, (unless IncludeSeen) everything already seen, and
	// ALWAYS everything the subject has net-negative explicit feedback for.
	seen := map[string]map[string]struct{}{}
	if !opts.IncludeSeen {
		for _, kind := range kinds {
			s, err := store.SeenIDs(ctx, h.tenant, subject, kind)
			if err != nil {
				return nil, err
			}
			seen[kind] = s
		}
	}
	negative, err := store.NegativeIDs(ctx, h.tenant, subject, kinds)
	if err != nil {
		return nil, err
	}
	allowed := map[string]struct{}{}
	for _, kind := range kinds {
		allowed[kind] = struct{}{}
	}

	out := make([]RecHit, 0, limit)
	have := map[ContentKey]struct{}{}
	push := func(ref ContentRef, score float32) {
		key := ref.Key()
		if _, ok := allowed[ref.ContentKind]; !ok {
			return
		}
		if _, ok := seedSet[key]; ok {
			return
		}
		if _, ok := negative[key]; ok {
			return
		}
		if _, ok := have[key]; ok {
			return
		}
		if s, ok := seen[ref.ContentKind]; ok {
			if _, isSeen := s[ref.ContentID]; isSeen {
				return
			}
		}
		have[key] = struct{}{}
		out = append(out, RecHit{ContentRef: ref, Score: score})
	}
	for _, f := range fused {
		if len(out) >= limit {
			break
		}
		push(f.Ref(), f.Score)
	}

	// Cold start / thin results: fill from popularity. Popular scores live
	// on a different scale than RRF, so filled hits are appended after the
	// fused block with decaying tail scores.
	if len(out) < limit {
		for _, kind := range kinds {
			if len(out) >= limit {
				break
			}
			// Oversample so exclusions still leave enough to fill.
			popLimit := clampInt((limit-len(out))*3, 20, 500)
			pop, err := store.Popular(ctx, h.tenant, kind, signal.PopularOptions{Window: opts.PopularWindow, Limit: popLimit})
			if err != nil {
				return nil, err
			}
			var tail float32
			if n := len(out); n > 0 {
				tail = out[n-1].Score
			}
			for i, ph := range pop {
				if len(out) >= limit {
					break
				}
				score := tail / 2
				if tail == 0 {
					score = 1 / float32(h.defaultRRFK+i+1)
				}
				push(ph.ContentRef, score)
			}
		}
	}
	return out, nil
}
