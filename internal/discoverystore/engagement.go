package discoverystore

import (
	"context"
	"errors"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/search"
	signalstore "github.com/open-rails/contentkit/internal/signalstore"
	"github.com/open-rails/contentkit/signal"
	"golang.org/x/sync/errgroup"
)

// seedConcurrency bounds how many seed queries are in flight at once, so one
// recommendation never takes the whole ClickHouse pool.
const seedConcurrency = 4

var errNoStore = errors.New("discovery: Engagement.Store is required")

// query bounds one candidate request.
type query struct {
	// ContentKinds limits candidate kinds (empty = any).
	ContentKinds []string
	// Limit is the number of results the caller returns after filtering.
	Limit int
	// Window bounds the evidence (zero = all time).
	Window signal.Window
	// SeedLimit and SeedContentKinds bound which of the subject's history
	// informs ForSubject (0 / empty = default).
	SeedLimit        int
	SeedContentKinds []string
}

// candidate is one ranked work.
type candidate struct {
	Ref   contentref.ContentRef
	Score float64
}

// Engagement is the co-engagement candidate source: Similar reads works
// co-engaged with the anchor; ForSubject seeds from the subject's
// highest-signal non-negative works and RRF-fuses each seed's co-engaged list.
type Engagement struct {
	Store  *signalstore.Store
	Tenant string
	// RRFK is the seed fusion constant (default 60).
	RRFK int
}

func (e Engagement) Similar(ctx context.Context, anchor contentref.ContentRef, q query) ([]candidate, error) {
	if e.Store == nil {
		return nil, errNoStore
	}
	co, err := e.Store.CoEngaged(ctx, e.Tenant, anchor, signal.CoEngagedOptions{
		ContentKinds: q.ContentKinds,
		Window:       q.Window,
		Limit:        clampInt(q.Limit*2, q.Limit, 200),
	})
	if err != nil {
		return nil, err
	}
	out := make([]candidate, 0, len(co))
	for _, c := range co {
		out = append(out, candidate{Ref: c.ContentRef, Score: float64(c.Strength)})
	}
	return out, nil
}

// ForSubject never returns its seeds.
func (e Engagement) ForSubject(ctx context.Context, subject signal.Subject, q query) ([]candidate, error) {
	if e.Store == nil {
		return nil, errNoStore
	}
	seedLimit := q.SeedLimit
	if seedLimit <= 0 {
		seedLimit = 5
	}
	seeds, err := e.Store.TopStates(ctx, e.Tenant, subject, signal.TopStatesOptions{
		ContentKinds:    q.SeedContentKinds,
		ExcludeNegative: true,
		Limit:           seedLimit,
	})
	if err != nil {
		return nil, err
	}
	if len(seeds) == 0 {
		return nil, nil
	}
	perSeed := clampInt(q.Limit, 20, 100)
	isSeed := make(map[contentref.ContentKey]struct{}, len(seeds))
	for _, seed := range seeds {
		isSeed[seed.Key()] = struct{}{}
	}

	// Per-seed slots keep the fused order deterministic (RRF is positional).
	lists := make([][]search.RRFKey, len(seeds))
	entity, entityCtx := errgroup.WithContext(ctx)
	entity.SetLimit(seedConcurrency)
	for i, seed := range seeds {
		entity.Go(func() error {
			co, err := e.Store.CoEngaged(entityCtx, e.Tenant, seed.ContentRef, signal.CoEngagedOptions{ContentKinds: q.ContentKinds, Window: q.Window, Limit: perSeed})
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
	if err := entity.Wait(); err != nil {
		return nil, err
	}
	fused := search.FuseRRF(lists, search.RRFOptions{K: e.RRFK})
	out := make([]candidate, 0, len(fused))
	for _, f := range fused {
		ref := f.Ref()
		if _, ok := isSeed[ref.Key()]; ok {
			continue
		}
		out = append(out, candidate{Ref: ref, Score: float64(f.Score)})
	}
	return out, nil
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
