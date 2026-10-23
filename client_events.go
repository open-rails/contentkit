package contentkit

import (
	"context"

	"github.com/open-rails/contentkit/catalog"
)

// ListEvents reads a retained page without registering or acknowledging a reader.
func (c *Client) ListEvents(ctx context.Context, query catalog.EventQuery) (catalog.EventPage, error) {
	return c.GetEvents(ctx, GetEventsQuery{After: query.After, Limit: query.Limit, Types: query.Types})
}
