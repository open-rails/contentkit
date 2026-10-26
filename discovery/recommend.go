package discovery

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/resolve"
	"github.com/open-rails/contentkit/signal"
)

// RecommendationOptions select a similar or "for you" list.
type RecommendationOptions struct {
	Reader
	Filter
	CardEntities []string
	// Limit defaults to 12 and is capped at 50.
	Limit int
	// Summary counts omissions within the bounded recommendation candidates.
	Summary bool
}

// admission is Admit over the reader's visibility and filter, keeping the
// version each admitted content shows.
type admission struct {
	s          *Service
	q          *sel
	picked     map[string]pick
	candidates map[string]struct{}
}

func (a *admission) admit(ctx context.Context, refs []contentref.ContentRef) (map[contentref.ContentKey]bool, error) {
	ids := make([]string, len(refs))
	for i, r := range refs {
		ids[i] = r.ContentID
		if a.candidates != nil {
			a.candidates[r.ContentID] = struct{}{}
		}
	}
	picks, err := a.s.pickAmong(ctx, a.q, ids)
	if err != nil {
		return nil, err
	}
	kept := map[string]bool{}
	for _, p := range picks {
		a.picked[p.content], kept[p.content] = p, true
	}
	out := make(map[contentref.ContentKey]bool, len(refs))
	for _, r := range refs {
		out[r.Key()] = kept[r.ContentID]
	}
	return out, nil
}

func (a *admission) cards(ctx context.Context, hits []Hit, o RecommendationOptions) (ContentPage, error) {
	picks := make([]pick, 0, len(hits))
	for _, h := range hits {
		if p, ok := a.picked[h.ContentID]; ok {
			picks = append(picks, p)
		}
	}
	cards, err := a.s.cards(ctx, a.q, picks, cardOptions{chips: o.CardEntities})
	out := ContentPage{Contents: cards}
	if err == nil && o.Summary {
		ids := make([]string, 0, len(a.candidates))
		for id := range a.candidates {
			ids = append(ids, id)
		}
		sort, sortErr := a.s.parseSort(ctx, SortNewest, Filter{})
		if sortErr != nil {
			return out, sortErr
		}
		out.Summary, err = a.s.listingSummary(ctx, o.Filter, a.q, sort, &summaryCandidates{visible: ids, relaxed: ids, truncated: true})
	}
	return out, err
}

func recLimit(n int) int {
	if n <= 0 {
		return 12
	}
	return min(n, 50)
}

// Similar lists contents like the anchor content that the reader's listing
// shows; of the anchor's kind unless the filter names one.
func (s *Service) Similar(ctx context.Context, anchorID string, o RecommendationOptions) (ContentPage, error) {
	ctx = access.WithMemo(ctx)
	empty := ContentPage{Contents: []Card{}}
	recs, err := s.recommender()
	if err != nil {
		return empty, err
	}
	if err := contentref.ValidateID(anchorID); err != nil {
		return empty, fmt.Errorf("%w: content id: %w", ErrInvalid, err)
	}
	var kind string
	err = s.o.Pool.QueryRow(ctx, `SELECT kind FROM `+s.t.items+` WHERE tenant_id = $1 AND id = $2`, s.o.Tenant, anchorID).Scan(&kind)
	if errors.Is(err, pgx.ErrNoRows) {
		return empty, ErrNotFound
	}
	if err != nil {
		return empty, err
	}
	anchor := contentref.New(s.o.Tenant, kind, anchorID)
	res, err := resolve.One(ctx, s.o.Resolver, anchor, o.Actor)
	if err != nil {
		return empty, err
	}
	if !res.Visible {
		return empty, ErrNotFound
	}
	if o.Kind == "" {
		o.Kind = kind
	}
	q, err := s.compile(ctx, o.Reader, o.Filter)
	if err != nil {
		return empty, err
	}
	a := &admission{s: s, q: q, picked: map[string]pick{}}
	if o.Summary {
		a.candidates = map[string]struct{}{}
	}
	if s.o.Candidates == nil {
		recs.Candidates = Fallback{Primary: recs.Candidates, Secondary: sharedEntities{s: s}, OnError: func(err error) {
			s.o.Logger.WarnContext(ctx, "discovery: co-reading similar failed; using shared entities", "err", err)
		}}
	}
	hits, err := recs.Similar(ctx, anchor, SimilarOptions{Limit: recLimit(o.Limit), ContentKinds: []string{o.Kind}, Admit: a.admit})
	if err != nil {
		return empty, err
	}
	return a.cards(ctx, hits, o)
}

// Recommendations lists contents for the signed-in reader from their
// history, filled from what is popular lately, then ever, then newest, that
// their listing shows; seen and disliked content left out.
func (s *Service) Recommendations(ctx context.Context, o RecommendationOptions) (ContentPage, error) {
	ctx = access.WithMemo(ctx)
	empty := ContentPage{Contents: []Card{}}
	recs, err := s.recommender()
	if err != nil {
		return empty, err
	}
	if !o.Actor.Member() {
		return empty, fmt.Errorf("%w: recommendations need a signed-in reader", ErrInvalid)
	}
	q, err := s.compile(ctx, o.Reader, o.Filter)
	if err != nil {
		return empty, err
	}
	a := &admission{s: s, q: q, picked: map[string]pick{}}
	if o.Summary {
		a.candidates = map[string]struct{}{}
	}
	hits, err := recs.Recommend(ctx, signal.Subject{UserID: o.Actor.AccountID()}, RecommendOptions{ContentKinds: s.searchKinds(o.Kind),
		Limit: recLimit(o.Limit), Backfill: s.newest(q), Admit: a.admit,
		Popular: func(ctx context.Context, kind string, limit int) ([]string, error) {
			key, ok := s.rankingFor(ctx, o.Reader, kind, "")
			if !ok {
				return nil, nil
			}
			return s.topRanked(ctx, key, limit)
		}})
	if err != nil {
		return empty, err
	}
	return a.cards(ctx, hits, o)
}

// newest backfills a "for you" list with the newest contents q lists.
func (s *Service) newest(q *sel) Backfill {
	return func(ctx context.Context, kind string, exclude []string, limit int) ([]contentref.ContentRef, error) {
		o, err := s.parseSort(ctx, SortNewest, Filter{})
		if err != nil {
			return nil, err
		}
		picks, _, err := s.page(ctx, q.with("f.kind = @b_kind AND f.content_id <> ALL(@b_exclude::text[])",
			pgx.NamedArgs{"b_kind": kind, "b_exclude": nonNil(exclude)}), o, "", limit, 0)
		if err != nil {
			return nil, err
		}
		out := make([]contentref.ContentRef, len(picks))
		for i, p := range picks {
			out[i] = contentref.New(s.o.Tenant, kind, p.content)
		}
		return out, nil
	}
}
