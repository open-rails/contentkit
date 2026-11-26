package discoverystore

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	discovery "github.com/open-rails/contentkit/discovery"
	"github.com/open-rails/contentkit/internal/gate"
	"github.com/open-rails/contentkit/internal/rankings"
)

// popularListsEverything: a popular list shows every content its filters
// keep, the ones without activity after the ranked ones, as entity listings
// do. False would list only the ranked contents. (The owner has yet to
// confirm galleries work like entities.)
const popularListsEverything = true

// maxSelection is the largest selection a popular list orders in memory;
// a larger one pages the ranking in chunks.
const maxSelection = 20_000

// popularQuery is one page of a popular list.
type popularQuery struct {
	reader       discovery.Reader
	kind         string
	window       string
	siteLanguage string
	cursor       string
	limit        int
	// offset is a numbered page's.
	offset int
	// niche lists one entity's contents: they rank by their audience's
	// popularity, so a polarizing work ranks high where it is wanted.
	niche bool
	// selective filters (entities, a channel, creators, ids, a keyword
	// match) keep few contents: order the selection instead of paging the
	// ranking.
	selective bool
}

// site is the reader's site: the host's domain and the explicit language,
// else the request's Accept-Language, else the first configured.
func (s *Service) site(ctx context.Context, r discovery.Reader, siteLanguage string) rankings.Site {
	host := r.Host
	if host == "" {
		host = gate.Domain(ctx)
	}
	return s.o.Rankings.Site(host, cmpOr(r.Language, siteLanguage))
}

// popular pages q by its kind's popularity on the reader's site over the
// window: a chunk of the cached ranking at a time, read with q's filters and
// access, in order.
func (s *Service) popular(ctx context.Context, q *sel, p popularQuery) ([]pick, string, *discovery.PopularitySnapshot, error) {
	if p.kind == "" {
		return nil, "", nil, fmt.Errorf("%w: sort popular needs a kind", ErrInvalid)
	}
	w, err := rankings.ParseWindow(p.window, s.o.Rankings.Now())
	if err != nil {
		return nil, "", nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	var cur rankings.Cursor
	if p.cursor != "" {
		var ok bool
		if cur, ok = rankings.DecodeCursor(p.cursor); !ok {
			return nil, "", nil, fmt.Errorf("%w: cursor", ErrInvalid)
		}
	}
	r, err := s.o.Rankings.Ranking(ctx, s.site(ctx, p.reader, p.siteLanguage), w, rankings.List{Kind: p.kind, Audience: p.niche}, cur.Version)
	if errors.Is(err, rankings.ErrInvalid) {
		return nil, "", nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if err != nil {
		return nil, "", nil, err
	}
	snapshot := &discovery.PopularitySnapshot{Window: w.Name, From: w.From, Until: w.Until, Ready: r.Ready, Stale: r.Stale}
	if r.Ready {
		at := r.Computed
		snapshot.ComputedAt = &at
	}
	var ids []string
	var next *rankings.Cursor
	selected := false
	if p.selective {
		ids, next, selected, err = s.popularSelection(ctx, q, r, cur, p.limit, p.offset)
	}
	if !selected && err == nil {
		ids, next, err = rankings.Page(ctx, r, contentLister{s, q}, cur, p.limit, p.offset)
	}
	if err != nil {
		return nil, "", nil, err
	}
	picks, err := s.pickAmong(ctx, q, ids)
	if err != nil {
		return nil, "", nil, err
	}
	token := ""
	if next != nil {
		token = next.Encode()
	}
	return picks, token, snapshot, nil
}

// popularSelection orders a selective filter's contents by the ranking in
// memory; false when the selection is too large to.
func (s *Service) popularSelection(ctx context.Context, q *sel, r *rankings.Ranking, cur rankings.Cursor, limit, skip int) ([]string, *rankings.Cursor, bool, error) {
	args := pgx.NamedArgs{"p_limit": maxSelection + 1}
	for k, v := range q.args {
		args[k] = v
	}
	rows, err := s.o.Pool.Query(ctx, fmt.Sprintf(`SELECT f.content_id, max(coalesce(f.published_at, 'epoch'::timestamptz)) FROM %s f WHERE %s
 GROUP BY f.content_id LIMIT @p_limit`, s.t.facets, q.predicate()), args)
	if err != nil {
		return nil, nil, false, err
	}
	type selected struct {
		id        string
		part, pos int
		published time.Time
	}
	var all []selected
	var x selected
	_, err = pgx.ForEachRow(rows, []any{&x.id, &x.published}, func() error {
		x.part, x.pos = 1, 0
		if p, ok := r.Position(x.id); ok {
			x.part, x.pos = 0, p
			if r.Stats[p].Score < 0 {
				x.part = 2
			}
		}
		if x.part == 1 && !popularListsEverything {
			return nil
		}
		all = append(all, x)
		return nil
	})
	if err != nil || len(all) > maxSelection {
		return nil, nil, false, err
	}
	slices.SortFunc(all, func(a, b selected) int {
		return cmp.Or(cmp.Compare(a.part, b.part), cmp.Compare(a.pos, b.pos), b.published.Compare(a.published), strings.Compare(b.id, a.id))
	})
	offset := min(skip, len(all))
	if cur.Version == r.Version && cur.Pos > 0 {
		offset = min(cur.Pos, len(all))
	}
	end := min(offset+limit, len(all))
	ids := make([]string, 0, end-offset)
	for _, c := range all[offset:end] {
		ids = append(ids, c.id)
	}
	if end == len(all) {
		return ids, nil, true, nil
	}
	return ids, &rankings.Cursor{Version: r.Version, Pos: end}, true, nil
}

// contentLister reads a popular list's chunks and rest with q's filters.
type contentLister struct {
	s *Service
	q *sel
}

func (l contentLister) Admit(ctx context.Context, ids []string) ([]string, error) {
	args := pgx.NamedArgs{"p_ids": ids}
	for k, v := range l.q.args {
		args[k] = v
	}
	rows, err := l.s.o.Pool.Query(ctx, fmt.Sprintf(`SELECT DISTINCT f.content_id FROM %s f WHERE %s AND f.content_id = ANY(@p_ids::text[])`,
		l.s.t.facets, l.q.predicate()), args)
	if err != nil {
		return nil, err
	}
	found, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	kept := make(map[string]bool, len(found))
	for _, id := range found {
		kept[id] = true
	}
	return slices.DeleteFunc(slices.Clone(ids), func(id string) bool { return !kept[id] }), nil
}

// Rest lists newest first, the order of contents without activity.
func (l contentLister) Rest(ctx context.Context, after string, n int) ([]rankings.Row, error) {
	if !popularListsEverything {
		return nil, nil
	}
	const newest = "coalesce(f.published_at, 'epoch'::timestamptz)"
	args := pgx.NamedArgs{"p_limit": n}
	for k, v := range l.q.args {
		args[k] = v
	}
	keyset := ""
	if after != "" {
		at, id, ok := strings.Cut(after, "|")
		t, err := time.Parse(time.RFC3339Nano, at)
		if !ok || err != nil {
			return nil, fmt.Errorf("%w: cursor", ErrInvalid)
		}
		keyset, args["p_at"], args["p_id"] = " AND ("+newest+", f.content_id) < (@p_at::timestamptz, @p_id::text)", t, id
	}
	rows, err := l.s.o.Pool.Query(ctx, fmt.Sprintf(`SELECT f.content_id, %[1]s FROM %[2]s f WHERE %[3]s%[4]s
 GROUP BY %[1]s, f.content_id ORDER BY %[1]s DESC, f.content_id DESC LIMIT @p_limit`, newest, l.s.t.facets, l.q.predicate(), keyset), args)
	if err != nil {
		return nil, err
	}
	var out []rankings.Row
	var id string
	var at time.Time
	_, err = pgx.ForEachRow(rows, []any{&id, &at}, func() error {
		out = append(out, rankings.Row{ID: id, Key: at.UTC().Format(time.RFC3339Nano) + "|" + id})
		return nil
	})
	return out, err
}

// popularScores scores ids by their place in kind's featured ranking on the
// reader's site, higher first; ids without positive popularity are absent.
func (s *Service) popularScores(ctx context.Context, r discovery.Reader, kind string, window string, ids []string) (map[string]float64, error) {
	w, err := rankings.ParseWindow(window, s.o.Rankings.Now())
	if err != nil {
		return nil, err
	}
	rk, err := s.o.Rankings.Ranking(ctx, s.site(ctx, r, ""), w, rankings.List{Kind: kind}, "")
	if err != nil {
		return nil, err
	}
	out := map[string]float64{}
	for _, id := range ids {
		if p, ok := rk.Position(id); ok && rk.Stats[p].Score > 0 {
			out[id] = 1 / float64(p+1)
		}
	}
	return out, nil
}

// topPopular is up to limit ids of kind's featured ranking with positive
// popularity, best first.
func (s *Service) topPopular(ctx context.Context, r discovery.Reader, kind, window string, limit int) ([]string, error) {
	w, err := rankings.ParseWindow(window, s.o.Rankings.Now())
	if err != nil {
		return nil, err
	}
	rk, err := s.o.Rankings.Ranking(ctx, s.site(ctx, r, ""), w, rankings.List{Kind: kind}, "")
	if err != nil {
		return nil, err
	}
	var out []string
	for i, id := range rk.IDs {
		if len(out) == limit || rk.Stats[i].Score <= 0 {
			break
		}
		out = append(out, id)
	}
	return out, nil
}
