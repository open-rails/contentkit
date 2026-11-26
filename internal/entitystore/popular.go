package entitystore

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	entities "github.com/open-rails/contentkit/entities"
	"github.com/open-rails/contentkit/internal/gate"
	"github.com/open-rails/contentkit/internal/rankings"
)

// popular pages one kind by its ranking on the request's site: the ranking's
// chunks read with the listing's filters, so it drops only what they block.
func (s *Store) popular(ctx context.Context, opts ListOptions, scope readScope) (entities.EntityPage, error) {
	empty := entities.EntityPage{Entities: []entities.EntityCard{}}
	kind := strings.TrimSpace(opts.Kind)
	switch {
	case s.rankings == nil || s.channels:
		return empty, fmt.Errorf("%w: sort popular needs the signal plane and a descriptive entity kind", entities.ErrInvalid)
	case kind == "":
		return empty, fmt.Errorf("%w: sort popular lists one kind", entities.ErrInvalid)
	case opts.Page < 0 || opts.Page > 0 && opts.Cursor != "":
		return empty, fmt.Errorf("%w: page", entities.ErrInvalid)
	case strings.TrimSpace(opts.Query) != "" || opts.IDs != nil || opts.Deleted:
		return empty, fmt.Errorf("%w: sort popular lists active entities without q or ids", entities.ErrInvalid)
	}
	if _, err := s.entityKind(kind); err != nil {
		return empty, err
	}
	w, err := rankings.ParseWindow(opts.Window, s.rankings.Now())
	if err != nil {
		return empty, fmt.Errorf("%w: %w", entities.ErrInvalid, err)
	}
	var cur rankings.Cursor
	if opts.Cursor != "" {
		var ok bool
		if cur, ok = rankings.DecodeCursor(opts.Cursor); !ok {
			return empty, fmt.Errorf("%w: cursor", entities.ErrInvalid)
		}
	}
	language := opts.Language
	if strings.TrimSpace(language) == "" {
		language = opts.SiteLanguage
	}
	site := s.rankings.Site(gate.Domain(ctx), language)
	r, err := s.rankings.Ranking(ctx, site, w, rankings.List{Kind: kind, Entities: true}, cur.Version)
	if errors.Is(err, rankings.ErrInvalid) {
		return empty, fmt.Errorf("%w: %w", entities.ErrInvalid, err)
	}
	if err != nil {
		return empty, err
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = 50
	}
	limit = min(limit, 200)
	skip := 0
	if opts.Page > 0 {
		if opts.Page > maxPageOffset/limit {
			return empty, fmt.Errorf("%w: page × limit is at most %d", entities.ErrInvalid, maxPageOffset)
		}
		skip = (opts.Page - 1) * limit
	}
	l := &entityLister{s: s, opts: opts, scope: scope, cards: map[string]entities.EntityCard{}}
	ids, next, err := rankings.Page(ctx, r, l, cur, limit, skip)
	if err != nil {
		return empty, err
	}
	page := entities.EntityPage{Entities: make([]entities.EntityCard, 0, len(ids)), Popularity: popularWindow(w, r)}
	if opts.Page > 0 {
		next = nil
		o := opts
		o.Sort, o.Cursor, o.chunk = entities.SortCount, "", false
		c, err := s.compile(o, nil, scope)
		if err != nil {
			return empty, err
		}
		var total int
		if err := s.read(ctx, func(q Querier) error { return q.QueryRow(ctx, c.count, c.args).Scan(&total) }); err != nil {
			return empty, err
		}
		page.Total = &total
	}
	var artists []string
	for _, id := range ids {
		card := l.cards[id]
		if p, ok := r.Position(id); ok {
			card.Popularity = &entities.EntityPopularity{Score: r.Stats[p].Score, Polarization: float64(r.Stats[p].Polarization)}
		}
		page.Entities = append(page.Entities, card)
		artists = append(artists, id)
	}
	if s.cfg.EntityKinds[kind].Artist && len(artists) > 0 {
		rates, err := s.rankings.ReadThrough(ctx, artists)
		if err != nil {
			return empty, err
		}
		for i := range page.Entities {
			g := &page.Entities[i]
			if rate, ok := rates[string(g.ID)]; ok {
				if g.Popularity == nil {
					g.Popularity = &entities.EntityPopularity{}
				}
				g.Popularity.ReadThrough = &entities.ReadThrough{Rate: rate.Rate, Readers: rate.Readers}
			}
		}
	}
	if next != nil {
		page.NextCursor = next.Encode()
	}
	shows := make([]shown, len(page.Entities))
	for i := range page.Entities {
		shows[i] = cardShown(&page.Entities[i])
	}
	return page, s.fill(ctx, shows, opts.Language)
}

func popularWindow(w rankings.Window, r *rankings.Ranking) *entities.PopularWindow {
	out := &entities.PopularWindow{Window: w.Name, From: w.From, Until: w.Until, Ready: r.Ready, Stale: r.Stale}
	if r.Ready {
		at := r.Computed
		out.ComputedAt = &at
	}
	return out
}

// entityLister reads a popular listing's chunks and rest with its filters,
// keeping the cards it read for the page.
type entityLister struct {
	s     *Store
	opts  ListOptions
	scope readScope
	cards map[string]entities.EntityCard
}

func (l *entityLister) query(ctx context.Context, opts ListOptions, ids []string) ([]entities.EntityCard, error) {
	opts.Sort, opts.Page, opts.chunk = entities.SortCount, 0, true
	c, err := l.s.compile(opts, ids, l.scope)
	if err != nil {
		return nil, err
	}
	var page entities.EntityPage
	err = l.s.read(ctx, func(q Querier) error {
		page, err = l.s.cards(ctx, q, c)
		return err
	})
	for _, g := range page.Entities {
		l.cards[string(g.ID)] = g
	}
	return page.Entities, err
}

func (l *entityLister) Admit(ctx context.Context, ids []string) ([]string, error) {
	o := l.opts
	o.Cursor, o.Limit = "", len(ids)
	cards, err := l.query(ctx, o, ids)
	if err != nil {
		return nil, err
	}
	kept := map[string]bool{}
	for _, g := range cards {
		kept[string(g.ID)] = true
	}
	out := make([]string, 0, len(kept))
	for _, id := range ids {
		if kept[id] {
			out = append(out, id)
		}
	}
	return out, nil
}

// Rest lists by content count, the order of entities without activity.
func (l *entityLister) Rest(ctx context.Context, after string, n int) ([]rankings.Row, error) {
	o := l.opts
	o.Cursor, o.Limit = after, n
	cards, err := l.query(ctx, o, nil)
	if err != nil {
		return nil, err
	}
	rows := make([]rankings.Row, len(cards))
	for i, g := range cards {
		b, _ := json.Marshal(listCursor{Count: g.Count, ID: string(g.ID)})
		rows[i] = rankings.Row{ID: string(g.ID), Key: base64.RawURLEncoding.EncodeToString(b)}
	}
	return rows, nil
}
