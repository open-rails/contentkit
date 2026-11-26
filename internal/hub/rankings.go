package hub

import (
	"context"

	"github.com/open-rails/contentkit/signal"
)

// Popularity sums content_daily for one site and window; see
// signalstore.Store.Popularity.
func (h *Hub) Popularity(ctx context.Context, q signal.PopularityQuery) ([]signal.ContentPopularity, error) {
	store, err := h.requireStore()
	if err != nil {
		return nil, err
	}
	return store.Popularity(ctx, h.tenant, q)
}
