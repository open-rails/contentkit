package rankings

import (
	"cmp"
	"context"
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/signal"
)

// accessState is one content_access_history snapshot.
type accessState struct {
	LiveFrom *time.Time `json:"live_from"`
	Deleted  bool       `json:"deleted"`
	// Listing is set only on snapshots from before listing policies were
	// removed.
	Listing   string           `json:"listing"`
	Languages []string         `json:"languages"`
	Level     access.Level     `json:"level"`
	Tiers     []string         `json:"tiers"`
	Entity    string           `json:"group_id"`
	Releases  []access.Release `json:"releases"`
}

// audience is who could open a content over a span: everyone, the holders
// of tiers, or someone the rankings cannot count (a purchase or a channel's
// members), whose days are not scaled.
type audience struct {
	from, until time.Time
	public      bool
	tiers       []string
}

// gatedDays are the days within [from, until) on which a content of the
// site's language was open only to tiers, read from the catalog's access
// history: a day takes the access in effect at its midpoint. A content's
// editions together are public when any is, otherwise open to the union of
// their tiers; an edition whose audience is not a tier leaves the span
// unscaled.
func (e *Engine) gatedDays(ctx context.Context, language string, from, until time.Time) ([]signal.GatedDays, error) {
	history := e.table("content_access_history")
	rows, err := e.o.Pool.Query(ctx, `WITH gated AS (
 SELECT DISTINCT content_id FROM `+history+` WHERE tenant_id = $1
 AND (snapshot->>'level' <> 'public' OR jsonb_array_length(coalesce(snapshot->'releases', '[]'::jsonb)) > 0)
), versions AS (
 SELECT DISTINCT h.content_id, h.version_id FROM `+history+` h JOIN gated USING (content_id) WHERE h.tenant_id = $1
), prior AS (
 SELECT DISTINCT ON (h.version_id) h.id, h.content_id, h.version_id, h.occurred_at, h.snapshot
 FROM `+history+` h JOIN versions USING (content_id, version_id)
 WHERE h.tenant_id = $1 AND h.occurred_at <= $2 ORDER BY h.version_id, h.occurred_at DESC, h.id DESC
), recent AS (
 SELECT h.id, h.content_id, h.version_id, h.occurred_at, h.snapshot
 FROM `+history+` h JOIN versions USING (content_id, version_id)
 WHERE h.tenant_id = $1 AND h.occurred_at > $2 AND h.occurred_at < $3
) SELECT content_id, version_id, occurred_at, snapshot FROM (SELECT * FROM prior UNION ALL SELECT * FROM recent) s
 ORDER BY content_id, version_id, occurred_at, id`, e.o.Tenant, from, until)
	if err != nil {
		return nil, err
	}
	type row struct {
		content, version string
		at               time.Time
		state            accessState
	}
	var states []row
	var r row
	var raw []byte
	_, err = pgx.ForEachRow(rows, []any{&r.content, &r.version, &r.at, &raw}, func() error {
		r.state = accessState{}
		if err := json.Unmarshal(raw, &r.state); err != nil {
			return err
		}
		states = append(states, r)
		return nil
	})
	if err != nil {
		return nil, err
	}
	var out []signal.GatedDays
	for first := 0; first < len(states); {
		last := first
		for last < len(states) && states[last].content == states[first].content {
			last++
		}
		var spans []audience
		for i := first; i < last; i++ {
			s := states[i]
			end := until
			if i+1 < last && states[i+1].version == s.version {
				end = states[i+1].at
			}
			begin := later(s.at, from)
			st := s.state
			if st.Deleted || st.LiveFrom == nil || st.Listing != "" && st.Listing != "listed" ||
				language != "" && !slices.Contains(st.Languages, language) {
				continue
			}
			begin = later(begin, *st.LiveFrom)
			if !begin.Before(end) {
				continue
			}
			rule := access.Rule{Level: st.Level, Tiers: st.Tiers, ChannelID: st.Entity, Releases: st.Releases}
			cuts := []time.Time{begin, end}
			for _, release := range rule.Releases {
				if release.At.After(begin) && release.At.Before(end) {
					cuts = append(cuts, release.At)
				}
			}
			slices.SortFunc(cuts, func(a, b time.Time) int { return a.Compare(b) })
			for j := 0; j+1 < len(cuts); j++ {
				if !cuts[j].Before(cuts[j+1]) {
					continue
				}
				a := audience{from: cuts[j], until: cuts[j+1], public: rule.Free(cuts[j])}
				if !a.public {
					a.tiers = rule.AudienceTiers(cuts[j])
				}
				spans = append(spans, a)
			}
		}
		out = append(out, gatedSpans(states[first].content, spans)...)
		first = last
	}
	return out, nil
}

// gatedSpans unions a content's editions and turns the spans open only to
// tiers into days.
func gatedSpans(content string, spans []audience) []signal.GatedDays {
	type change struct {
		at    time.Time
		span  audience
		delta int
	}
	changes := make([]change, 0, 2*len(spans))
	for _, s := range spans {
		changes = append(changes, change{s.from, s, 1}, change{s.until, s, -1})
	}
	slices.SortFunc(changes, func(a, b change) int { return a.at.Compare(b.at) })
	var out []signal.GatedDays
	public, unknown := 0, 0
	tiers := map[string]int{}
	var previous time.Time
	for i := 0; i < len(changes); {
		at := changes[i].at
		if !previous.IsZero() && previous.Before(at) && public == 0 && unknown == 0 && len(tiers) > 0 {
			held := slices.Sorted(maps.Keys(tiers))
			// A day takes the access at its midpoint.
			first, stop := ceilDay(previous.Add(-12*time.Hour)), ceilDay(at.Add(-12*time.Hour))
			if first.Before(stop) {
				n := len(out)
				if n > 0 && out[n-1].Until.Equal(first) && slices.Equal(out[n-1].Tiers, held) {
					out[n-1].Until = stop
				} else {
					out = append(out, signal.GatedDays{ContentID: content, From: first, Until: stop, Tiers: held})
				}
			}
		}
		for i < len(changes) && changes[i].at.Equal(at) {
			c := changes[i]
			switch {
			case c.span.public:
				public += c.delta
			case len(c.span.tiers) == 0:
				unknown += c.delta
			default:
				for _, t := range c.span.tiers {
					if tiers[t] += c.delta; tiers[t] == 0 {
						delete(tiers, t)
					}
				}
			}
			i++
		}
		previous = at
	}
	slices.SortFunc(out, func(a, b signal.GatedDays) int {
		return cmp.Or(strings.Compare(a.ContentID, b.ContentID), a.From.Compare(b.From))
	})
	return out
}

// ceilDay is the first UTC midnight at or after t.
func ceilDay(t time.Time) time.Time {
	if d := day(t); d.Equal(t.UTC()) {
		return d
	}
	return day(t).AddDate(0, 0, 1)
}
