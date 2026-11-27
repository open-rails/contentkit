package rankings

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/open-rails/contentkit/internal/outcomevalue"
	"github.com/open-rails/contentkit/signal"
)

// strong is the |q| from which a person-day is a strong positive or negative:
// a bounce, a dislike, a like, a favorite.
const strong = 0.5

// polarizationPrior shrinks a thinly read entity's polarization toward 0.
const polarizationPrior = 20

type outcomeValues struct {
	table   outcomevalue.Table
	version string
}

// loadValues reads outcome_values; its version changes with any value, so
// a refit makes new closed-range keys.
func (e *Engine) loadValues(ctx context.Context) (outcomeValues, error) {
	t, err := outcomevalue.Load(ctx, e.o.Pool, e.o.Schema, e.o.Tenant)
	if err != nil {
		return outcomeValues{}, err
	}
	h := sha256.New()
	for _, segment := range slices.Sorted(maps.Keys(t)) {
		for _, outcome := range slices.Sorted(maps.Keys(t[segment])) {
			fmt.Fprintf(h, "%s|%s|%.12g\n", segment, outcome, t[segment][outcome])
		}
	}
	return outcomeValues{table: t, version: hex.EncodeToString(h.Sum(nil))[:12]}, nil
}

// compute ranks every list of site over windows ending together, in one
// ClickHouse query; a list per window holds what had activity in it.
func (e *Engine) compute(ctx context.Context, site Site, windows []Window, values outcomeValues, at time.Time) ([]map[List]*Ranking, error) {
	cat, err := e.loadCatalog(ctx)
	if err != nil {
		return nil, err
	}
	quality := map[string]map[string]float64{}
	for name, kind := range e.o.Kinds.Kinds {
		quality[name] = values.table.Segment(outcomevalue.SegmentOf(kind.Presentation))
	}
	q := signal.PopularityQuery{Domain: site.Domain, Language: site.Language, Until: windows[0].Until, Quality: quality, Strong: strong}
	first := q.Until
	for _, w := range windows {
		if !w.Until.Equal(q.Until) {
			return nil, fmt.Errorf("rankings: windows of one query end together")
		}
		q.Windows = append(q.Windows, w.From)
		first = earlier(first, w.From)
	}
	if q.Gated, err = e.gatedDays(ctx, site.Language, first, q.Until); err != nil {
		return nil, err
	}
	rows, err := e.o.Source.Popularity(ctx, q)
	if err != nil {
		return nil, err
	}
	out := make([]map[List]*Ranking, len(windows))
	for i, w := range windows {
		out[i] = e.lists(cat, rows, i, w, at)
	}
	return out, nil
}

type entry struct {
	id                 string
	stat               Stat
	published          time.Time
	positive, negative uint64
	members            int
}

// lists ranks window i's contents of every kind, their audiences' and every
// entity kind.
func (e *Engine) lists(cat *catalog, rows []signal.ContentPopularity, i int, w Window, at time.Time) map[List]*Ranking {
	contents := map[string][]entry{}
	audiences := map[string][]entry{}
	type sums struct {
		score              float64
		opens              uint64
		positive, negative uint64
		members            int
	}
	entities := map[string]*sums{}
	for _, row := range rows {
		c, ok := cat.contents[row.ContentID]
		if !ok || c.kind != row.ContentKind || c.opens.After(at) {
			continue
		}
		score, audience := row.Score[i], row.Audience[i]
		if row.Opens[i] == 0 && score == 0 && audience == 0 && row.Positive[i] == 0 && row.Negative[i] == 0 {
			continue
		}
		if w.Trending {
			days := liveDays(c.liveFrom, w.From, at)
			score, audience = score/days, audience/days
		}
		contents[c.kind] = append(contents[c.kind], entry{id: row.ContentID, published: c.published, stat: Stat{Score: score, Opens: clampUint32(row.Opens[i])}})
		audiences[c.kind] = append(audiences[c.kind], entry{id: row.ContentID, published: c.published, stat: Stat{Score: audience, Opens: clampUint32(row.Opens[i])}})
		for _, id := range c.entities {
			s := entities[id]
			if s == nil {
				s = &sums{}
				entities[id] = s
			}
			s.score += score
			s.opens += row.Opens[i]
			s.positive += row.Positive[i]
			s.negative += row.Negative[i]
			s.members++
		}
	}
	ranking := func(entries []entry) *Ranking {
		slices.SortFunc(entries, func(a, b entry) int {
			return cmp.Or(cmp.Compare(b.stat.Score, a.stat.Score), cmp.Compare(b.stat.Opens, a.stat.Opens),
				cmp.Compare(b.members, a.members), b.published.Compare(a.published), strings.Compare(a.id, b.id))
		})
		r := &Ranking{From: w.From, Until: w.Until, Computed: at, Ready: true, IDs: make([]string, len(entries)), Stats: make([]Stat, len(entries)), positions: &positions{}}
		for j, x := range entries {
			r.IDs[j], r.Stats[j] = x.id, x.stat
		}
		return r
	}
	out := map[List]*Ranking{}
	for kind := range e.o.Kinds.Kinds {
		out[List{Kind: kind}] = ranking(contents[kind])
		out[List{Kind: kind, Audience: true}] = ranking(audiences[kind])
	}
	byKind := map[string][]entry{}
	for id, s := range entities {
		kind, ok := cat.entityKind[id]
		if !ok {
			continue
		}
		polarization := 2 * float64(min(s.positive, s.negative)) / float64(s.positive+s.negative+polarizationPrior)
		byKind[kind] = append(byKind[kind], entry{id: id, members: s.members,
			stat: Stat{Score: s.score, Opens: clampUint32(s.opens), Polarization: float32(polarization)}})
	}
	for kind := range e.o.Kinds.EntityKinds {
		out[List{Kind: kind, Entities: true}] = ranking(byKind[kind])
	}
	return out
}

// liveDays are the days of the window from its start a content was live by
// at: whole past days, today as the part elapsed (at least an hour).
func liveDays(liveFrom, from, at time.Time) float64 {
	today := day(at)
	start := later(day(liveFrom), from)
	days := 0.0
	if start.Before(today) {
		days = today.Sub(start).Hours() / 24
	}
	return days + max(at.Sub(today).Hours()/24, 1.0/24)
}

func clampUint32(n uint64) uint32 { return uint32(min(n, uint64(^uint32(0)))) }
