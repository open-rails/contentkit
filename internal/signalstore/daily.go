package signalstore

import (
	"context"
	"fmt"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/google/uuid"

	"github.com/open-rails/contentkit/internal/outcomevalue"
)

// RebuildDay rebuilds a tenant-day's content_daily and audience_daily whole,
// every site and kind in one generation, then deletes the older ones.
// Readers take each day's newest generation, so repeating a rebuild changes
// nothing. links fold anonymous subjects active that day into their
// accounts.
func (st *Store) RebuildDay(ctx context.Context, tenant string, t Thresholds, day time.Time, links []Link, audience Audience, generation time.Time) error {
	from := utcDay(day)
	to := from.AddDate(0, 0, 1)
	gen := generation.UTC().Format(tsLayout)
	ctx, err := withLinks(ctx, links, nil, audience)
	if err != nil {
		return err
	}
	content := `INSERT INTO ` + st.db + `.content_daily (tenant, domain, language, day, content_kind, content_id, audience, class, reaction, reason, people, generation)
SELECT ?, domain, language, day, content_kind, content_id,
 NOT (reaction = 'dislike' AND reason = '` + outcomevalue.NotInto + `')
  AND (content_id NOT IN (SELECT content_id FROM contentkit_polarizing) OR (person, content_id) IN (SELECT person, content_id FROM contentkit_audience)) AS audience,
 class, reaction, reason, toUInt32(count()), ?
FROM (` + st.outcomeSQL(true, "a.ts >= ? AND a.ts < ?") + `)
WHERE class != '' OR reaction != ''
GROUP BY domain, language, day, content_kind, content_id, audience, class, reaction, reason`
	args := append([]any{tenant, gen}, outcomeArgs(t, tenant)...)
	if err := st.conn.Exec(ctx, content, append(args, from, to)...); err != nil {
		return fmt.Errorf("signal: rebuild content_daily: %w", err)
	}
	active := `INSERT INTO ` + st.db + `.audience_daily (tenant, domain, language, day, access, people, generation)
SELECT ?, domain, language, day, access, toUInt32(count()), ?
FROM (SELECT if(l.account = ` + zeroUUID + `, a.subject, l.account) AS person, a.domain AS domain, a.language AS language, toDate(a.ts) AS day,
  argMax(a.access, a.ts) AS access
 FROM ` + st.db + `.activity AS a LEFT ANY JOIN contentkit_links AS l ON l.anonymous = a.subject
 WHERE a.tenant = ? AND a.ts >= ? AND a.ts < ? AND ` + st.notErased("a.subject") + `
 GROUP BY person, domain, language, day)
GROUP BY domain, language, day, access`
	if err := st.conn.Exec(ctx, active, tenant, gen, tenant, from, to, tenant); err != nil {
		return fmt.Errorf("signal: rebuild audience_daily: %w", err)
	}
	for _, table := range []string{"content_daily", "audience_daily"} {
		if err := st.conn.Exec(ctx, `DELETE FROM `+st.db+`.`+table+` WHERE tenant = ? AND day = ? AND generation < ?`, tenant, from.Format(time.DateOnly), gen); err != nil {
			return fmt.Errorf("signal: retire older %s: %w", table, err)
		}
	}
	return nil
}

// Audience says who is in the audience of the day's galleries that carry
// polarizing tags; every reader of any other gallery is. A reader who
// disliked a gallery as not into its tag is outside it either way.
type Audience struct {
	// Polarizing are the day's galleries carrying polarizing tags.
	Polarizing []uuid.UUID
	// In are their readers in their audience.
	In []PersonContent
}

// PersonContent is a person and a gallery.
type PersonContent struct{ Person, ContentID uuid.UUID }

// Read lists the galleries opened or read on day.
func (st *Store) Read(ctx context.Context, tenant string, day time.Time) ([]uuid.UUID, error) {
	from := utcDay(day)
	rows, err := st.conn.Query(ctx, `SELECT DISTINCT content_id FROM `+st.db+`.activity
 WHERE tenant = ? AND ts >= ? AND ts < ? AND action IN ('open', 'progress')`, tenant, from, from.AddDate(0, 0, 1))
	if err != nil {
		return nil, fmt.Errorf("signal: galleries read: %w", err)
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// Readers lists who opened or read contents on day, with links applied.
func (st *Store) Readers(ctx context.Context, tenant string, day time.Time, contents []uuid.UUID, links []Link) ([]PersonContent, error) {
	if len(contents) == 0 {
		return nil, nil
	}
	ctx, err := withLinks(ctx, links, nil, Audience{Polarizing: contents})
	if err != nil {
		return nil, err
	}
	from := utcDay(day)
	rows, err := st.conn.Query(ctx, `SELECT DISTINCT if(l.account = `+zeroUUID+`, a.subject, l.account) AS person, a.content_id
 FROM `+st.db+`.activity AS a LEFT ANY JOIN contentkit_links AS l ON l.anonymous = a.subject
 WHERE a.tenant = ? AND a.ts >= ? AND a.ts < ? AND a.action IN ('open', 'progress') AND a.content_id IN (SELECT content_id FROM contentkit_polarizing)
  AND `+st.notErased("a.subject"), tenant, from, from.AddDate(0, 0, 1), tenant)
	if err != nil {
		return nil, fmt.Errorf("signal: readers: %w", err)
	}
	defer rows.Close()
	var out []PersonContent
	for rows.Next() {
		var p PersonContent
		if err := rows.Scan(&p.Person, &p.ContentID); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ActiveDays sums audience_daily's newest generation of each day in
// [from, to) over the sites: active people per day and access. It serves a
// request, so it carries a deadline and a memory ceiling.
func (st *Store) ActiveDays(ctx context.Context, tenant string, from, to time.Time) (map[time.Time]map[string]uint64, error) {
	ctx = clickhouse.Context(ctx, clickhouse.WithSettings(clickhouse.Settings{"max_execution_time": 20, "max_memory_usage": 1 << 30}))
	rows, err := st.conn.Query(ctx, `SELECT day, access, sum(people) FROM `+st.db+`.audience_daily
 WHERE tenant = ? AND day >= ? AND day < ? AND (day, generation) IN (SELECT day, max(generation) FROM `+st.db+`.audience_daily
  WHERE tenant = ? AND day >= ? AND day < ? GROUP BY day)
 GROUP BY day, access`, tenant, utcDay(from).Format(time.DateOnly), utcDay(to).Format(time.DateOnly), tenant, utcDay(from).Format(time.DateOnly), utcDay(to).Format(time.DateOnly))
	if err != nil {
		return nil, fmt.Errorf("signal: active days: %w", err)
	}
	defer rows.Close()
	out := map[time.Time]map[string]uint64{}
	for rows.Next() {
		var day time.Time
		var access string
		var n uint64
		if err := rows.Scan(&day, &access, &n); err != nil {
			return nil, err
		}
		day = utcDay(day)
		if out[day] == nil {
			out[day] = map[string]uint64{}
		}
		out[day][access] = n
	}
	return out, rows.Err()
}
