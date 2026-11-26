// Package readquality is the quality of a read: P(sequel), the probability
// that the person would read a sequel, remake or enhanced version, per
// segment and outcome, and q, its place from -1 to +1 around the segment's
// base rate. It names the keys of read_quality and holds the starting guesses
// used until the monthly refit measures them.
package readquality

import (
	"context"
	"maps"

	"github.com/jackc/pgx/v5"
)

// Segments, by a content kind's presentation.
const (
	Gallery = "gallery"
	Video   = "video"
	Post    = "post"
)

// Segments are every segment, in seed order.
var Segments = []string{Gallery, Video, Post}

// SegmentOf is the segment of a content kind with this presentation.
func SegmentOf(presentation string) string {
	switch presentation {
	case "player":
		return Video
	case "post":
		return Post
	}
	return Gallery
}

// Outcomes. A person-day's outcome is the day's last reaction, keyed with its
// reason when one was given ("dislike:not_for_me"), else its read class.
const (
	// Base is the segment's rate with no information; its q is 0.
	Base     = "base"
	Bounce   = "bounce"
	Neutral  = "neutral"
	Good     = "good"
	Complete = "complete"
	Like     = "like"
	Favorite = "favorite"
	Hidden   = "hidden"
	Dislike  = "dislike"
)

// Dislike reasons.
const (
	NotForMe   = "not_for_me"
	LowQuality = "low_quality"
)

// Keyed is a reaction's outcome key with its reason.
func Keyed(reaction, reason string) string {
	if reason == "" {
		return reaction
	}
	return reaction + ":" + reason
}

// BaseRate is the starting guess of a segment's P(sequel).
const BaseRate = 0.25

// guesses are the starting P(sequel) of every outcome.
var guesses = map[string]float64{
	Base: BaseRate, Bounce: 0.05, Neutral: 0.15, Good: 0.40, Complete: 0.60,
	Like: 0.70, Favorite: 0.90, Hidden: 0, Dislike: 0.05,
	Keyed(Dislike, NotForMe): 0, Keyed(Dislike, LowQuality): 0.05,
}

// Q places p around the base rate b: (p-b)/(1-b) at or above it, (p-b)/b
// below. A loved read is +1, an ordinary one 0, a rejected one -1.
func Q(p, b float64) float64 {
	if b <= 0 || b >= 1 {
		return 0
	}
	if p >= b {
		return (p - b) / (1 - b)
	}
	return (p - b) / b
}

// Row is one read_quality row.
type Row struct {
	Segment, Outcome string
	PSequel, Q       float64
}

// Seed is every segment's starting rows, to insert with fitted_at NULL.
func Seed() []Row {
	var out []Row
	for _, segment := range Segments {
		for _, outcome := range outcomes() {
			p := guesses[outcome]
			out = append(out, Row{Segment: segment, Outcome: outcome, PSequel: p, Q: Q(p, BaseRate)})
		}
	}
	return out
}

func outcomes() []string {
	return []string{Base, Bounce, Neutral, Good, Complete, Like, Favorite, Hidden, Dislike, Keyed(Dislike, NotForMe), Keyed(Dislike, LowQuality)}
}

// Table is q by segment and outcome.
type Table map[string]map[string]float64

// Defaults is the table of the starting guesses.
func Defaults() Table {
	t := Table{}
	for _, r := range Seed() {
		if t[r.Segment] == nil {
			t[r.Segment] = map[string]float64{}
		}
		t[r.Segment][r.Outcome] = r.Q
	}
	return t
}

// Querier reads read_quality.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Load reads an app's table: its rows over the starting guesses, so an
// unseeded segment or outcome still has its guess.
func Load(ctx context.Context, q Querier, schema, app string) (Table, error) {
	t := Defaults()
	rows, err := q.Query(ctx, `SELECT segment, outcome, q FROM `+pgx.Identifier{schema, "read_quality"}.Sanitize()+` WHERE app_id = $1::uuid`, app)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var segment, outcome string
		var v float64
		if err := rows.Scan(&segment, &outcome, &v); err != nil {
			return nil, err
		}
		if t[segment] == nil {
			t[segment] = maps.Clone(t[Gallery])
		}
		t[segment][outcome] = v
	}
	return t, rows.Err()
}

// Segment is one segment's q by outcome; an unknown segment has the
// gallery's.
func (t Table) Segment(segment string) map[string]float64 {
	if s, ok := t[segment]; ok {
		return s
	}
	return t[Gallery]
}

// Of is a person-day's q: its reaction's, with the reason when that outcome
// is known, else its class's; 0 with neither.
func (t Table) Of(segment, class, reaction, reason string) float64 {
	s := t.Segment(segment)
	if reaction != "" {
		if v, ok := s[Keyed(reaction, reason)]; ok {
			return v
		}
		if v, ok := s[reaction]; ok {
			return v
		}
	}
	return s[class]
}
