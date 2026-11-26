package signalstore

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/ext"
	"github.com/google/uuid"
)

// Thresholds classify a person × gallery × day by the fraction read, so a
// 5-page and a 200-page gallery are judged alike. They are the outcome
// query's parameters and can be retuned.
type Thresholds struct {
	// BounceActive: a bounce is nothing past the first page (any point of a
	// video), less active time than this and no reaction.
	BounceActive time.Duration
	// GoodFraction of the pages or of the video, at the minimum pace.
	GoodFraction float64
	// CompleteFraction of a video; a gallery is complete at its last page.
	CompleteFraction float64
	// PageTime and SecondTime are the minimum active time per page and per
	// second of video reached, so flipping or seeking ahead doesn't count.
	PageTime, SecondTime time.Duration
}

// DefaultThresholds are #300's starting values.
var DefaultThresholds = Thresholds{BounceActive: 10 * time.Second, GoodFraction: 0.3, CompleteFraction: 0.9,
	PageTime: 1500 * time.Millisecond, SecondTime: 500 * time.Millisecond}

// Read classes, worst to best.
const (
	ClassNone     = ""
	ClassBounce   = "bounce"
	ClassNeutral  = "neutral"
	ClassGood     = "good"
	ClassComplete = "complete"
)

// Reactions an outcome carries: the day's last one.
const (
	ReactionLike     = "like"
	ReactionDislike  = "dislike"
	ReactionFavorite = "favorite"
	ReactionHidden   = "hidden"
)

// Link maps an anonymous subject to the account it signed in as: its rows
// count as the account's.
type Link struct{ Anonymous, Account uuid.UUID }

// Outcome is one person × gallery × day.
type Outcome struct {
	Person      uuid.UUID
	Day         time.Time
	ContentKind string
	ContentID   uuid.UUID
	// FirstAt and LastAt span its open and progress rows; zero for a day
	// with only a reaction.
	FirstAt, LastAt time.Time
	ActiveMS        uint64
	Furthest        uint32
	// Position is the current point of the day's last progress report;
	// HasProgress says whether there was one.
	Position    uint32
	HasProgress bool
	Length      uint32
	Unit        string
	Class       string
	Reaction    string
	Reason      string
}

const zeroUUID = "toUUID('00000000-0000-0000-0000-000000000000')"

// outcomeSQL is the parameterized query giving each person × gallery × day
// (× site when bySite) its outcome. where filters activity rows aliased a;
// its binds follow the thresholds and the tenant.
func (st *Store) outcomeSQL(bySite bool, where string) string {
	site, siteCols := "", ""
	if bySite {
		site, siteCols = ", a.domain AS domain, a.language AS language", ", domain, language"
	}
	return `SELECT person, day, content_id, content_kind` + siteCols + `, first_at, last_at, opened, has_progress, active_ms, furthest, position, length, unit,
 multiIf(last_reaction.1 = 'reaction' AND last_reaction.2 = 1, 'like', last_reaction.1 = 'reaction' AND last_reaction.2 = -1, 'dislike',
  last_reaction.1 = 'favorite' AND last_reaction.2 = 1, 'favorite', last_reaction.1 = 'hide' AND last_reaction.2 = 1, 'hidden', '') AS reaction,
 if(reaction = '', '', last_reaction.3) AS reason,
 toBool(toFloat64(active_ms) >= toFloat64(furthest) * if(unit = 'second', ?, ?)) AS paced,
 multiIf(NOT opened AND NOT has_progress, '',
  paced AND length > 0 AND if(unit = 'second', furthest >= length * ?, furthest >= length), 'complete',
  reaction = '' AND active_ms < ? AND (unit = 'second' OR furthest <= 1), 'bounce',
  paced AND length > 0 AND furthest >= length * ?, 'good',
  'neutral') AS class
FROM (SELECT if(l.account = ` + zeroUUID + `, a.subject, l.account) AS person, toDate(a.ts) AS day, a.content_id AS content_id,
  any(a.content_kind) AS content_kind` + site + `,
  minIf(a.ts, a.action IN ('open', 'progress')) AS first_at, maxIf(a.ts, a.action IN ('open', 'progress')) AS last_at,
  toBool(countIf(a.action = 'open') > 0) AS opened, toBool(countIf(a.action = 'progress') > 0) AS has_progress,
  sumIf(toUInt64(a.active_ms), a.action = 'progress') AS active_ms, maxIf(a.furthest, a.action = 'progress') AS furthest,
  argMaxIf(a.current, a.ts, a.action = 'progress') AS position, maxIf(a.length, a.action = 'progress') AS length,
  anyIf(a.unit, a.action = 'progress') AS unit,
  argMaxIf(tuple(toString(a.action), a.value, toString(a.reason)), a.ts, a.action IN ('reaction', 'favorite', 'hide')) AS last_reaction
 FROM ` + st.db + `.activity AS a LEFT ANY JOIN contentkit_links AS l ON l.anonymous = a.subject
 WHERE a.tenant = ? AND a.action IN ('open', 'progress', 'reaction', 'favorite', 'hide') AND a.content_id != ` + zeroUUID + `
  AND ` + st.notErased("a.subject") + ` AND ` + where + `
 GROUP BY person, day, content_id` + siteCols + `)`
}

// outcomeArgs are outcomeSQL's leading binds: the thresholds, then the
// tenant twice (activity and its erasure filter).
func outcomeArgs(t Thresholds, tenant string) []any {
	return []any{t.SecondTime.Seconds() * 1000, t.PageTime.Seconds() * 1000, t.CompleteFraction, t.BounceActive.Milliseconds(), t.GoodFraction, tenant, tenant}
}

// withLinks sends links as the query's contentkit_links table, subjects
// (when not nil) as contentkit_subjects and an audience as
// contentkit_polarizing and contentkit_audience.
func withLinks(ctx context.Context, links []Link, subjects []uuid.UUID, audience ...Audience) (context.Context, error) {
	table, err := ext.NewTable("contentkit_links", ext.Column("anonymous", "UUID"), ext.Column("account", "UUID"))
	if err != nil {
		return nil, err
	}
	for _, l := range links {
		if err := table.Append(l.Anonymous, l.Account); err != nil {
			return nil, err
		}
	}
	tables := []*ext.Table{table}
	if subjects != nil {
		s, err := ext.NewTable("contentkit_subjects", ext.Column("subject", "UUID"))
		if err != nil {
			return nil, err
		}
		for _, id := range subjects {
			if err := s.Append(id); err != nil {
				return nil, err
			}
		}
		tables = append(tables, s)
	}
	for _, a := range audience {
		polarizing, err := ext.NewTable("contentkit_polarizing", ext.Column("content_id", "UUID"))
		if err != nil {
			return nil, err
		}
		for _, id := range a.Polarizing {
			if err := polarizing.Append(id); err != nil {
				return nil, err
			}
		}
		in, err := ext.NewTable("contentkit_audience", ext.Column("person", "UUID"), ext.Column("content_id", "UUID"))
		if err != nil {
			return nil, err
		}
		for _, p := range a.In {
			if err := in.Append(p.Person, p.ContentID); err != nil {
				return nil, err
			}
		}
		tables = append(tables, polarizing, in)
	}
	return clickhouse.Context(ctx, clickhouse.WithExternalTable(tables...)), nil
}

// Outcomes computes the outcomes of subjects' galleries on the UTC days in
// [from, to); links fold anonymous subjects into their accounts.
func (st *Store) Outcomes(ctx context.Context, tenant string, t Thresholds, subjects []uuid.UUID, links []Link, from, to time.Time) ([]Outcome, error) {
	if len(subjects) == 0 {
		return nil, nil
	}
	ctx, err := withLinks(ctx, links, subjects)
	if err != nil {
		return nil, err
	}
	q := st.outcomeSQL(false, "a.ts >= ? AND a.ts < ? AND a.subject IN (SELECT subject FROM contentkit_subjects)")
	rows, err := st.conn.Query(ctx, q, append(outcomeArgs(t, tenant), from.UTC(), to.UTC())...)
	if err != nil {
		return nil, fmt.Errorf("signal: outcomes: %w", err)
	}
	defer rows.Close()
	var out []Outcome
	for rows.Next() {
		var o Outcome
		var opened, paced bool
		if err := rows.Scan(&o.Person, &o.Day, &o.ContentID, &o.ContentKind, &o.FirstAt, &o.LastAt, &opened, &o.HasProgress, &o.ActiveMS,
			&o.Furthest, &o.Position, &o.Length, &o.Unit, &o.Reaction, &o.Reason, &paced, &o.Class); err != nil {
			return nil, fmt.Errorf("signal: outcomes: %w", err)
		}
		if !opened && !o.HasProgress {
			o.FirstAt, o.LastAt = time.Time{}, time.Time{}
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// Event is one raw row of a person's timeline.
type Event struct {
	Person    uuid.UUID
	At        time.Time
	Action    string
	ContentID uuid.UUID
	Query     string
	Offset    int16
	// Surface is an open's: the list it came from.
	Surface string
}

// Timeline is every row of subjects in [from, to), people's in time order,
// with links applied: the gaps between them mark sessions.
func (st *Store) Timeline(ctx context.Context, tenant string, subjects []uuid.UUID, links []Link, from, to time.Time) ([]Event, error) {
	if len(subjects) == 0 {
		return nil, nil
	}
	ctx, err := withLinks(ctx, links, subjects)
	if err != nil {
		return nil, err
	}
	rows, err := st.conn.Query(ctx, `SELECT if(l.account = `+zeroUUID+`, a.subject, l.account) AS person, a.ts, toString(a.action), a.content_id, a.query, a.utc_offset, toString(a.surface)
 FROM `+st.db+`.activity AS a LEFT ANY JOIN contentkit_links AS l ON l.anonymous = a.subject
 WHERE a.tenant = ? AND a.ts >= ? AND a.ts < ? AND a.subject IN (SELECT subject FROM contentkit_subjects) AND `+st.notErased("a.subject")+`
 ORDER BY person, a.ts`, tenant, from.UTC(), to.UTC(), tenant)
	if err != nil {
		return nil, fmt.Errorf("signal: timeline: %w", err)
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.Person, &e.At, &e.Action, &e.ContentID, &e.Query, &e.Offset, &e.Surface); err != nil {
			return nil, fmt.Errorf("signal: timeline: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// SubjectDay is a subject with rows on a UTC day.
type SubjectDay struct {
	Subject uuid.UUID
	Day     time.Time
	// Last is the subject's latest activity that day.
	Last time.Time
}

// Touched lists the subjects and days with rows ingested at or after since
// (all of them when since is zero) and the newest ingestion time, the next
// watermark: ClickHouse's clock, not the API's. Rows are stamped when the
// API receives them and land within seconds, so their time bounds the
// partitions read.
func (st *Store) Touched(ctx context.Context, tenant string, since time.Time) ([]SubjectDay, time.Time, error) {
	where, args := "tenant = ?", []any{tenant}
	if !since.IsZero() {
		where, args = where+" AND ingested_at >= ? AND ts >= ?", append(args, since.UTC(), since.Add(-48*time.Hour).UTC())
	}
	rows, err := st.conn.Query(ctx, `SELECT subject, toDate(ts) AS day, max(ts), max(ingested_at) FROM `+st.db+`.activity
 WHERE `+where+` AND `+st.notErased("subject")+` GROUP BY subject, day`, append(args, tenant)...)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("signal: touched subjects: %w", err)
	}
	defer rows.Close()
	var out []SubjectDay
	var newest time.Time
	for rows.Next() {
		var s SubjectDay
		var ingested time.Time
		if err := rows.Scan(&s.Subject, &s.Day, &s.Last, &ingested); err != nil {
			return nil, time.Time{}, err
		}
		if ingested.After(newest) {
			newest = ingested
		}
		out = append(out, s)
	}
	return out, newest, rows.Err()
}

// ActiveSubjects lists the subjects with rows on day.
func (st *Store) ActiveSubjects(ctx context.Context, tenant string, day time.Time) ([]uuid.UUID, error) {
	from := utcDay(day)
	rows, err := st.conn.Query(ctx, `SELECT DISTINCT subject FROM `+st.db+`.activity WHERE tenant = ? AND ts >= ? AND ts < ?`,
		tenant, from, from.AddDate(0, 0, 1))
	if err != nil {
		return nil, fmt.Errorf("signal: active subjects: %w", err)
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

func utcDay(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}

// sqlList joins placeholders for n values.
func sqlList(n int) string { return "(" + strings.TrimSuffix(strings.Repeat("?, ", n), ", ") + ")" }
