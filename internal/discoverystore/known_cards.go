package discoverystore

import (
	"context"
	"fmt"

	discovery "github.com/open-rails/contentkit/discovery"
	entitlement "github.com/open-rails/contentkit/internal/entitlement"
)

// KnownCards hydrates explicit IDs in caller order.
// No publication, domain, preference, access or deletion rule is relaxed.
func (s *Service) KnownCards(ctx context.Context, o discovery.KnownCardsOptions) (discovery.ContentPage, error) {
	ctx = entitlement.WithMemo(ctx)
	out := discovery.ContentPage{Contents: []discovery.Card{}}
	if len(o.IDs) == 0 || len(o.IDs) > 100 {
		return out, fmt.Errorf("%w: 1-100 content ids", ErrInvalid)
	}
	filter := Filter{Kind: o.Kind, IDs: o.IDs}
	q, err := s.compileKnown(ctx, o.Reader, filter)
	if err != nil {
		return out, err
	}
	picks, err := s.pickAmong(ctx, q, o.IDs)
	if err != nil {
		return out, err
	}
	out.Contents, err = s.cards(ctx, q, picks, cardOptions{chips: o.CardEntities})
	if err != nil {
		return out, err
	}
	if o.Summary {
		sort, err := s.parseSort(ctx, discovery.SortNewest, Filter{Kind: o.Kind})
		if err != nil {
			return out, err
		}
		out.Summary, err = s.listingSummary(ctx, filter, q, sort, nil)
		if err != nil {
			return out, err
		}
	}
	return out, nil
}
