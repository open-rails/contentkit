package discovery

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/groups"
	"github.com/open-rails/contentkit/internal/hub"
	"github.com/open-rails/contentkit/search"
	"github.com/open-rails/contentkit/signal"
)

// SortRelevance orders search results by match, personalized for a signed-in
// reader.
const SortRelevance = "relevance"

// sortedMatches bounds the matches a non-relevance search sorts.
const sortedMatches = 1000

// SearchOptions select one page of keyword search.
type SearchOptions struct {
	Reader
	Filter
	Query string
	// Sort is relevance (the default) or a list sort of the best matches.
	Sort       string
	CardGroups []string
	Cursor     string
	// Limit defaults to 24 and is capped at 100.
	Limit int
	// Unpersonalized ranks a signed-in reader's results as anyone's.
	Unpersonalized bool
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
func (s *Service) Search(ctx context.Context, o SearchOptions) (ContentPage, error) {
	empty := ContentPage{Contents: []Card{}}
	text := strings.TrimSpace(o.Query)
	if text == "" {
		return empty, fmt.Errorf("%w: q is required", ErrInvalid)
	}
	q, err := s.compile(ctx, o.Reader, o.Filter)
	if err != nil {
		return empty, err
	}
	limit := pageLimit(o.Limit, 24)
	base := hub.SearchOptions{Language: q.display, LanguageMode: s.languageMode(), ContentKinds: s.searchKinds(o.Kind), Eligibility: s.eligibility(q)}
	if o.Sort != "" && o.Sort != SortRelevance {
		srt, err := s.parseSort(o.Sort, o.Filter)
		if err != nil {
			return empty, err
		}
		base.Limit = sortedMatches
		res, err := s.o.Hub.Search(ctx, text, hub.HubSearchOptions{SearchOptions: base})
		if err != nil {
			return empty, err
		}
		ids := make([]string, 0, len(res.Hits))
		for _, h := range res.Hits {
			ids = append(ids, h.ContentID)
		}
		q.and("f.content_id = ANY(@d_matches::text[])", pgx.NamedArgs{"d_matches": ids})
		picks, next, err := s.page(ctx, q, srt, o.Cursor, limit)
		if err != nil {
			return empty, err
		}
		cards, err := s.cards(ctx, q, picks, cardOptions{chips: o.CardGroups})
		return ContentPage{Contents: cards, NextCursor: next}, err
	}
	cur, err := decodeCursor(o.Cursor, SortRelevance)
	if err != nil {
		return empty, err
	}
	offset := 0
	if cur.ID != "" {
		if offset, err = strconv.Atoi(cur.Value); err != nil || offset < 0 {
			return empty, fmt.Errorf("%w: cursor", ErrInvalid)
		}
	}
	base.Limit, base.Offset = limit, offset
	opts := hub.HubSearchOptions{SearchOptions: base}
	if a := o.Actor; a.Person() && !o.Unpersonalized {
		opts.Personalize = &hub.Personalization{Subject: signal.Subject{UserID: a.ID}, DemoteSeen: true,
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
	cards, err := s.cards(ctx, q, picks, cardOptions{chips: o.CardGroups})
	out := ContentPage{Contents: cards}
	if res.HasMore && len(res.Hits) > 0 {
		out.NextCursor = cursor{Sort: SortRelevance, Value: strconv.Itoa(offset + len(res.Hits)), ID: ids[len(ids)-1]}.encode()
	}
	return out, err
}

// TypeaheadOptions select suggestions while a reader types.
type TypeaheadOptions struct {
	Reader
	Query string
	// Kind limits content suggestions to one kind.
	Kind string
	// GroupKinds are the group kinds suggested; every kind with a page by
	// default.
	GroupKinds []string
	// Limit is per section: 8 by default, at most 20.
	Limit int
}

// Typeahead is the suggestions: content cards and groups, best first.
type Typeahead struct {
	Contents []Card             `json:"contents"`
	Groups   []groups.GroupCard `json:"groups"`
}

// Typeahead suggests contents the reader's listing shows and active groups,
// tolerating typos and partial words.
func (s *Service) Typeahead(ctx context.Context, o TypeaheadOptions) (Typeahead, error) {
	out := Typeahead{Contents: []Card{}, Groups: []groups.GroupCard{}}
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
	kinds := o.GroupKinds
	if len(kinds) == 0 {
		for k, gk := range s.o.Kinds.GroupKinds {
			if gk.Route != "" {
				kinds = append(kinds, k)
			}
		}
		slices.Sort(kinds)
	}
	for _, k := range kinds {
		if _, ok := s.o.Kinds.GroupKinds[k]; !ok {
			return out, fmt.Errorf("%w: group kind %q is not configured", ErrInvalid, k)
		}
	}
	if len(kinds) == 0 {
		return out, nil
	}
	ghits, err := s.o.Hub.Typeahead(ctx, text, hub.TypeaheadOptions{Language: q.display, LanguageMode: s.languageMode(), ContentKinds: kinds, Limit: limit,
		Eligibility: &search.Eligibility{SQL: `SELECT 0 AS priority WHERE EXISTS (SELECT 1 FROM ` + s.t.groups +
			` g WHERE g.tenant_id = sd.tenant_id AND g.id = sd.content_id AND g.kind = sd.content_kind AND g.state = 'active')`}})
	if err != nil {
		return out, err
	}
	gids := make([]groups.GroupID, 0, len(ghits))
	for _, h := range ghits {
		gids = append(gids, groups.GroupID(h.ContentID))
	}
	cards, err := s.o.Groups.Cards(ctx, gids, groups.CardOptions{Language: q.path, Covers: true})
	if err != nil {
		return out, err
	}
	rank := make(map[groups.GroupID]int, len(gids))
	for i, id := range gids {
		rank[id] = i
	}
	slices.SortFunc(cards, func(a, b groups.GroupCard) int { return rank[a.ID] - rank[b.ID] })
	out.Groups = cards
	return out, nil
}

// now is the service clock.
func (s *Service) now() time.Time { return s.o.Now() }
