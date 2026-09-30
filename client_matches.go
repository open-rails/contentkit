package contentkit

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/contentkit/search"
)

// SearchMatchOptions selects all matches in one language, without a candidate
// window. Hosts use this for catalog-wide non-relevance ordering and counts.
type SearchMatchOptions struct {
	Language     string
	ContentKinds []string
	Eligibility  *Eligibility
	FilterSQL    string
	FilterArgs   map[string]any
}

// WalkSearchMatches visits batches of up to 256 matched works in identity
// order, each carrying its best eligible matched edition. It does not paginate
// or rank the works.
// A successful return means the complete match set was visited; errors must
// not be interpreted as a complete count or page. The visitor receives the
// request's bounded context and must propagate it to downstream work.
// The caller owns tx, using the client's database, and must roll back on error.
func (c *Client) WalkSearchMatches(ctx context.Context, tx pgx.Tx, text string, opts SearchMatchOptions, visit func(context.Context, []SearchHit) error) error {
	if visit == nil {
		return fmt.Errorf("match visitor is required")
	}
	language := strings.TrimSpace(opts.Language)
	if language == "" {
		language = c.defaultLanguage
	}
	kinds := cloneAndTrim(opts.ContentKinds)
	if len(kinds) == 0 {
		return fmt.Errorf("ContentKinds is required")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var current group
	haveCurrent := false
	batch := make([]SearchHit, 0, 256)
	emit := func(g group) error {
		batch = append(batch, hitFromGroup(g))
		if len(batch) == 256 {
			if err := visit(ctx, batch); err != nil {
				return err
			}
			batch = make([]SearchHit, 0, 256)
		}
		return nil
	}
	err := search.WalkKeywordMatches(ctx, tx, text, search.Options{
		Schema: c.schema, Tenant: c.tenant, Language: language, ContentKinds: kinds,
		Eligibility: opts.Eligibility, FilterSQL: opts.FilterSQL, FilterArgs: opts.FilterArgs,
	}, func(h search.Hit) error {
		doc := groupedDoc{ref: h.ContentRef, language: h.Language, priority: h.Priority, score: h.Score, requested: true}
		if haveCurrent && current.best.ref.Content().Key() != h.Content().Key() {
			if err := emit(current); err != nil {
				return err
			}
			haveCurrent = false
		}
		if !haveCurrent {
			current = group{best: doc, representative: doc}
			haveCurrent = true
			return nil
		}
		if rankLess(doc, current.best) {
			current.best = doc
		}
		if representLess(doc, current.representative) {
			current.representative = doc
		}
		return nil
	})
	if err != nil {
		return err
	}
	if haveCurrent {
		if err := emit(current); err != nil {
			return err
		}
	}
	if len(batch) > 0 {
		return visit(ctx, batch)
	}
	return nil
}
