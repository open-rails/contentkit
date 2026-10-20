package discovery

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/groups"
)

// DailyReleasesOptions selects a complete calendar-day window of releases.
type DailyReleasesOptions struct {
	Reader
	Filter
	// Days includes EndDate, or today; 10 by default, from 1 through 31.
	Days int
	// EndDate is an inclusive YYYY-MM-DD in TimeZone; today by default.
	// Future dates are rejected. Visibility is evaluated at the current time.
	EndDate string
	// TimeZone is an IANA zone, UTC by default.
	TimeZone   string
	CardGroups []string
}

type ReleaseDay struct {
	Date     string `json:"date"`
	Contents []Card `json:"contents"`
	// FilteredCount is the unique contents omitted only by optional entity
	// preferences, not by access or hard domain policy.
	FilteredCount int `json:"filtered_count"`
	// Counts overlap, so their sum is not FilteredCount. Hidden public filters
	// with no matches have count zero. Locked entity redaction still applies.
	FilteredEntities []ReleaseFilteredEntity `json:"filtered_entities"`
}

type ReleaseFilteredEntity struct {
	Entity groups.GroupCard `json:"entity"`
	Count  int              `json:"count"`
}

// DailyReleases contains every visible card in the requested window, without
// a per-day or whole-response item limit. Empty days preserve its extent.
type DailyReleases struct {
	TimeZone    string       `json:"time_zone"`
	FromDate    string       `json:"from_date"`
	ThroughDate string       `json:"through_date"`
	Days        []ReleaseDay `json:"days"`
}

func releaseWindow(now time.Time, days int, zone, endDate string) (time.Time, time.Time, *time.Location, error) {
	if days == 0 {
		days = 10
	}
	if days < 1 || days > 31 {
		return time.Time{}, time.Time{}, nil, fmt.Errorf("%w: days must be from 1 through 31", ErrInvalid)
	}
	if zone == "" {
		zone = "UTC"
	}
	if zone == "Local" {
		return time.Time{}, time.Time{}, nil, fmt.Errorf("%w: time_zone must name an IANA time zone", ErrInvalid)
	}
	loc, err := time.LoadLocation(zone)
	if err != nil {
		return time.Time{}, time.Time{}, nil, fmt.Errorf("%w: invalid time_zone %q", ErrInvalid, zone)
	}
	local := now.In(loc)
	today := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
	through := today
	if endDate != "" {
		through, err = time.ParseInLocation(time.DateOnly, endDate, loc)
		if err != nil || through.Year() < 1 || through.After(today) {
			return time.Time{}, time.Time{}, nil, fmt.Errorf("%w: end_date must be a valid YYYY-MM-DD on or before today in time_zone", ErrInvalid)
		}
	}
	from := through.AddDate(0, 0, 1-days)
	if from.Year() < 1 {
		from = time.Date(1, 1, 1, 0, 0, 0, 0, loc)
	}
	return from, through, loc, nil
}

// Releases reads one complete window so clients can reveal more cards without
// additional API calls. It shares listing visibility, preferences and access.
func (s *Service) Releases(ctx context.Context, o DailyReleasesOptions) (DailyReleases, error) {
	ctx = access.WithMemo(ctx)
	out := DailyReleases{Days: []ReleaseDay{}}
	now := s.now()
	from, through, loc, err := releaseWindow(now, o.Days, o.TimeZone, o.EndDate)
	if err != nil {
		return out, err
	}
	out.TimeZone, out.FromDate, out.ThroughDate = loc.String(), from.Format(time.DateOnly), through.Format(time.DateOnly)
	for day := through; !day.Before(from); day = day.AddDate(0, 0, -1) {
		out.Days = append(out.Days, ReleaseDay{Date: day.Format(time.DateOnly), Contents: []Card{}, FilteredEntities: []ReleaseFilteredEntity{}})
	}
	filter := o.Filter
	filter.ReleasedOn = ""
	filter.TimeZone = loc.String()
	if filter.PublishedWithin != "" {
		filter.publishedAsOf = now
	}
	q, err := s.compile(ctx, o.Reader, filter)
	if err != nil {
		return out, err
	}
	until := through.AddDate(0, 0, 1)
	got, err := s.releasePicks(ctx, q, from, until, now)
	if err != nil {
		return out, err
	}
	indices := make(map[string]int, len(out.Days))
	for i, day := range out.Days {
		indices[day.Date] = i
	}
	// Bound each hydration query, never the result. Every selected item is
	// returned, while a busy window does not become one oversized SQL array.
	const batchSize = 256
	visible := make([]string, 0, len(got))
	for offset := 0; offset < len(got); offset += batchSize {
		batch := got[offset:min(offset+batchSize, len(got))]
		picks := make([]pick, len(batch))
		dates := make(map[string]string, len(batch))
		for i, r := range batch {
			picks[i] = r.pick
			dates[r.content] = r.at.In(loc).Format(time.DateOnly)
		}
		cards, err := s.cards(ctx, q, picks, cardOptions{chips: o.CardGroups})
		if err != nil {
			return out, err
		}
		for _, card := range cards {
			visible = append(visible, card.ID)
			if i, ok := indices[dates[card.ID]]; ok {
				out.Days[i].Contents = append(out.Days[i].Contents, card)
			}
		}
	}
	if err := s.filteredReleases(ctx, &out, filter, q, visible, from, until, now); err != nil {
		return out, err
	}
	return out, nil
}

type releasePick struct {
	pick
	at time.Time
}

// releasePicks applies feed-only publication and language constraints even to
// privileged readers, then selects the same preferred edition as card lists.
func (s *Service) releasePicks(ctx context.Context, q *sel, from, until, now time.Time) ([]releasePick, error) {
	q = q.with("f.published_at >= @r_from AND f.published_at < @r_until AND f.published_at <= @r_now AND f.live_from <= @r_now AND NOT f.deleted AND NOT f.unplayable AND f.listing <> 'unlisted' AND (@r_language = '' OR @r_language = ANY(f.languages))",
		pgx.NamedArgs{"r_from": from, "r_until": until, "r_now": now, "r_language": q.reader.Language})
	rows, err := s.o.Pool.Query(ctx, fmt.Sprintf(`SELECT p.content_id, p.version_id, p.published_at FROM (
 SELECT DISTINCT ON (f.content_id) f.content_id, f.version_id, f.published_at
 FROM %s f WHERE %s ORDER BY f.content_id, %s) p
 ORDER BY p.published_at DESC, p.content_id DESC`, s.t.facets, q.predicate(), preference), q.args)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (releasePick, error) {
		var r releasePick
		return r, row.Scan(&r.content, &r.version, &r.at)
	})
}
