package discovery

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/popularity"
)

// ListOptions select one page of a list.
type ListOptions struct {
	Reader
	Filter
	// Sort is newest, oldest, title, popular[:period], top_rated[:period],
	// most_favorited, position or random; the kind's configured
	// sort, else newest, by default. Periods are popularity.Periods.
	Sort string
	// ActivityFrom and ActivityUntil choose a custom popularity window of whole
	// UTC dates [from, until). Both require SortPopular and an explicit Kind.
	// They select activity dates, independently of publication-date filters.
	ActivityFrom  string
	ActivityUntil string
	// CardGroups are the group kinds cards name; every kind with a page by
	// default.
	CardGroups []string
	Cursor     string
	// Page numbers pages from 1 instead of Cursor.
	Page int
	// Limit defaults to 24 and is capped at 100.
	Limit int
	// Summary adds selection-wide counts on the first page, or on every
	// numbered page.
	Summary bool
}

func pageLimit(n, def int) int {
	if n <= 0 {
		return def
	}
	return min(n, 100)
}

// maxPageOffset bounds numbered pages; deeper reading pages by cursor.
const maxPageOffset = 10_000

// pageOffset is a numbered page's offset; zero without one.
func pageOffset(page, limit int, cursor, sort string) (int, error) {
	switch {
	case page == 0:
		return 0, nil
	case page < 0:
		return 0, fmt.Errorf("%w: page", ErrInvalid)
	case cursor != "":
		return 0, fmt.Errorf("%w: page and cursor are exclusive", ErrInvalid)
	case sort == SortRandom:
		return 0, fmt.Errorf("%w: random lists page by cursor", ErrInvalid)
	case page*limit > maxPageOffset:
		return 0, fmt.Errorf("%w: page × limit is at most %d", ErrInvalid, maxPageOffset)
	}
	return (page - 1) * limit, nil
}

// List pages cards: one per content item the reader's listing shows that
// matches the filter, in the sort's order.
func (s *Service) List(ctx context.Context, o ListOptions) (ContentPage, error) {
	ctx = access.WithMemo(ctx)
	window, custom, err := activityWindow(o)
	if err != nil {
		return ContentPage{Contents: []Card{}}, err
	}
	sort := s.sortName(o.Sort, o.Kind)
	if custom {
		sort = historicalSort(o.Kind, window)
		if _, err := decodeCursor(o.Cursor, sort); err != nil {
			return ContentPage{Contents: []Card{}}, err
		}
	}
	o.Filter, err = publicationCursor(o.Filter, o.Cursor, sort)
	if err != nil {
		return ContentPage{Contents: []Card{}}, err
	}
	q, err := s.compile(ctx, o.Reader, o.Filter)
	if err != nil {
		return ContentPage{Contents: []Card{}}, err
	}
	var srt order
	if custom {
		srt, err = s.historicalOrder(ctx, o.Kind, window, o.Reader)
	} else if name, period, _ := strings.Cut(sort, ":"); name == SortPopular && o.Kind != "" && s.o.Hub.SignalsConfigured() {
		if period == "" {
			period = popularity.DefaultPeriod
		}
		window, err = popularity.WindowForPeriod(period, s.now())
		if err != nil {
			err = fmt.Errorf("%w: %w", ErrInvalid, err)
		} else {
			srt, err = s.historicalOrder(ctx, o.Kind, window, o.Reader)
			srt.raw = sort
		}
	} else {
		srt, err = s.parseSort(ctx, o.Sort, o.Filter)
	}
	if err != nil {
		return ContentPage{Contents: []Card{}}, err
	}
	limit := pageLimit(o.Limit, 24)
	offset, err := pageOffset(o.Page, limit, o.Cursor, sort)
	if err != nil {
		return ContentPage{Contents: []Card{}}, err
	}
	picks, next, err := s.page(ctx, q, srt, o.Cursor, limit, offset)
	if err != nil {
		return ContentPage{Contents: []Card{}}, err
	}
	cards, err := s.cards(ctx, q, picks, cardOptions{chips: o.CardGroups})
	for i := range cards {
		if ranking, ok := srt.rankings[cards[i].ID]; ok {
			cards[i].Ranking = &ranking
		}
	}
	out := ContentPage{Contents: cards, NextCursor: next, Ranking: srt.ranking}
	if err == nil && o.Summary && o.Cursor == "" {
		out.Summary, err = s.listingSummary(ctx, o.Filter, q, srt, nil)
	}
	return out, err
}

// ReleaseDatesOptions select the release date index.
type ReleaseDatesOptions struct {
	Reader
	Filter
	// By is month (the default) or day.
	By string
	// Cursor is the previous page's next_cursor: dates before it follow.
	Cursor string
	// Limit defaults to 60 and is capped at 1000.
	Limit int
}

// DateCount is one day or month of the index.
type DateCount struct {
	// Date is YYYY-MM-DD or YYYY-MM in the filter's time zone.
	Date string `json:"date"`
	// Count is the contents released then; Free those open to anyone.
	Count int `json:"count"`
	Free  int `json:"free"`
}

// ReleaseDates is one page of the index, newest first.
type ReleaseDates struct {
	Dates      []DateCount `json:"dates"`
	NextCursor string      `json:"next_cursor,omitempty"`
}

// ReleaseDates counts the contents the reader's listing shows by the day or
// month they were published, newest first.
func (s *Service) ReleaseDates(ctx context.Context, o ReleaseDatesOptions) (ReleaseDates, error) {
	out := ReleaseDates{Dates: []DateCount{}}
	unit, layout, format := "month", "2006-01", "YYYY-MM"
	switch o.By {
	case "", "month":
	case "day":
		unit, layout, format = "day", time.DateOnly, "YYYY-MM-DD"
	default:
		return out, fmt.Errorf("%w: by %q is day or month", ErrInvalid, o.By)
	}
	q, err := s.compile(ctx, o.Reader, o.Filter)
	if err != nil {
		return out, err
	}
	limit := o.Limit
	if limit <= 0 {
		limit = 60
	}
	limit = min(limit, 1000)
	args := pgx.NamedArgs{"d_unit": unit, "d_format": format, "d_tz": q.loc.String(), "d_limit": limit + 1}
	for k, v := range q.args {
		args[k] = v
	}
	before := "true"
	if o.Cursor != "" {
		if _, err := time.Parse(layout, o.Cursor); err != nil {
			return out, fmt.Errorf("%w: cursor", ErrInvalid)
		}
		before, args["d_before"] = "d.date < @d_before::text", o.Cursor
	}
	rows, err := s.o.Pool.Query(ctx, fmt.Sprintf(`SELECT d.date, count(*)::int, count(*) FILTER (WHERE d.free)::int FROM (
 SELECT to_char(date_trunc(@d_unit::text, max(f.published_at) AT TIME ZONE @d_tz::text), @d_format::text) AS date,
  bool_or(f.access_level = 'public' OR f.access_free_after <= now()) AS free
 FROM %s f WHERE %s AND f.published_at IS NOT NULL GROUP BY f.content_id) d
WHERE %s GROUP BY d.date ORDER BY d.date DESC LIMIT @d_limit`, s.t.facets, q.predicate(), before), args)
	if err != nil {
		return out, err
	}
	dates, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (DateCount, error) {
		var d DateCount
		return d, r.Scan(&d.Date, &d.Count, &d.Free)
	})
	if err != nil {
		return out, err
	}
	if len(dates) > limit {
		dates = dates[:limit]
		out.NextCursor = dates[limit-1].Date
	}
	out.Dates = append(out.Dates, dates...)
	return out, nil
}
