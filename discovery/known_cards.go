package discovery

import (
	"context"
	"fmt"

	"github.com/open-rails/contentkit/access"
)

// KnownCardsOptions selects a bounded set of already-known IDs, not a listing.
type KnownCardsOptions struct {
	Reader
	IDs          []string
	Kind         string
	CardEntities []string
	Summary      bool
}

// KnownCards hydrates explicit IDs in caller order.
// No publication, domain, preference, access or deletion rule is relaxed.
func (s *Service) KnownCards(ctx context.Context, o KnownCardsOptions) (ContentPage, error) {
	ctx = access.WithMemo(ctx)
	out := ContentPage{Contents: []Card{}}
	if len(o.IDs) == 0 || len(o.IDs) > 100 {
		return out, fmt.Errorf("%w: 1-100 content ids", ErrInvalid)
	}
	filter := Filter{Kind: o.Kind, IDs: o.IDs, keepUnplayable: true}
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
		sort, err := s.parseSort(ctx, SortNewest, Filter{Kind: o.Kind})
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
