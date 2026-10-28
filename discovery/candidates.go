package discovery

import (
	"context"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/signal"
)

// Candidates generates ranked candidate works for similar and "for you"
// lists; Recommender applies the shared policy (tenant, kinds, anchor, seen
// and disliked exclusions, admission, dedupe, limit, popularity fill) so
// every source gets it. Engagement is the default co-engagement source;
// Fallback composes a new source over it. Order is rank. Implementations
// should oversample past Limit: Recommender filters afterward and truncates.
type Candidates interface {
	Similar(ctx context.Context, anchor contentref.ContentRef, q Query) ([]Candidate, error)
	ForSubject(ctx context.Context, subject signal.Subject, q Query) ([]Candidate, error)
}

// Query bounds one candidate request.
type Query struct {
	// ContentKinds limits candidate kinds (empty = any).
	ContentKinds []string
	// Limit is the number of results the caller returns after filtering.
	Limit int
	// Window bounds the evidence (zero = all time).
	Window signal.Window
	// SeedLimit and SeedContentKinds bound which of the subject's history
	// informs ForSubject (0 / empty = implementation default).
	SeedLimit        int
	SeedContentKinds []string
}

// Candidate is one ranked work.
type Candidate struct {
	Ref   contentref.ContentRef
	Score float64
}

// Fallback serves Primary and uses Secondary when Primary fails (after
// OnError) or returns fewer than Limit candidates. Recommender decides whether
// to fill after its policy filters, so excluded candidates do not count.
type Fallback struct {
	Primary, Secondary Candidates
	OnError            func(error)
}

func (f Fallback) Similar(ctx context.Context, anchor contentref.ContentRef, q Query) ([]Candidate, error) {
	return f.run(q.Limit, func(c Candidates) ([]Candidate, error) { return c.Similar(ctx, anchor, q) })
}

func (f Fallback) ForSubject(ctx context.Context, subject signal.Subject, q Query) ([]Candidate, error) {
	return f.run(q.Limit, func(c Candidates) ([]Candidate, error) { return c.ForSubject(ctx, subject, q) })
}

func (f Fallback) run(limit int, call func(Candidates) ([]Candidate, error)) ([]Candidate, error) {
	primary, err := call(f.Primary)
	if err != nil {
		reportFallback(f, err)
		return call(f.Secondary)
	}
	if len(primary) >= max(limit, 1) {
		return primary, nil
	}
	secondary, err := call(f.Secondary)
	if err != nil {
		if len(primary) == 0 {
			return nil, err
		}
		reportFallback(f, err)
		return primary, nil
	}
	have := make(map[contentref.ContentKey]struct{}, len(primary))
	for _, c := range primary {
		have[c.Ref.Content().Key()] = struct{}{}
	}
	for _, c := range secondary {
		key := c.Ref.Content().Key()
		if _, ok := have[key]; ok {
			continue
		}
		have[key] = struct{}{}
		primary = append(primary, c)
	}
	return primary, nil
}

func reportFallback(f Fallback, err error) {
	if f.OnError != nil {
		f.OnError(err)
	}
}
