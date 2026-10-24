package discovery

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/entities"
	"github.com/open-rails/contentkit/internal/hub"
	"github.com/open-rails/contentkit/popularity"
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

// Search pages the contents matching a keyword query, grouped per content
// item, under the reader's visibility and the filter.
func (s *Service) Search(ctx context.Context, o SearchOptions) (result ContentPage, resultErr error) {
	ctx = access.WithMemo(ctx)
	empty := ContentPage{Contents: []Card{}}
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
	q, err := s.compile(ctx, o.Reader, o.Filter)
	if err != nil {
		return empty, err
	}
	var summaryOrder order
	defer func() {
		if resultErr == nil && o.Summary && o.Cursor == "" {
			result.Summary, resultErr = s.searchSummary(ctx, o, q, summaryOrder)
		}
	}()
	limit := pageLimit(o.Limit, 24)
	offset, err := pageOffset(o.Page, limit, o.Cursor, sortName)
	if err != nil {
		return empty, err
	}
	base := hub.SearchOptions{Language: q.display, LanguageMode: s.languageMode(), ContentKinds: s.searchKinds(o.Kind), Eligibility: s.eligibility(q)}
	if o.Sort != "" && o.Sort != SortRelevance {
		srt, err := s.parseSort(ctx, o.Sort, o.Filter)
		if name, period, _ := strings.Cut(o.Sort, ":"); err == nil && name == SortPopular && o.Kind != "" && s.o.Hub.SignalsConfigured() {
			if period == "" {
				period = popularity.DefaultPeriod
			}
			window, windowErr := popularity.WindowForPeriod(period, s.now())
			if windowErr != nil {
				return empty, windowErr
			}
			srt, err = s.historicalOrder(ctx, o.Kind, window, o.Reader)
			srt.raw = o.Sort
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
		for i := range cards {
			if ranking, ok := srt.rankings[cards[i].ID]; ok {
				cards[i].Ranking = &ranking
			}
		}
		return ContentPage{Contents: cards, NextCursor: next, Ranking: srt.ranking}, err
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
			PopularityWindow: signal.LastDays(30, s.o.Now())}
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
	out := ContentPage{Contents: cards}
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
	ghits, err := s.o.Hub.Typeahead(ctx, text, hub.TypeaheadOptions{Language: q.display, LanguageMode: s.languageMode(), ContentKinds: kinds, Limit: limit,
		Eligibility: &search.Eligibility{SQL: `SELECT 0 AS priority WHERE EXISTS (SELECT 1 FROM ` + s.t.entities +
			` g WHERE g.tenant_id = sd.tenant_id AND g.id = sd.content_id AND g.kind = sd.content_kind AND g.state = 'active' AND NOT g.internal)`}})
	if err != nil {
		return out, err
	}
	gids := make([]entities.EntityID, 0, len(ghits))
	for _, h := range ghits {
		gids = append(gids, entities.EntityID(h.ContentID))
	}
	cards, err := s.o.Entities.Cards(ctx, gids, entities.CardOptions{Language: q.display, Covers: true})
	if err != nil {
		return out, err
	}
	rank := make(map[entities.EntityID]int, len(gids))
	for i, id := range gids {
		rank[id] = i
	}
	slices.SortFunc(cards, func(a, b entities.EntityCard) int { return rank[a.ID] - rank[b.ID] })
	out.Entities = cards
	return out, nil
}

// now is the service clock.
func (s *Service) now() time.Time { return s.o.Now() }
