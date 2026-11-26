package signalstore

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/ext"
	"github.com/open-rails/contentkit/signal"
)

// popularityTimeout bounds one popularity sum; it fills a cache or serves a
// report, never a featured page.
const popularityTimeout = 2 * time.Minute

// Popularity sums content_daily over a site in one query, every window at
// once: each content's quality-weighted person-days, the days it was open
// only to tiers scaled by audience_daily to the site's active people. Each
// day is read at its newest generation.
func (st *Store) Popularity(ctx context.Context, tenant string, q signal.PopularityQuery) ([]signal.ContentPopularity, error) {
	if strings.TrimSpace(tenant) == "" || len(q.Windows) == 0 {
		return nil, fmt.Errorf("signal: popularity needs a tenant and a window")
	}
	first := q.Until
	for _, from := range q.Windows {
		if !from.Before(q.Until) {
			return nil, fmt.Errorf("signal: popularity windows must start before they end")
		}
		if from.Before(first) {
			first = from
		}
	}
	contentKinds := slices.Sorted(maps.Keys(q.Quality))
	if len(contentKinds) == 0 {
		return []signal.ContentPopularity{}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, popularityTimeout)
	defer cancel()
	day := func(t time.Time) string { return t.UTC().Format(time.DateOnly) }
	from, until := day(first), day(q.Until)
	var keys []string
	var values []float64
	for _, kind := range contentKinds {
		for _, outcome := range slices.Sorted(maps.Keys(q.Quality[kind])) {
			keys, values = append(keys, kind+"|"+outcome), append(values, q.Quality[kind][outcome])
		}
	}
	site := "tenant = ? AND domain = ? AND (? = '' OR language = ?) AND day >= toDate(?) AND day < toDate(?)"
	siteArgs := []any{tenant, q.Domain, q.Language, q.Language, from, until}
	newest := func(table string) string {
		return fmt.Sprintf("(day, generation) IN (SELECT day, max(generation) FROM %s.%s WHERE tenant = ? AND day >= toDate(?) AND day < toDate(?) GROUP BY day)", st.db, table)
	}
	args := append([]any{keys, values, keys, values, keys, values}, siteArgs...)
	args = append(args, contentKinds, tenant, from, until)
	ctes := []string{fmt.Sprintf(`cd AS (SELECT content_kind, content_id, day, audience, class, people,
 transform(concat(content_kind, '|', reaction, ':', reason), ?, ?,
  transform(concat(content_kind, '|', reaction), ?, ?,
   transform(concat(content_kind, '|', class), ?, ?, 0.))) AS q
 FROM %s.content_daily WHERE %s AND content_kind IN ? AND %s)`, st.db, site, newest("content_daily"))}
	// in(w) keeps window w's days; the windows' bounds are literals of this
	// query's own making.
	in := func(w int, column string) string {
		return fmt.Sprintf("%s >= toDate('%s')", column, day(q.Windows[w]))
	}
	columns := func(f func(w int) string) string {
		parts := make([]string, len(q.Windows))
		for w := range q.Windows {
			parts[w] = f(w)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	}
	boosts, boostJoin := "", ""
	if len(q.Gated) > 0 {
		table, err := ext.NewTable("contentkit_gated_days",
			ext.Column("content_id", "String"), ext.Column("start", "Date"), ext.Column("stop", "Date"), ext.Column("tiers", "String"))
		if err != nil {
			return nil, fmt.Errorf("signal: gated days: %w", err)
		}
		for _, g := range q.Gated {
			tiers := slices.Clone(g.Tiers)
			slices.Sort(tiers)
			if err := table.Append(g.ContentID, g.From.UTC(), g.Until.UTC(), strings.Join(tiers, ",")); err != nil {
				return nil, fmt.Errorf("signal: gated days: %w", err)
			}
		}
		ctx = clickhouse.Context(ctx, clickhouse.WithExternalTable(table))
		ctes = append(ctes,
			`gated AS (SELECT toUUID(content_id) AS content_id, start, stop, tiers FROM contentkit_gated_days)`,
			fmt.Sprintf(`aud AS (SELECT day, access, sum(people) AS people FROM %s.audience_daily WHERE %s AND %s GROUP BY day, access)`, st.db, site, newest("audience_daily")),
			`totals AS (SELECT day, sum(people) AS total FROM aud GROUP BY day)`,
			`eligible AS (SELECT t.tiers AS tiers, a.day AS day, sum(a.people) AS people FROM (SELECT DISTINCT tiers FROM gated) t CROSS JOIN aud a
 WHERE hasAny(splitByChar(',', a.access), splitByChar(',', t.tiers)) GROUP BY t.tiers, a.day)`,
			fmt.Sprintf(`boosts AS (SELECT content_kind, content_id, %s AS boost, %s AS audience_boost FROM (
 SELECT d.content_kind AS content_kind, d.content_id AS content_id, d.day AS day, d.s * (t.total / e.people - 1) AS extra, d.sa * (t.total / e.people - 1) AS audience_extra
 FROM (SELECT content_kind, content_id, day, sum(people * q) AS s, sumIf(people * q, audience) AS sa FROM cd WHERE content_id IN (SELECT content_id FROM gated) GROUP BY content_kind, content_id, day) d
 INNER JOIN gated g ON g.content_id = d.content_id
 INNER JOIN totals t ON t.day = d.day
 INNER JOIN eligible e ON e.tiers = g.tiers AND e.day = d.day
 WHERE d.day >= g.start AND d.day < g.stop AND e.people > 0)
 GROUP BY content_kind, content_id)`,
				columns(func(w int) string { return "sumIf(extra, " + in(w, "day") + ")" }),
				columns(func(w int) string { return "sumIf(audience_extra, " + in(w, "day") + ")" })))
		args = append(args, siteArgs...)
		args = append(args, tenant, from, until)
		boosts = ", b.boost, b.audience_boost"
		boostJoin = " LEFT JOIN boosts b ON b.content_kind = s.content_kind AND b.content_id = s.content_id"
	}
	strong := fmt.Sprintf("%g", q.Strong)
	sql := fmt.Sprintf(`WITH %s
SELECT s.content_kind, toString(s.content_id), s.score, s.audience, s.opens, s.positive, s.negative%s FROM (
 SELECT content_kind, content_id,
  %s AS score, %s AS audience, %s AS opens, %s AS positive, %s AS negative
 FROM cd GROUP BY content_kind, content_id) s%s
SETTINGS max_execution_time = 120`, strings.Join(ctes, ",\n"), boosts,
		columns(func(w int) string { return "sumIf(people * q, " + in(w, "day") + ")" }),
		columns(func(w int) string { return "sumIf(people * q, audience AND " + in(w, "day") + ")" }),
		columns(func(w int) string { return "sumIf(people, class != '' AND " + in(w, "day") + ")" }),
		columns(func(w int) string { return "sumIf(people, q >= " + strong + " AND " + in(w, "day") + ")" }),
		columns(func(w int) string { return "sumIf(people, q <= -" + strong + " AND " + in(w, "day") + ")" }),
		boostJoin)
	rows, err := st.conn.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("signal: popularity: %w", err)
	}
	defer rows.Close()
	out := []signal.ContentPopularity{}
	for rows.Next() {
		var p signal.ContentPopularity
		dest := []any{&p.ContentKind, &p.ContentID, &p.Score, &p.Audience, &p.Opens, &p.Positive, &p.Negative}
		var boost, audienceBoost []float64
		if boosts != "" {
			dest = append(dest, &boost, &audienceBoost)
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, fmt.Errorf("signal: popularity scan: %w", err)
		}
		for w := range boost {
			p.Score[w] += boost[w]
			p.Audience[w] += audienceBoost[w]
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
