package contentkit

import (
	"context"

	"github.com/open-rails/contentkit/eval"
)

// NewEvalRunner adapts a Client to eval.CaseRunner so a golden suite can be
// executed against real search. The base options carry cross-case settings
// (LanguageMode, Semantic, RRFK, filters); each case overrides Language,
// ContentKinds, and Limit from its own definition.
//
// This adapter is the single seam where the client meets the dependency-free
// eval package.
func NewEvalRunner(client *Client, base SearchOptions) eval.CaseRunner {
	return clientRunner{client: client, base: base}
}

type clientRunner struct {
	client *Client
	base   SearchOptions
}

func (r clientRunner) Run(ctx context.Context, c eval.GoldenCase) ([]eval.Result, string, error) {
	opts := r.base
	if c.Language != "" {
		opts.Language = c.Language
	}
	if len(c.ContentKinds) > 0 {
		opts.ContentKinds = c.ContentKinds
	}
	opts.Limit = c.K

	page, err := r.client.Search(ctx, c.Query, opts)
	if err != nil {
		return nil, "search", err
	}

	// Golden cases judge content items, so the work is the graded key.
	results := make([]eval.Result, len(page.Hits))
	for i, hit := range page.Hits {
		results[i] = eval.Result{
			Key:   eval.GoldenKey{ContentKind: hit.ContentKind, ContentID: hit.ContentID},
			Score: hit.Score,
		}
	}
	return results, "", nil
}
