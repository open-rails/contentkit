// Package analytics is the job that folds ClickHouse's activity log into
// PostgreSQL's reading state and history and rebuilds the daily rollups.
// Every write is a recompute or a maximum/latest merge, so a repeated or
// crashed run is harmless; the progress row only says where to resume.
package analytics

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/contentkit/internal/outcomevalue"
	"github.com/open-rails/contentkit/internal/readingstore"
	"github.com/open-rails/contentkit/internal/signalstore"
	"github.com/open-rails/contentkit/internal/subject"
)

// The job's schedule: it runs every FoldEvery and does the rest when due.
// Today's rollups are rebuilt every RollupEvery, and yesterday's once more
// CloseDelay after midnight UTC, when its last rows have landed.
const (
	FoldEvery   = 15 * time.Minute
	RollupEvery = time.Hour
	CloseDelay  = 15 * time.Minute
	EraseEvery  = 24 * time.Hour
	// EraseOverlap re-deletes the day before the last delete, catching rows a
	// writer still held when that subject was erased.
	EraseOverlap = 24 * time.Hour
)

const (
	// overlap re-reads rows ingested just before the watermark.
	overlap = time.Minute
	// sessionGap separates sessions.
	sessionGap = 30 * time.Minute
	// batchPeople bounds one outcome query.
	batchPeople = 500
	// linksPerRun bounds the sign-in links folded per run.
	linksPerRun = 1000
)

// ErrDown means ClickHouse is down; the next run catches up.
var ErrDown = errors.New("contentkit: analytics: ClickHouse is down")

// Job is one app's analytics job.
type Job struct {
	Tenant     string
	Signals    func() *signalstore.Store
	Reading    *readingstore.Store
	Thresholds signalstore.Thresholds
	// Polarizing are the tags whose galleries have an audience: their
	// readers with positive affinity for one of them. Nil has none.
	Polarizing func(context.Context) ([]string, error)
	// Values are the app's outcome values, and Segment a content kind's.
	Values  func(context.Context) (outcomevalue.Table, error)
	Segment func(kind string) string
	// Rebuilt runs after a day's rollups are rebuilt; its failure is logged.
	Rebuilt func(ctx context.Context, day time.Time) error
	// Surface maps an open's surface name; nil records every one as other.
	Surface func(name string) readingstore.Surface
	Now     func() time.Time
	Log     *slog.Logger
}

// Run folds new sign-in links and the activity ingested since the last run,
// then rebuilds the daily rollups and deletes erased rows when they are due.
func (j *Job) Run(ctx context.Context) error {
	ch := j.Signals()
	if ch == nil {
		return ErrDown
	}
	now := j.Now().UTC().Truncate(time.Second)
	p, err := j.Reading.Progress(ctx)
	if err != nil {
		return err
	}
	if err := j.foldLinks(ctx); err != nil {
		return err
	}
	var since time.Time
	if p.IngestedThrough != nil {
		since = p.IngestedThrough.Add(-overlap)
	}
	through, err := j.fold(ctx, ch, since)
	if err != nil {
		return err
	}
	if !through.IsZero() {
		p.IngestedThrough = &through
	}
	today := utcDay(now)
	if closing := today.Add(CloseDelay); !now.Before(closing) && (p.ClosedAt == nil || p.ClosedAt.Before(closing)) {
		if err := j.rollup(ctx, ch, today.AddDate(0, 0, -1), now); err != nil {
			return err
		}
		p.ClosedAt = &now
	}
	if p.RolledUpAt == nil || now.Sub(*p.RolledUpAt) >= RollupEvery {
		if err := j.rollup(ctx, ch, today, now); err != nil {
			return err
		}
		p.RolledUpAt = &now
	}
	if p.ErasedAt == nil || now.Sub(*p.ErasedAt) >= EraseEvery {
		var since time.Time
		if p.ErasuresThrough != nil {
			since = p.ErasuresThrough.Add(-EraseOverlap)
		}
		newest, err := ch.DeleteErased(ctx, j.Tenant, since)
		if err != nil {
			return err
		}
		if !newest.IsZero() && (p.ErasuresThrough == nil || newest.After(*p.ErasuresThrough)) {
			p.ErasuresThrough = &newest
		}
		p.ErasedAt = &now
	}
	return j.Reading.SaveProgress(ctx, p)
}

func (j *Job) surfaceOf(name string) readingstore.Surface {
	if name == "" {
		return readingstore.SurfaceOutside
	}
	if j.Surface == nil {
		return readingstore.SurfaceOther
	}
	return j.Surface(name)
}

// foldLinks folds sign-in links recorded since the last run.
func (j *Job) foldLinks(ctx context.Context) error {
	links, err := j.Reading.Unfolded(ctx, linksPerRun)
	if err != nil {
		return err
	}
	for _, l := range links {
		if err := j.Reading.Fold(ctx, l); err != nil {
			return err
		}
	}
	return nil
}

type personDay struct {
	person uuid.UUID
	day    time.Time
}

// fold recomputes the outcomes and history of every person × day with rows
// ingested since since, merges them in and returns the newest ingestion.
func (j *Job) fold(ctx context.Context, ch *signalstore.Store, since time.Time) (time.Time, error) {
	touched, through, err := ch.Touched(ctx, j.Tenant, since)
	if err != nil || len(touched) == 0 {
		return time.Time{}, err
	}
	subjects := make([]uuid.UUID, 0, len(touched))
	for _, t := range touched {
		subjects = append(subjects, t.Subject)
	}
	accounts, err := j.Reading.Accounts(ctx, subjects)
	if err != nil {
		return time.Time{}, err
	}
	days := map[uuid.UUID]map[time.Time]bool{}
	for _, t := range touched {
		person := t.Subject
		if a, ok := accounts[person]; ok {
			person = a
		}
		if days[person] == nil {
			days[person] = map[time.Time]bool{}
		}
		days[person][utcDay(t.Day)] = true
	}
	people := make([]uuid.UUID, 0, len(days))
	for p := range days {
		people = append(people, p)
	}
	erased, err := j.Reading.Erased(ctx, people)
	if err != nil {
		return time.Time{}, err
	}
	people = slices.DeleteFunc(people, func(p uuid.UUID) bool { return erased[p] })
	last := map[uuid.UUID]time.Time{}
	for _, t := range touched {
		if _, linked := accounts[t.Subject]; linked || erased[t.Subject] || !subject.IsAnonymous(t.Subject) {
			continue
		}
		if t.Last.After(last[t.Subject]) {
			last[t.Subject] = t.Last
		}
	}
	if err := j.Reading.Active(ctx, last); err != nil {
		return time.Time{}, err
	}
	slices.SortFunc(people, func(a, b uuid.UUID) int { return slices.Compare(a[:], b[:]) })
	for start := 0; start < len(people); start += batchPeople {
		if err := j.foldBatch(ctx, ch, people[start:min(start+batchPeople, len(people))], days); err != nil {
			return time.Time{}, err
		}
	}
	return through, nil
}

func (j *Job) foldBatch(ctx context.Context, ch *signalstore.Store, people []uuid.UUID, days map[uuid.UUID]map[time.Time]bool) error {
	anonymous, err := j.Reading.Anonymous(ctx, people)
	if err != nil {
		return err
	}
	subjects := slices.Clone(people)
	var links []signalstore.Link
	var from, to time.Time
	for _, p := range people {
		for _, a := range anonymous[p] {
			subjects = append(subjects, a)
			links = append(links, signalstore.Link{Anonymous: a, Account: p})
		}
		for d := range days[p] {
			if from.IsZero() || d.Before(from) {
				from = d
			}
			if end := d.AddDate(0, 0, 1); end.After(to) {
				to = end
			}
		}
	}
	outcomes, err := ch.Outcomes(ctx, j.Tenant, j.Thresholds, subjects, links, from, to)
	if err != nil {
		return err
	}
	events, err := ch.Timeline(ctx, j.Tenant, subjects, links, from.Add(-sessionGap), to)
	if err != nil {
		return err
	}
	type key struct {
		personDay
		content uuid.UUID
	}
	byGallery := map[key]signalstore.Outcome{}
	var updates []readingstore.StateUpdate
	for _, o := range outcomes {
		if !days[o.Person][utcDay(o.Day)] {
			continue
		}
		byGallery[key{personDay{o.Person, utcDay(o.Day)}, o.ContentID}] = o
		if o.Class == signalstore.ClassNone {
			continue
		}
		u := readingstore.StateUpdate{Subject: o.Person, ContentID: o.ContentID, Day: o.Day, LastSeen: o.LastAt, Outcome: ordinal(o.Class), Furthest: o.Furthest}
		if o.HasProgress {
			position := o.Position
			u.Position = &position
		}
		updates = append(updates, u)
	}
	if err := j.Reading.Merge(ctx, updates); err != nil {
		return err
	}
	// Each gallery's surface is that of its first open that day.
	surface := map[key]readingstore.Surface{}
	for _, e := range events {
		k := key{personDay{e.Person, utcDay(e.At)}, e.ContentID}
		if _, seen := surface[k]; e.Action == signalstore.ActionOpen && !seen {
			surface[k] = j.surfaceOf(e.Surface)
		}
	}
	history := map[personDay][]readingstore.Action{}
	var prev time.Time
	var current uuid.UUID
	start := true
	emitted := map[key]bool{}
	for _, e := range events {
		if e.Person != current {
			current, prev = e.Person, time.Time{}
		}
		if prev.IsZero() || e.At.Sub(prev) > sessionGap {
			start = true
		}
		prev = e.At
		pd := personDay{e.Person, utcDay(e.At)}
		if !days[e.Person][pd.day] {
			start = false // a session already under way before the day
			continue
		}
		var a readingstore.Action
		switch e.Action {
		case signalstore.ActionSearch:
			a = readingstore.Action{Kind: readingstore.ActionSearch, Query: e.Query}
		case signalstore.ActionOpen, signalstore.ActionProgress, signalstore.ActionReaction, signalstore.ActionFavorite, signalstore.ActionHide:
			k := key{pd, e.ContentID}
			o, ok := byGallery[k]
			read := e.Action == signalstore.ActionOpen || e.Action == signalstore.ActionProgress
			// At its first open; a day with only a reaction, at the reaction.
			if !ok || emitted[k] || o.Class != signalstore.ClassNone && !read || o.Class == signalstore.ClassNone && o.Reaction == "" {
				continue
			}
			emitted[k] = true
			a = readingstore.Action{Kind: readingstore.ActionGallery, ID: e.ContentID, Tenths: tenths(o), ActiveS: uint32(o.ActiveMS / 1000), Reaction: reaction(o.Reaction), Surface: surface[k]}
		default:
			continue
		}
		a.At, a.Offset, a.SessionStart = e.At, e.Offset, start
		start = false
		history[pd] = append(history[pd], a)
	}
	var out []readingstore.HistoryDay
	for _, p := range people {
		for d := range days[p] {
			out = append(out, readingstore.HistoryDay{Subject: p, Day: d, Actions: history[personDay{p, d}]})
		}
	}
	if err := j.Reading.PutHistory(ctx, out); err != nil {
		return fmt.Errorf("analytics: history: %w", err)
	}
	return nil
}

// rollup rebuilds day's content_daily and audience_daily, then runs Rebuilt.
func (j *Job) rollup(ctx context.Context, ch *signalstore.Store, day, now time.Time) error {
	subjects, err := ch.ActiveSubjects(ctx, j.Tenant, day)
	if err != nil {
		return err
	}
	accounts, err := j.Reading.Accounts(ctx, subjects)
	if err != nil {
		return err
	}
	links := make([]signalstore.Link, 0, len(accounts))
	for anon, account := range accounts {
		links = append(links, signalstore.Link{Anonymous: anon, Account: account})
	}
	audience, err := j.audience(ctx, ch, day, links)
	if err != nil {
		return err
	}
	if err := ch.RebuildDay(ctx, j.Tenant, j.Thresholds, day, links, audience, now); err != nil {
		return err
	}
	if j.Rebuilt != nil {
		if err := j.Rebuilt(ctx, day); err != nil {
			j.Log.WarnContext(ctx, "contentkit: analytics: after the daily rebuild", "day", day.Format(time.DateOnly), "err", err)
		}
	}
	return nil
}

// audience finds the readers in the audience of day's galleries carrying
// polarizing tags: those with positive affinity for one of the gallery's.
func (j *Job) audience(ctx context.Context, ch *signalstore.Store, day time.Time, links []signalstore.Link) (signalstore.Audience, error) {
	var out signalstore.Audience
	if j.Polarizing == nil {
		return out, nil
	}
	tags, err := j.Polarizing(ctx)
	if err != nil || len(tags) == 0 {
		return out, err
	}
	read, err := ch.Read(ctx, j.Tenant, day)
	if err != nil {
		return out, err
	}
	carried, err := j.Reading.Tags(ctx, read, tags)
	if err != nil || len(carried) == 0 {
		return out, err
	}
	for id := range carried {
		out.Polarizing = append(out.Polarizing, id)
	}
	readers, err := ch.Readers(ctx, j.Tenant, day, out.Polarizing, links)
	if err != nil {
		return out, err
	}
	seen := map[uuid.UUID]bool{}
	var people []uuid.UUID
	for _, r := range readers {
		if !seen[r.Person] {
			seen[r.Person] = true
			people = append(people, r.Person)
		}
	}
	values, err := j.Values(ctx)
	if err != nil {
		return out, err
	}
	since := day.AddDate(0, 0, 1).Add(-readingstore.AffinityWindow)
	affine := map[uuid.UUID]map[string]bool{}
	for start := 0; start < len(people); start += batchPeople {
		got, err := j.Reading.Affine(ctx, people[start:min(start+batchPeople, len(people))], tags, since, values, j.Segment)
		if err != nil {
			return out, err
		}
		for p, t := range got {
			affine[p] = t
		}
	}
	for _, r := range readers {
		for _, tag := range carried[r.ContentID] {
			if affine[r.Person][tag] {
				out.In = append(out.In, r)
				break
			}
		}
	}
	return out, nil
}

func ordinal(class string) int16 {
	switch class {
	case signalstore.ClassBounce:
		return readingstore.OutcomeBounce
	case signalstore.ClassNeutral:
		return readingstore.OutcomeNeutral
	case signalstore.ClassGood:
		return readingstore.OutcomeGood
	case signalstore.ClassComplete:
		return readingstore.OutcomeComplete
	}
	return readingstore.OutcomeNone
}

func reaction(r string) readingstore.Reaction {
	switch r {
	case signalstore.ReactionLike:
		return readingstore.ReactionLike
	case signalstore.ReactionDislike:
		return readingstore.ReactionDislike
	case signalstore.ReactionFavorite:
		return readingstore.ReactionFavorite
	case signalstore.ReactionHidden:
		return readingstore.ReactionHidden
	}
	return readingstore.ReactionNone
}

func tenths(o signalstore.Outcome) uint8 {
	if o.Length == 0 {
		return 0
	}
	return uint8(min(10, uint64(o.Furthest)*10/uint64(o.Length)))
}

func utcDay(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}
