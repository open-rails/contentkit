package discovery

import (
	"context"
	"errors"

	"golang.org/x/sync/errgroup"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/search"
	"github.com/open-rails/contentkit/signal"
)

// seedConcurrency bounds how many seed queries are in flight at once, so one
// recommendation never takes the whole ClickHouse pool.
const seedConcurrency = 4

var errNoStore = errors.New("discovery: Engagement.Store is required")

// Engagement is the co-engagement Candidates source: Similar reads works
// co-engaged with the anchor; ForSubject seeds from the subject's
// highest-signal non-negative works and RRF-fuses each seed's co-engaged list.
type Engagement struct {
	Store  *signal.Store
	Tenant string
	// RRFK is the seed fusion constant (default 60).
	RRFK int
}

var _ Candidates = Engagement{}

// NewEngagement builds the default source over a hub's ClickHouse database.
func NewEngagement(conn signal.Conn, database, tenant string) (Engagement, error) {
	store, err := signal.NewStore(conn, database)
	if err != nil {
		return Engagement{}, err
	}
	return Engagement{Store: store, Tenant: tenant}, nil
}

func (e Engagement) Similar(ctx context.Context, anchor contentref.ContentRef, q Query) ([]Candidate, error) {
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
	out := make([]Candidate, 0, len(co))
	for _, c := range co {
		out = append(out, Candidate{Ref: c.ContentRef, Score: float64(c.Strength)})
	}
	return out, nil
}

// ForSubject never returns its seeds.
func (e Engagement) ForSubject(ctx context.Context, subject signal.Subject, q Query) ([]Candidate, error) {
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
	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(seedConcurrency)
	for i, seed := range seeds {
		group.Go(func() error {
			co, err := e.Store.CoEngaged(groupCtx, e.Tenant, seed.ContentRef, signal.CoEngagedOptions{ContentKinds: q.ContentKinds, Window: q.Window, Limit: perSeed})
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
	fused := search.FuseRRF(lists, search.RRFOptions{K: e.RRFK})
	out := make([]Candidate, 0, len(fused))
	for _, f := range fused {
		ref := f.Ref()
		if _, ok := isSeed[ref.Key()]; ok {
			continue
		}
		out = append(out, Candidate{Ref: ref, Score: float64(f.Score)})
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
