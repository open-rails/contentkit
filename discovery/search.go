package discovery

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/entities"
	"github.com/open-rails/contentkit/internal/codes"
	"github.com/open-rails/contentkit/internal/hub"
	"github.com/open-rails/contentkit/search"
	"github.com/open-rails/contentkit/signal"
)

// SortRelevance orders search results by match, personalized for a signed-in
// reader.
const SortRelevance = "relevance"

// SearchOptions select one page of keyword search.
type SearchOptions struct {
	Reader
	Filter
	Query string
	// Sort is relevance (the default) or a list sort of the best matches.
	Sort         string
	CardEntities []string
	Cursor       string
	// Page numbers pages from 1 instead of Cursor.
	Page int
	// Limit defaults to 24 and is capped at 100.
	Limit int
	// Unpersonalized ranks a signed-in reader's results as anyone's.
	Unpersonalized bool
	// Summary adds contextual counts on the first page, or on every numbered
	// page.
	Summary bool
}

// eligibility keeps the content documents whose item the reader's listing
// shows and the filter matches.
func (s *Service) eligibility(q *sel) *search.Eligibility {
	args := make(map[string]any, len(q.args))
	for k, v := range q.args {
		args[k] = v
	}
	return &search.Eligibility{SQL: fmt.Sprintf(`SELECT 0 AS priority WHERE EXISTS (SELECT 1 FROM %s f
 WHERE f.content_id = sd.content_id AND f.kind = sd.content_kind AND %s)`, s.t.facets, q.predicate()), Args: args}
}

func (s *Service) searchKinds(kind string) []string {
	if kind != "" {
		return []string{kind}
	}
	return s.kindNames()
}

func (s *Service) languageMode() hub.LanguageMode {
	if slices.Contains(s.languages(), "en") {
		return hub.LanguageModeFallbackEnglish
	}
	return hub.LanguageModeExact
}

// SearchPage is a page of search results. The first page also carries the
// entities matching the query that have contents the reader can see.
type SearchPage struct {
	ContentPage
	Entities []entities.EntityCard `json:"entities,omitempty"`
}

// Search pages the contents matching a keyword query, grouped per content
// item, under the reader's visibility and the filter.
func (s *Service) Search(ctx context.Context, o SearchOptions) (result SearchPage, resultErr error) {
	ctx = access.WithMemo(ctx)
	empty := SearchPage{ContentPage: ContentPage{Contents: []Card{}}}
	text := strings.TrimSpace(o.Query)
	if text == "" {
		return empty, fmt.Errorf("%w: q is required", ErrInvalid)
	}
	var err error
	sortName := o.Sort
	if sortName == "" {
		sortName = SortRelevance
	}
	o.Filter, err = publicationCursor(o.Filter, o.Cursor, sortName)
	if err != nil {
		return empty, err
	}
	if o.Filter.named, err = s.namedEntities(ctx, text); err != nil {
		return empty, err
	}
	q, err := s.compile(ctx, o.Reader, o.Filter)
	if err != nil {
		return empty, err
	}
	var summaryOrder order
	firstPage := o.Cursor == "" && o.Page <= 1
	entityMatch := s.entityMatch(q)
	defer func() {
		if resultErr == nil && o.Summary && o.Cursor == "" {
			result.Summary, resultErr = s.searchSummary(ctx, o, q, summaryOrder)
		}
		if resultErr == nil && firstPage {
			result.Entities, resultErr = s.matchingEntities(ctx, text, q.display, entityMatch)
		}
	}()
	limit := pageLimit(o.Limit, 24)
	offset, err := pageOffset(o.Page, limit, o.Cursor, sortName)
	if err != nil {
		return empty, err
	}
	base := hub.SearchOptions{Language: q.display, LanguageMode: s.languageMode(), ContentKinds: s.searchKinds(o.Kind), Eligibility: s.eligibility(q)}
	if o.Sort != "" && o.Sort != SortRelevance {
		var srt order
		var snapshot *PopularitySnapshot
		if id, popular := popularSort(o.Sort); popular {
			srt, snapshot, err = s.popularOrder(ctx, o.Kind, id, o.Reader, o.Sort)
		} else {
			srt, err = s.parseSort(ctx, o.Sort, o.Filter)
		}

		if err != nil {
			return empty, err
		}
		summaryOrder = srt
		match, err := s.o.Hub.KeywordMatch(ctx, text, base)
		if err != nil {
			return empty, err
		}
		q.and("f.content_id IN ("+match.SQL+")", match.Args)
		picks, next, err := s.page(ctx, q, srt, o.Cursor, limit, offset)
		if err != nil {
			return empty, err
		}
		cards, err := s.cards(ctx, q, picks, cardOptions{chips: o.CardEntities})
		return SearchPage{ContentPage: ContentPage{Contents: cards, NextCursor: next, Popularity: snapshot}}, err
	}
	cur, err := decodeCursor(o.Cursor, SortRelevance)
	if err != nil {
		return empty, err
	}
	if cur.ID != "" {
		if offset, err = strconv.Atoi(cur.Value); err != nil || offset < 0 {
			return empty, fmt.Errorf("%w: cursor", ErrInvalid)
		}
	}
	base.Limit, base.Offset = limit, offset
	opts := hub.HubSearchOptions{SearchOptions: base}
	if a := o.Actor; a.Member() && !o.Unpersonalized {
		opts.Personalize = &hub.Personalization{Subject: signal.Subject{UserID: a.AccountID()}, DemoteSeen: true,
			Popularity: func(ctx context.Context, kind string, ids []string) (map[string]float64, error) {
				key, ok := s.rankingFor(ctx, o.Reader, kind, "")
				if !ok {
					return nil, nil
				}
				return s.rankedScores(ctx, key, ids)
			}}
	}
	res, err := s.o.Hub.Search(ctx, text, opts)
	if err != nil {
		return empty, err
	}
	ids := make([]string, 0, len(res.Hits))
	for _, h := range res.Hits {
		ids = append(ids, h.ContentID)
	}
	picks, err := s.pickAmong(ctx, q, ids)
	if err != nil {
		return empty, err
	}
	cards, err := s.cards(ctx, q, picks, cardOptions{chips: o.CardEntities})
	out := SearchPage{ContentPage: ContentPage{Contents: cards}}
	if res.HasMore && len(res.Hits) > 0 {
		out.NextCursor = cursor{Sort: SortRelevance, Value: strconv.Itoa(offset + len(res.Hits)), ID: ids[len(ids)-1], AsOf: q.asOf}.encode()
	}
	return out, err
}

// TypeaheadOptions select suggestions while a reader types.
type TypeaheadOptions struct {
	Reader
	Query string
	// Kind limits content suggestions to one kind.
	Kind string
	// EntityKinds are the entity kinds suggested; every kind with a page by
	// default.
	EntityKinds []string
	// Limit is per section: 8 by default, at most 20.
	Limit int
}

// Typeahead is the suggestions: content cards and entities, best first.
type Typeahead struct {
	Contents []Card                `json:"contents"`
	Entities []entities.EntityCard `json:"entities"`
}

// Typeahead suggests contents the reader's listing shows and active entities,
// matching literal substrings of their own titles, names or aliases, with
// prefix bias. Full search separately handles contextual keywords and typos.
func (s *Service) Typeahead(ctx context.Context, o TypeaheadOptions) (Typeahead, error) {
	out := Typeahead{Contents: []Card{}, Entities: []entities.EntityCard{}}
	text := strings.TrimSpace(o.Query)
	if text == "" {
		return out, nil
	}
	limit := o.Limit
	if limit <= 0 {
		limit = 8
	}
	limit = min(limit, 20)
	q, err := s.compile(ctx, o.Reader, Filter{Kind: o.Kind})
	if err != nil {
		return out, err
	}
	hits, err := s.o.Hub.Typeahead(ctx, text, hub.TypeaheadOptions{Language: q.display, LanguageMode: s.languageMode(),
		ContentKinds: s.searchKinds(o.Kind), Limit: limit, Eligibility: s.eligibility(q)})
	if err != nil {
		return out, err
	}
	ids := make([]string, 0, len(hits))
	for _, h := range hits {
		ids = append(ids, h.ContentID)
	}
	picks, err := s.pickAmong(ctx, q, ids)
	if err != nil {
		return out, err
	}
	if out.Contents, err = s.cards(ctx, q, picks, cardOptions{lean: true}); err != nil {
		return out, err
	}
	kinds := o.EntityKinds
	if len(kinds) == 0 {
		for k, gk := range s.o.Kinds.EntityKinds {
			if gk.Route != "" {
				kinds = append(kinds, k)
			}
		}
		slices.Sort(kinds)
	}
	for _, k := range kinds {
		if _, ok := s.o.Kinds.EntityKinds[k]; !ok {
			return out, fmt.Errorf("%w: group kind %q is not configured", ErrInvalid, k)
		}
	}
	if len(kinds) == 0 {
		return out, nil
	}
	out.Entities, err = s.entityTypeahead(ctx, text, q.display, kinds, limit, &search.Eligibility{SQL: `SELECT 0 AS priority WHERE EXISTS (SELECT 1 FROM ` + s.t.entities +
		` g WHERE g.tenant_id = sd.tenant_id AND g.id = sd.content_id AND g.kind = sd.content_kind AND g.state = 'active' AND NOT g.internal)`})
	return out, err
}

// entityMatch keeps the active public entities with a content q selects.
// It copies q now, before Search narrows q to the keyword matches.
func (s *Service) entityMatch(q *sel) *search.Eligibility {
	return &search.Eligibility{SQL: fmt.Sprintf(`SELECT 0 AS priority FROM %s g
 WHERE g.tenant_id = sd.tenant_id AND g.id = sd.content_id AND g.kind = sd.content_kind AND g.state = 'active' AND NOT g.internal
 AND EXISTS (SELECT 1 FROM %s f WHERE (f.entity_ids @> ARRAY[g.id] OR f.owner_entity_id = g.id) AND %s)`, s.t.entities, s.t.facets, q.predicate()),
		Args: maps.Clone(q.args)}
}

// matchingEntities are up to 8 routed entities whose names match text.
func (s *Service) matchingEntities(ctx context.Context, text, language string, eligible *search.Eligibility) ([]entities.EntityCard, error) {
	var kinds []string
	for k, ek := range s.o.Kinds.EntityKinds {
		if ek.Route != "" {
			kinds = append(kinds, k)
		}
	}
	if len(kinds) == 0 {
		return nil, nil
	}
	slices.Sort(kinds)
	return s.entityTypeahead(ctx, text, language, kinds, 8, eligible)
}

// entityTypeahead returns the cards of the eligible entities of kinds whose
// names match text, best match first.
func (s *Service) entityTypeahead(ctx context.Context, text, language string, kinds []string, limit int, eligible *search.Eligibility) ([]entities.EntityCard, error) {
	hits, err := s.o.Hub.Typeahead(ctx, text, hub.TypeaheadOptions{Language: language, LanguageMode: s.languageMode(), ContentKinds: kinds, Limit: limit, Eligibility: eligible})
	if err != nil {
		return nil, err
	}
	ids := make([]entities.EntityID, 0, len(hits))
	for _, h := range hits {
		ids = append(ids, entities.EntityID(h.ContentID))
	}
	cards, err := s.o.Entities.Cards(ctx, ids, entities.CardOptions{Language: language, Covers: true})
	if err != nil {
		return nil, err
	}
	rank := make(map[entities.EntityID]int, len(ids))
	for i, id := range ids {
		rank[id] = i
	}
	slices.SortFunc(cards, func(a, b entities.EntityCard) int { return rank[a.ID] - rank[b.ID] })
	return cards, nil
}

// now is the service clock.
func (s *Service) now() time.Time { return s.o.Now() }

// namedEntities are the active public entities a query names: the whole query
// or one of its words is a name, alias or slug of the entity. Searching for an
// entity the reader hides shows its contents (Doujins #211); filters a domain
// requires still apply.
func (s *Service) namedEntities(ctx context.Context, text string) ([]string, error) {
	terms := append([]string{text}, strings.Fields(text)...)
	slugs := make([]string, len(terms))
	for i, t := range terms {
		slugs[i] = codes.Slugify(t)
	}
	rows, err := s.o.Pool.Query(ctx, fmt.Sprintf(`SELECT g.id FROM %[1]s g WHERE g.tenant_id = $1 AND g.state = 'active' AND NOT g.internal
 AND (g.slug = ANY($3::text[]) OR EXISTS (SELECT 1 FROM %[2]s n WHERE n.tenant_id = g.tenant_id AND n.entity_id = g.id
  AND n.normalized = ANY(SELECT %[3]s(t) FROM unnest($2::text[]) t)))`, s.t.entities, s.t.entityNames, s.t.normalize),
		s.o.Tenant, terms, slugs)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}
