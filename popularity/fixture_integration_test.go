package popularity_test

import (
	"context"
	"fmt"
	"math"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/engagementtest"
	ckhub "github.com/open-rails/contentkit/internal/hub"
	"github.com/open-rails/contentkit/internal/signaltest"
	"github.com/open-rails/contentkit/popularity"
	"github.com/open-rails/contentkit/signal"
)

const (
	fxTenant  = "doujins"
	fxKind    = "gallery"
	fxDB      = "contentkit_popularity_test"
	typeReact = "reaction"
)

// Fixture galleries (docs/popularity-policy.md lists them with the judgments);
// each id is a canonical UUIDv7 ending in the gallery's fixture number.
const (
	fxNoVotes      = "01920000-0000-7000-8000-000000000101"
	fxOneLike      = "01920000-0000-7000-8000-000000000102"
	fxOneDislike   = "01920000-0000-7000-8000-000000000103"
	fxMixed        = "01920000-0000-7000-8000-000000000104"
	fxLiked        = "01920000-0000-7000-8000-000000000105"
	fxDisliked     = "01920000-0000-7000-8000-000000000106"
	fxMoreReaders  = "01920000-0000-7000-8000-000000000107"
	fxBigDisliked  = "01920000-0000-7000-8000-000000000108"
	fxCrowdDislike = "01920000-0000-7000-8000-000000000109"
	fxTenLikes     = "01920000-0000-7000-8000-000000000110"
	fxRepeat       = "01920000-0000-7000-8000-000000000111"
	fxFive         = "01920000-0000-7000-8000-000000000112"
	fxReturn       = "01920000-0000-7000-8000-000000000113"
	fxOnce         = "01920000-0000-7000-8000-000000000114"
	fxShort        = "01920000-0000-7000-8000-000000000121"
	fxLongSkim     = "01920000-0000-7000-8000-000000000122"
	fxLongFull     = "01920000-0000-7000-8000-000000000123"
	fxMulti        = "01920000-0000-7000-8000-000000000131"
	fxDup          = "01920000-0000-7000-8000-000000000141"
	fxSingle       = "01920000-0000-7000-8000-000000000142"
	fxEarly        = "01920000-0000-7000-8000-000000000151"
	fxLate         = "01920000-0000-7000-8000-000000000152"
	fxOutside      = "01920000-0000-7000-8000-000000000153"
	fxVoteChanges  = "01920000-0000-7000-8000-000000000161"
)

var fxNames = map[string]string{
	fxNoVotes: "no_votes", fxOneLike: "one_like", fxOneDislike: "one_dislike", fxMixed: "mixed", fxLiked: "liked",
	fxDisliked: "disliked", fxMoreReaders: "more_readers", fxBigDisliked: "big_disliked", fxCrowdDislike: "crowd_disliked",
	fxTenLikes: "ten_likes", fxRepeat: "repeat_reader", fxFive: "five_readers", fxReturn: "return_readers", fxOnce: "once_readers",
	fxShort: "short_full", fxLongSkim: "long_skimmed", fxLongFull: "long_full", fxMulti: "two_versions", fxDup: "duplicate_delivery",
	fxSingle: "single_delivery", fxEarly: "window_start", fxLate: "window_end", fxOutside: "before_window", fxVoteChanges: "vote_changes",
}

type fxCheckpoint struct {
	revision       uint64
	pages, seconds uint32
}

type fxSession struct {
	work      string
	subject   signal.Subject
	viewID    string
	version   string
	pageCount uint32
	at        time.Time
	// checkpoints in delivery order; the highest revision is the session.
	deliveries []fxCheckpoint
}

type fxVote struct {
	work       string
	subject    signal.Subject
	value      float64
	at         time.Time
	deliveries int
}

type popularityFixture struct {
	sessions []fxSession
	votes    []fxVote
	window   signal.Window
}

func fxReader(work string, i int) signal.Subject {
	if i%2 == 0 {
		return signal.Subject{UserID: fmt.Sprintf("u-%s-%d", work, i)}
	}
	return signal.Subject{AnonKey: fmt.Sprintf("anon-%s-%d", work, i)}
}

// buildFixture is the judged fixture set: every scenario the tracker names,
// all inside the 30-day window except before_window.
func buildFixture(now time.Time) popularityFixture {
	window, err := popularity.WindowForPeriod("30d", now)
	if err != nil {
		panic(err)
	}
	mid := window.From.AddDate(0, 0, 15).Add(6 * time.Hour)
	var fx popularityFixture
	fx.window = window
	session := func(work string, subject signal.Subject, viewID, version string, pageCount, pages, seconds uint32, at time.Time) {
		fx.sessions = append(fx.sessions, fxSession{work: work, subject: subject, viewID: viewID, version: version, pageCount: pageCount, at: at,
			deliveries: []fxCheckpoint{{revision: 1, pages: pages, seconds: seconds}}})
	}
	readers := func(work string, n int, pageCount, pages, seconds uint32, at time.Time) {
		for i := 0; i < n; i++ {
			session(work, fxReader(work, i), fmt.Sprintf("s-%s-%d", work, i), "v", pageCount, pages, seconds, at)
		}
	}
	votes := func(work string, from, to int, value float64) {
		for i := from; i < to; i++ {
			fx.votes = append(fx.votes, fxVote{work: work, subject: fxReader(work, i), value: value, at: mid.Add(time.Hour), deliveries: 1})
		}
	}

	// Votes: 20 readers each exposing 8 of 10 pages over 80 s (not completed).
	for _, g := range []string{fxNoVotes, fxOneLike, fxOneDislike, fxMixed, fxLiked, fxDisliked} {
		readers(g, 20, 10, 8, 80, mid)
	}
	votes(fxOneLike, 0, 1, 1)
	votes(fxOneDislike, 0, 1, -1)
	votes(fxMixed, 0, 10, 1)
	votes(fxMixed, 10, 20, -1)
	votes(fxLiked, 0, 8, 1)
	votes(fxDisliked, 0, 8, -1)
	readers(fxMoreReaders, 25, 10, 8, 80, mid)
	readers(fxBigDisliked, 40, 10, 8, 80, mid)
	votes(fxBigDisliked, 0, 30, -1)
	readers(fxCrowdDislike, 120, 10, 8, 80, mid)
	votes(fxCrowdDislike, 0, 60, -1)
	readers(fxTenLikes, 10, 10, 8, 80, mid)
	votes(fxTenLikes, 0, 10, 1)

	// Consumption: full reads (10/10 pages, 120 s).
	for i := 0; i < 50; i++ {
		session(fxRepeat, fxReader(fxRepeat, 0), fmt.Sprintf("s-%s-%d", fxRepeat, i), "v", 10, 10, 120, mid.Add(time.Duration(i)*time.Minute))
	}
	readers(fxFive, 5, 10, 10, 120, mid)
	readers(fxReturn, 10, 10, 10, 120, mid)
	for i := 0; i < 10; i++ {
		session(fxReturn, fxReader(fxReturn, i), fmt.Sprintf("s2-%s-%d", fxReturn, i), "v", 10, 10, 120, mid.AddDate(0, 0, 3))
	}
	readers(fxOnce, 10, 10, 10, 120, mid)

	// Size: 20 readers each.
	readers(fxShort, 20, 6, 6, 60, mid)
	readers(fxLongSkim, 20, 200, 30, 300, mid)
	readers(fxLongFull, 20, 200, 200, 2000, mid)

	// Versions: en (10 pages) fully read by e0..e9; es (12 pages) fully read
	// by e0..e4 and half read (6/12, 48 s) by three other readers.
	for i := 0; i < 10; i++ {
		session(fxMulti, fxReader(fxMulti, i), fmt.Sprintf("en-%d", i), "v-en", 10, 10, 120, mid)
	}
	for i := 0; i < 5; i++ {
		session(fxMulti, fxReader(fxMulti, i), fmt.Sprintf("es-%d", i), "v-es", 12, 12, 150, mid.Add(2*time.Hour))
	}
	for i := 100; i < 103; i++ {
		session(fxMulti, fxReader(fxMulti, i), fmt.Sprintf("es-%d", i), "v-es", 12, 6, 48, mid.Add(3*time.Hour))
	}

	// Duplicate delivery: the same two checkpoints, final one first, each
	// delivered again; likes delivered twice.
	for i := 0; i < 10; i++ {
		fx.sessions = append(fx.sessions, fxSession{work: fxDup, subject: fxReader(fxDup, i), viewID: fmt.Sprintf("s-%s-%d", fxDup, i), version: "v", pageCount: 10, at: mid,
			deliveries: []fxCheckpoint{{2, 8, 80}, {1, 3, 30}, {2, 8, 80}, {1, 3, 30}}})
		fx.sessions = append(fx.sessions, fxSession{work: fxSingle, subject: fxReader(fxSingle, i), viewID: fmt.Sprintf("s-%s-%d", fxSingle, i), version: "v", pageCount: 10, at: mid,
			deliveries: []fxCheckpoint{{1, 3, 30}, {2, 8, 80}}})
	}
	for i := 0; i < 4; i++ {
		fx.votes = append(fx.votes, fxVote{work: fxDup, subject: fxReader(fxDup, i), value: 1, at: mid.Add(time.Hour), deliveries: 2})
		fx.votes = append(fx.votes, fxVote{work: fxSingle, subject: fxReader(fxSingle, i), value: 1, at: mid.Add(time.Hour), deliveries: 1})
	}

	// Vote changes: feedback is each subject's current preference, counted on
	// the day it last changed.
	readers(fxVoteChanges, 20, 10, 8, 80, mid)
	vote := func(i int, value float64, at time.Time) {
		fx.votes = append(fx.votes, fxVote{work: fxVoteChanges, subject: fxReader(fxVoteChanges, i), value: value, at: at, deliveries: 1})
	}
	vote(0, 1, mid)                           // liked, then
	vote(0, 0, mid.Add(time.Hour))            // unvoted: neither
	vote(1, 1, window.From.AddDate(0, 0, -1)) // liked before the window, unchanged
	vote(2, 1, window.From.AddDate(0, 0, -1)) // liked before the window, then
	vote(2, -1, mid)                          // disliked inside it
	vote(3, -1, mid)                          // disliked, then
	vote(3, 1, mid.Add(time.Hour))            // liked
	vote(4, 1, mid)                           // liked, delivered twice
	vote(4, 1, mid)

	// Window: identical readers at the first hour of the window, today, and
	// the hour before the window opened.
	today := now.UTC().Truncate(24 * time.Hour)
	readers(fxEarly, 10, 10, 10, 120, window.From.Add(time.Hour))
	readers(fxLate, 10, 10, 10, 120, today.Add(time.Hour))
	readers(fxOutside, 10, 10, 10, 120, window.From.Add(-time.Hour))
	return fx
}

func (fx popularityFixture) workIDs() []string {
	seen := map[string]struct{}{}
	for _, s := range fx.sessions {
		seen[s.work] = struct{}{}
	}
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func ref(id string) signal.ContentRef { return contentref.New(fxTenant, fxKind, id) }

// cid is the nth deterministic content id: a canonical UUIDv7, ordered by n.
func cid(n int) string { return fmt.Sprintf("01920000-0000-7000-8000-%012d", n) }

// Fixture artists: group ids are canonical UUIDv7s too.
const (
	fxArtist1 = contentref.GroupID("01930000-0000-7000-8000-000000000001")
	fxArtist2 = contentref.GroupID("01930000-0000-7000-8000-000000000002")
	fxArtist3 = contentref.GroupID("01930000-0000-7000-8000-000000000003")
	fxArtist4 = contentref.GroupID("01930000-0000-7000-8000-000000000004")
)

// record delivers explicit per-page observations through the real hub.
// Edition measurements project to the work without another recorded event.
// Delivery rounds retain their listed order and stable segment identities.
func (fx popularityFixture) record(t *testing.T, hub *scoringHub) {
	t.Helper()
	for round := 0; ; round++ {
		var batch []engagementtest.Observation
		for _, s := range fx.sessions {
			if round >= len(s.deliveries) {
				continue
			}
			d := s.deliveries[round]
			work := engagementtest.Observation{
				ContentRef: ref(s.work), Subject: s.subject, Type: engagementtest.TypeReading, EventID: s.viewID, Revision: d.revision, OccurredAt: s.at,
				DurationS: d.seconds, Progress: d.pages, ProgressMax: s.pageCount,
				Payload: map[string]any{"unique_pages_viewed": d.pages, "version_id": s.version},
			}
			version := work
			version.ContentRef = work.WithVersion(s.version)
			batch = append(batch, version)
		}
		if len(batch) == 0 {
			break
		}
		recordBatched(t, hub, batch)
	}
	// Votes are one replaceable event per subject × work × axis whose
	// revision is the mutation time; a mutation delivered twice is the same
	// signal twice.
	var prefs []engagementtest.Observation
	for _, v := range fx.votes {
		for i := 0; i < v.deliveries; i++ {
			prefs = append(prefs, engagementtest.Observation{ContentRef: ref(v.work), Subject: v.subject, Type: typeReact, EventID: "current",
				Revision: uint64(v.at.UTC().UnixMicro()), OccurredAt: v.at.UTC(), Value: v.value})
		}
	}
	recordBatched(t, hub, prefs)
}

func recordBatched(t *testing.T, hub *scoringHub, signals []engagementtest.Observation) {
	t.Helper()
	for start := 0; start < len(signals); start += signal.MaxSignalsPerBatch {
		if err := hub.RecordSignals(context.Background(), signals[start:min(start+signal.MaxSignalsPerBatch, len(signals))]); err != nil {
			t.Fatal(err)
		}
	}
}

type judgment struct {
	name  string
	check func(s map[string]float64) bool
}

func gt(a, b string) func(map[string]float64) bool {
	return func(s map[string]float64) bool { return s[a] > s[b] }
}
func eq(a, b string) func(map[string]float64) bool {
	return func(s map[string]float64) bool { return s[a] == s[b] && s[a] > 0 }
}
func liftUnder(a, b string, ratio float64) func(map[string]float64) bool {
	return func(s map[string]float64) bool { return s[a] > s[b] && s[a] <= s[b]*ratio }
}

var judgments = []judgment{
	{"one like helps a little", gt(fxOneLike, fxNoVotes)},
	{"one like cannot beat five more readers", gt(fxMoreReaders, fxOneLike)},
	{"one dislike hurts a little", gt(fxNoVotes, fxOneDislike)},
	{"one dislike hurts less than eight", gt(fxOneDislike, fxDisliked)},
	{"liked > mixed > disliked", func(s map[string]float64) bool { return s[fxLiked] > s[fxMixed] && s[fxMixed] > s[fxDisliked] }},
	{"one vote moves the score under 5%", func(s map[string]float64) bool {
		return math.Abs(s[fxOneLike]-s[fxNoVotes]) < 0.05*s[fxNoVotes] && math.Abs(s[fxOneDislike]-s[fxNoVotes]) < 0.05*s[fxNoVotes]
	}},
	{"75% of readers disliking outweighs double the reach", gt(fxLiked, fxBigDisliked)},
	{"approval is bounded: 120 readers half disliked beat 10 readers all liking", gt(fxCrowdDislike, fxTenLikes)},
	{"five readers beat one reader's fifty sessions", gt(fxFive, fxRepeat)},
	{"re-reads by the same readers help, under 15%", liftUnder(fxReturn, fxOnce, 1.15)},
	{"short gallery read fully beats long gallery skimmed", gt(fxShort, fxLongSkim)},
	{"page count itself is irrelevant", eq(fxShort, fxLongFull)},
	{"duplicate delivery changes nothing", eq(fxDup, fxSingle)},
	{"shifting observations inside the window changes nothing", eq(fxEarly, fxLate)},
	{"observations before the window do not rank", func(s map[string]float64) bool { _, ok := s[fxOutside]; return !ok }},
}

func lazyPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	// The content plane is never touched here; pgxpool connects lazily.
	pool, err := pgxpool.New(context.Background(), "postgres://unused:unused@127.0.0.1:9/unused")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// scoringHub records explicit measured observations through the real hub.
type scoringHub struct{ *ckhub.Hub }

func (h *scoringHub) RecordSignals(ctx context.Context, in []engagementtest.Observation) error {
	rows := engagementtest.Native(in)
	for start := 0; start < len(rows); start += signal.MaxSignalsPerBatch {
		if err := h.Hub.RecordSignals(ctx, rows[start:min(start+signal.MaxSignalsPerBatch, len(rows))]); err != nil {
			return err
		}
	}
	return nil
}
func newHub(t *testing.T, ch signal.Conn, database, tenant string, _ ...any) *scoringHub {
	t.Helper()
	hub, err := ckhub.New(ckhub.Config{PG: lazyPool(t), PGSchema: "hub", CH: ch, CHDatabase: database, Tenant: tenant})
	if err != nil {
		t.Fatal(err)
	}
	return &scoringHub{Hub: hub}
}

// metricsByID reads work-level metrics keyed by content id.
func metricsByID(t *testing.T, hub *scoringHub, ids []string, w signal.Window) map[string]signal.ContentMetrics {
	t.Helper()
	refs := make([]signal.ContentRef, 0, len(ids))
	for _, id := range ids {
		refs = append(refs, ref(id))
	}
	m, err := hub.Metrics(context.Background(), refs, w)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]signal.ContentMetrics{}
	for k, v := range m {
		out[k.ContentID] = v
	}
	return out
}

func scoresByID(hits []popularity.Hit) map[string]float64 {
	out := make(map[string]float64, len(hits))
	for _, h := range hits {
		out[h.ContentID] = h.Score
	}
	return out
}

// TestIntegrationPolicyFixture records the judged fixture through the real
// hub into ClickHouse, ranks it under the measured baseline and policy variants, and
// requires PolicyV1 to satisfy every judgment under viewer-weighted ranking.
// The ClickHouse RankExpr and the Go formula must agree; public counts are
// the raw metrics; windows are literal; a second tenant in the same database
// never leaks; group popularity derives through the Catalog port; cache
// entries are policy-named.
func TestIntegrationPolicyFixture(t *testing.T) {
	ctx := context.Background()
	env := signaltest.FromEnv(t)
	now := time.Now()
	fx := buildFixture(now)
	ids := fx.workIDs()

	v1Conn := env.Fresh(t, fxDB)
	v1Hub := newHub(t, v1Conn, fxDB, fxTenant, nil)
	otherHub := newHub(t, v1Conn, fxDB, "hentai0", nil)

	started := time.Now()
	fx.record(t, v1Hub)
	t.Logf("recorded %d measured visits and %d votes in %s", len(fx.sessions), len(fx.votes), time.Since(started).Round(time.Millisecond))
	// Another tenant reuses the no_votes id in the same database.
	var foreign []engagementtest.Observation
	for i := 0; i < 3; i++ {
		foreign = append(foreign, engagementtest.Observation{ContentRef: contentref.New("hentai0", fxKind, fxNoVotes), Subject: signal.Subject{UserID: fmt.Sprintf("h-%d", i)},
			Type: engagementtest.TypeReading, EventID: fmt.Sprintf("h-%d", i), OccurredAt: fx.window.From.AddDate(0, 0, 10), DurationS: 120, Progress: 10, ProgressMax: 10})
	}
	recordBatched(t, otherHub, foreign)

	v1Metrics := metricsByID(t, v1Hub, ids, fx.window)

	// Public counts are the metrics as recorded, whatever the ranking; work
	// consumption counts once across versions.
	counts := map[string][2]uint64{ // distinct qualified readers, measured completers
		fxNoVotes: {20, 0}, fxBigDisliked: {40, 0}, fxRepeat: {1, 1}, fxFive: {5, 5}, fxReturn: {10, 10}, fxOnce: {10, 10},
		fxShort: {20, 20}, fxLongSkim: {20, 0}, fxLongFull: {20, 20}, fxMulti: {13, 10}, fxDup: {10, 0}, fxSingle: {10, 0},
		fxEarly: {10, 10}, fxLate: {10, 10},
	}
	for id, want := range counts {
		m := v1Metrics[id]
		if m.Readers != want[0] || m.Completers != want[1] {
			t.Errorf("%s counts: readers=%d completers=%d, want %v", fxNames[id], m.Readers, m.Completers, want)
		}
	}
	if _, ok := v1Metrics[fxOutside]; ok {
		t.Errorf("before_window has metrics inside the window")
	}
	feedback := map[string][2]uint64{fxNoVotes: {0, 0}, fxOneLike: {1, 0}, fxMixed: {10, 10}, fxBigDisliked: {0, 30}, fxDup: {4, 0}, fxSingle: {4, 0}}
	for id, want := range feedback {
		m := v1Metrics[id]
		if m.PositiveSubjects != want[0] || m.NegativeSubjects != want[1] {
			t.Errorf("%s feedback: +%d -%d, want %v", fxNames[id], m.PositiveSubjects, m.NegativeSubjects, want)
		}
	}
	if a, b := v1Metrics[fxDup], v1Metrics[fxSingle]; a.ActiveMS != b.ActiveMS || a.ReaderQualitySum != b.ReaderQualitySum || a.ReturningReaders != b.ReturningReaders {
		t.Errorf("duplicate delivery changed metrics: %+v vs %+v", a, b)
	}
	// Feedback is the current preference per subject on the day it last
	// changed: unvotes drop out, changes count once at their final value, a
	// vote cast before the window counts only in windows that include its day.
	if m := v1Metrics[fxVoteChanges]; m.PositiveSubjects != 2 || m.NegativeSubjects != 1 {
		t.Errorf("vote changes in window: %+v", m)
	}
	if m := metricsByID(t, v1Hub, []string{fxVoteChanges}, signal.AllTime())[fxVoteChanges]; m.PositiveSubjects != 3 || m.NegativeSubjects != 1 {
		t.Errorf("vote changes all time: %+v", m)
	}
	// Coverage is measured against the selected version: the es half-readers
	// contribute quality 0.5 (6/12), not 0.6 (6/10). Same-day edition
	// changes do not turn a person into a returning reader.
	versions, err := v1Hub.Metrics(ctx, []signal.ContentRef{ref(fxMulti).WithVersion("v-en"), ref(fxMulti).WithVersion("v-es")}, fx.window)
	if err != nil {
		t.Fatal(err)
	}
	if en := versions[ref(fxMulti).WithVersion("v-en").Key()]; en.Readers != 10 || en.Completers != 10 {
		t.Errorf("en version metrics: %+v", en)
	}
	if es := versions[ref(fxMulti).WithVersion("v-es").Key()]; es.Readers != 8 || es.Completers != 5 {
		t.Errorf("es version metrics: %+v", es)
	}
	if multi := v1Metrics[fxMulti]; multi.ReaderQualitySum != 11.5 || multi.ReturningReaders != 0 {
		t.Errorf("work engagement across versions: %+v", multi)
	}

	chosen := map[string]float64{}
	for id, m := range v1Metrics {
		if m.Readers > 0 {
			chosen[id] = popularity.PolicyV1.Score(m)
		}
	}
	for _, judgment := range judgments {
		if !judgment.check(chosen) {
			t.Errorf("measured policy judgment failed: %s", judgment.name)
		}
	}
	ranked := make([]string, 0, len(chosen))
	for id := range chosen {
		ranked = append(ranked, id)
	}
	sort.Slice(ranked, func(i, j int) bool {
		if chosen[ranked[i]] != chosen[ranked[j]] {
			return chosen[ranked[i]] > chosen[ranked[j]]
		}
		return ranked[i] < ranked[j]
	})

	ranker, err := popularity.New(popularity.Config{Source: v1Hub, Policy: popularity.PolicyV1})
	if err != nil {
		t.Fatal(err)
	}
	global, err := ranker.Popular(ctx, fxKind, fx.window, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(global) != len(chosen) {
		t.Fatalf("global ranking has %d works, want %d", len(global), len(chosen))
	}
	for i, h := range global {
		want := chosen[h.ContentID]
		if math.Abs(h.Score-want) > 1e-9*math.Max(1, want) {
			t.Errorf("%s: ClickHouse rank %.12f, Go %.12f", fxNames[h.ContentID], h.Score, want)
		}
		if h.ContentID != ranked[i] {
			t.Errorf("global ranking position %d is %s, want %s", i, fxNames[h.ContentID], fxNames[ranked[i]])
		}
		if m := v1Metrics[h.ContentID]; h.Readers != m.Readers || h.MeanEngagement() != m.ReaderQualitySum/float64(m.Readers) {
			t.Errorf("%s public counts %+v differ from metrics %+v", fxNames[h.ContentID], h, m)
		}
	}
	byID := scoresByID(global)
	for _, pair := range [][2]string{{fxShort, fxLongFull}, {fxDup, fxSingle}, {fxEarly, fxLate}} {
		if byID[pair[0]] != byID[pair[1]] {
			t.Errorf("ClickHouse ranks %s and %s differently: %v vs %v", fxNames[pair[0]], fxNames[pair[1]], byID[pair[0]], byID[pair[1]])
		}
	}
	candidates, err := ranker.Candidates(ctx, fxKind, append(append([]string{}, ids...), fxOutside, " ", cid(987654)), fx.window)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != len(chosen) {
		t.Fatalf("candidate scoring has %d works, want %d", len(candidates), len(chosen))
	}
	for i, h := range candidates {
		if h.Score != chosen[h.ContentID] || h.ContentID != ranked[i] {
			t.Errorf("candidate %d = %s %v, want %s %v", i, fxNames[h.ContentID], h.Score, fxNames[ranked[i]], chosen[ranked[i]])
		}
	}

	// Windows are literal: 7d holds only today's readers, 30d everything but
	// before_window, the longer windows everything; identical observations
	// score exactly the same wherever they fall.
	for period, want := range map[string]int{"7d": 1, "30d": len(chosen), "90d": len(chosen) + 1, "365d": len(chosen) + 1, "all": len(chosen) + 1} {
		window, err := popularity.WindowForPeriod(period, now)
		if err != nil {
			t.Fatal(err)
		}
		hits, err := ranker.Popular(ctx, fxKind, window, 100)
		if err != nil {
			t.Fatal(err)
		}
		if len(hits) != want {
			t.Errorf("%s ranks %d works, want %d", period, len(hits), want)
		}
		s := scoresByID(hits)
		if period == "7d" && s[fxLate] == 0 {
			t.Errorf("7d must rank window_end: %v", s)
		}
		if want > len(chosen) && (s[fxOutside] != s[fxLate] || s[fxOutside] != s[fxEarly] || s[fxOutside] != s[fxOnce] || s[fxOutside] == 0) {
			t.Errorf("%s: position inside the window changed a score: %v %v %v %v", period, s[fxOutside], s[fxEarly], s[fxLate], s[fxOnce])
		}
	}

	// Tenant isolation: the other tenant's reuse of an id is invisible here
	// and ranks alone there.
	if m := v1Metrics[fxNoVotes]; m.Readers != 20 {
		t.Errorf("foreign tenant leaked into %s: %+v", fxNames[fxNoVotes], m)
	}
	otherRanker, err := popularity.New(popularity.Config{Source: otherHub, Policy: popularity.PolicyV1})
	if err != nil {
		t.Fatal(err)
	}
	other, err := otherRanker.Popular(ctx, fxKind, fx.window, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(other) != 1 || other[0].ContentID != fxNoVotes || other[0].Readers != 3 {
		t.Errorf("other tenant ranking: %+v", other)
	}

	// Group popularity derives through the Catalog join over the ranked
	// works; a record with no ranked member is absent; a work assigned twice
	// to one record counts once; no group row ever reaches the signal plane.
	var catalogCalls []string
	catalog := popularity.CatalogFunc(func(_ context.Context, tenant, contentKind, groupKind string, contentIDs []string) (map[string][]contentref.GroupID, error) {
		catalogCalls = append(catalogCalls, fmt.Sprintf("%s/%s/%s/%d", tenant, contentKind, groupKind, len(contentIDs)))
		return map[string][]contentref.GroupID{
			fxCrowdDislike: {fxArtist1}, fxFive: {fxArtist1}, fxShort: {fxArtist2, fxArtist4, fxArtist4}, fxLongFull: {fxArtist2}, fxOutside: {fxArtist3},
		}, nil
	})
	taxRanker, err := popularity.New(popularity.Config{Source: v1Hub, Policy: popularity.PolicyV1, Catalog: catalog})
	if err != nil {
		t.Fatal(err)
	}
	artists, err := taxRanker.Groups(ctx, fxKind, "artist", fx.window)
	if err != nil {
		t.Fatal(err)
	}
	wantArtists := []popularity.GroupHit{
		{GroupID: fxArtist2, GroupKind: "artist", Score: byID[fxShort] + byID[fxLongFull], ContentCount: 2, Readers: 40, MeanScore: (byID[fxShort] + byID[fxLongFull]) / 2},
		{GroupID: fxArtist1, GroupKind: "artist", Score: byID[fxCrowdDislike] + byID[fxFive], ContentCount: 2, Readers: 125, MeanScore: (byID[fxCrowdDislike] + byID[fxFive]) / 2},
		{GroupID: fxArtist4, GroupKind: "artist", Score: byID[fxShort], ContentCount: 1, Readers: 20, MeanScore: byID[fxShort]},
	}
	if fmt.Sprint(artists) != fmt.Sprint(wantArtists) {
		t.Errorf("artist ranking:\n got %+v\nwant %+v", artists, wantArtists)
	}
	if len(catalogCalls) != 1 || catalogCalls[0] != fmt.Sprintf("%s/%s/artist/%d", fxTenant, fxKind, len(chosen)) {
		t.Errorf("catalog calls %v", catalogCalls)
	}
	allTimeArtists, err := taxRanker.Groups(ctx, fxKind, "artist", signal.AllTime())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range allTimeArtists {
		if a.GroupID == fxArtist3 && a.ContentCount == 1 && a.Score > 0 {
			found = true
		}
	}
	if !found {
		t.Errorf("all-time artist ranking lacks a3: %+v", allTimeArtists)
	}
	bounded, err := popularity.New(popularity.Config{Source: v1Hub, Policy: popularity.PolicyV1, Catalog: catalog, GroupCandidateLimit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if top, err := bounded.Groups(ctx, fxKind, "artist", fx.window); err != nil || len(top) != 1 || top[0].GroupID != fxArtist1 || top[0].ContentCount != 1 {
		t.Errorf("candidate bound 1: %+v %v", top, err)
	}
	if _, err := ranker.Groups(ctx, fxKind, "artist", fx.window); err == nil {
		t.Error("group ranking without a Catalog must fail")
	}
	inventory, err := v1Hub.Inventory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range inventory {
		if row.ContentKind != fxKind {
			t.Errorf("signal plane holds a %q row: group popularity must not fan out", row.ContentKind)
		}
	}

	// The cache key names the policy: another policy sharing the cache never
	// reads v1's entry, and v1 reads its own back.
	cache := popularity.NewMemoryCache(0)
	cached, err := popularity.New(popularity.Config{Source: v1Hub, Policy: popularity.PolicyV1, Cache: cache})
	if err != nil {
		t.Fatal(err)
	}
	first, err := cached.Popular(ctx, fxKind, fx.window, 100)
	if err != nil {
		t.Fatal(err)
	}
	again, err := cached.Popular(ctx, fxKind, fx.window, 100)
	if err != nil || fmt.Sprint(again) != fmt.Sprint(first) {
		t.Fatalf("cached ranking differs: %v", err)
	}
	alt := popularity.PolicyV1
	alt.Name, alt.Weights.Approval = "v1-alt", 0
	altRanker, err := popularity.New(popularity.Config{Source: v1Hub, Policy: alt, Cache: cache})
	if err != nil {
		t.Fatal(err)
	}
	second, err := altRanker.Popular(ctx, fxKind, fx.window, 100)
	if err != nil {
		t.Fatal(err)
	}
	if scoresByID(second)[fxLiked] == scoresByID(first)[fxLiked] {
		t.Fatalf("policy %q was served policy v1's cached ranking", alt.Name)
	}
}
