package contentkit

import (
	"context"
	"net/http"

	"github.com/open-rails/contentkit/importer"
)

// PostImport imports a batch (POST /v1/import): records failing are in the
// result, the rest are written; re-sending converges.
func (c *Client) PostImport(ctx context.Context, body importer.Batch) (importer.Result, error) {
	var out importer.Result
	err := c.call(ctx, http.MethodPost, "/v1/import", body, &out)
	return out, err
}
