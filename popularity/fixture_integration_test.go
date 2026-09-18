package popularity_test

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	contentkit "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/signaltest"
	"github.com/open-rails/contentkit/popularity"
	"github.com/open-rails/contentkit/signal"
)

const (
	fxTenant   = "doujins"
	fxKind     = "gallery"
	fxDB       = "contentkit_popularity_test"
	fxLegacyDB = "contentkit_popularity_legacy_test"
	typeReact  = "reaction"
)

// Fixture galleries (docs/popularity-policy.md lists them with the judgments).
const (
	fxNoVotes      = "101"
	fxOneLike      = "102"
	fxOneDislike   = "103"
	fxMixed        = "104"
	fxLiked        = "105"
	fxDisliked     = "106"
	fxMoreViewers  = "107"
	fxBigDisliked  = "108"
	fxCrowdDislike = "109"
	fxTenLikes     = "110"
	fxRepeat       = "111"
	fxFive         = "112"
	fxReturn       = "113"
	fxOnce         = "114"
	fxShort        = "121"
	fxLongSkim     = "122"
	fxLongFull     = "123"
	fxMulti        = "131"
	fxDup          = "141"
	fxSingle       = "142"
	fxEarly        = "151"
	fxLate         = "152"
	fxOutside      = "153"
	fxVoteChanges  = "161"
)

var fxNames = map[string]string{
	fxNoVotes: "no_votes", fxOneLike: "one_like", fxOneDislike: "one_dislike", fxMixed: "mixed", fxLiked: "liked",
	fxDisliked: "disliked", fxMoreViewers: "more_viewers", fxBigDisliked: "big_disliked", fxCrowdDislike: "crowd_disliked",
	fxTenLikes: "ten_likes", fxRepeat: "repeat_reader", fxFive: "five_readers", fxReturn: "return_readers", fxOnce: "once_readers",
	fxShort: "short_full", fxLongSkim: "long_skimmed", fxLongFull: "long_full", fxMulti: "two_versions", fxDup: "duplicate_delivery",
	fxSingle: "single_delivery", fxEarly: "window_start", fxLate: "window_end", fxOutside: "before_window", fxVoteChanges: "vote_changes",
}

// expectedOrder is the viewer-weighted v1 ranking of the fixture; a
// tier holds works with identical metrics that must score exactly equal.
var expectedOrder = [][]string{
	{fxCrowdDislike}, {fxShort, fxLongFull}, {fxMoreViewers}, {fxLiked}, {fxBigDisliked}, {fxOneLike}, {fxNoVotes},
	{fxVoteChanges}, {fxOneDislike}, {fxMixed}, {fxMulti}, {fxLongSkim}, {fxReturn}, {fxDisliked},
	{fxEarly, fxLate, fxOnce}, {fxTenLikes}, {fxDup, fxSingle}, {fxFive}, {fxRepeat},
}

// expectedScores are the viewer-weighted v1 scores, two decimals.
var expectedScores = map[string]float64{
	fxCrowdDislike: 2.70, fxShort: 2.34, fxLongFull: 2.34, fxMoreViewers: 2.21, fxLiked: 2.14, fxBigDisliked: 2.11,
	fxOneLike: 2.08, fxNoVotes: 2.06, fxOneDislike: 1.99, fxMixed: 1.95, fxMulti: 1.95, fxLongSkim: 1.86, fxReturn: 1.83,
	fxDisliked: 1.82, fxEarly: 1.78, fxLate: 1.78, fxOnce: 1.78, fxTenLikes: 1.68, fxDup: 1.66, fxSingle: 1.66,
	fxFive: 1.28, fxRepeat: 0.47,
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
	readers(fxMoreViewers, 25, 10, 8, 80, mid)
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

// record drives every delivery through the hub the way a host recorder does:
// the work view and the selected version's view with one event id. Delivery
// rounds keep each session's checkpoints in their listed order, so a batch
// never carries two checkpoints of one session.
func (fx popularityFixture) record(t *testing.T, hub contentkit.Hub) {
	t.Helper()
	for round := 0; ; round++ {
		var batch []signal.Signal
		for _, s := range fx.sessions {
			if round >= len(s.deliveries) {
				continue
			}
			d := s.deliveries[round]
			work := signal.Signal{
				ContentRef: ref(s.work), Subject: s.subject, Type: signal.TypeView, EventID: s.viewID, Revision: d.revision, OccurredAt: s.at,
				DurationS: d.seconds, Progress: d.pages, ProgressMax: s.pageCount,
				Payload: map[string]any{"unique_pages_viewed": d.pages, "version_id": s.version},
			}
			version := work
			version.ContentRef = work.WithVersion(s.version)
			batch = append(batch, work, version)
		}
		if len(batch) == 0 {
			break
		}
		recordBatched(t, hub, batch)
	}
	// Votes are one replaceable event per subject × work × axis whose
	// revision is the mutation time; a mutation delivered twice is the same
	// signal twice.
	var prefs []signal.Signal
	for _, v := range fx.votes {
		for i := 0; i < v.deliveries; i++ {
			prefs = append(prefs, signal.Signal{ContentRef: ref(v.work), Subject: v.subject, Type: typeReact, EventID: "current",
				Revision: uint64(v.at.UTC().UnixMicro()), OccurredAt: v.at.UTC(), Value: v.value})
		}
	}
	recordBatched(t, hub, prefs)
}

func recordBatched(t *testing.T, hub contentkit.Hub, signals []signal.Signal) {
	t.Helper()
	for start := 0; start < len(signals); start += signal.MaxSignalsPerBatch {
		if err := hub.RecordSignals(context.Background(), signals[start:min(start+signal.MaxSignalsPerBatch, len(signals))]); err != nil {
			t.Fatal(err)
		}
	}
}

// legacyScorer is the pre-#888 engagement formula the corrected baseline
// ranks over: -30 + unique_pages×5 + seconds/8, clamped to [-30, 100].
type legacyScorer struct{}

func (legacyScorer) Score(_ context.Context, s signal.Signal) (signal.Scored, error) {
	if s.Type != signal.TypeView {
		return signal.Scored{}, nil
	}
	pages := float64(s.Progress)
	if v, ok := s.Payload["unique_pages_viewed"].(uint32); ok {
		pages = float64(v)
	}
	score := math.Max(-30, math.Min(100, math.Round(-30+pages*5+float64(s.DurationS)/8)))
	return signal.Scored{Score: int16(score), Progress: s.Progress, ProgressMax: s.ProgressMax, Completed: s.ProgressMax > 0 && 10*pages >= 9*float64(s.ProgressMax)}, nil
}

// defaultRank is signal.RankWeights' default ranking, computed in Go.
func defaultRank(m signal.ContentMetrics) float64 {
	if m.Views == 0 {
		return 0
	}
	return math.Log10(1+float64(m.Viewers)) * math.Max(1, float64(m.ScoreSum)/(float64(m.Views)+10))
}

type rankingVariant struct {
	name   string
	legacy bool // metrics from the legacy-scored database
	score  func(signal.ContentMetrics) float64
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
	{"one like cannot beat five more viewers", gt(fxMoreViewers, fxOneLike)},
	{"one dislike hurts a little", gt(fxNoVotes, fxOneDislike)},
	{"one dislike hurts less than eight", gt(fxOneDislike, fxDisliked)},
	{"liked > mixed > disliked", func(s map[string]float64) bool { return s[fxLiked] > s[fxMixed] && s[fxMixed] > s[fxDisliked] }},
	{"one vote moves the score under 5%", func(s map[string]float64) bool {
		return math.Abs(s[fxOneLike]-s[fxNoVotes]) < 0.05*s[fxNoVotes] && math.Abs(s[fxOneDislike]-s[fxNoVotes]) < 0.05*s[fxNoVotes]
	}},
	{"75% of viewers disliking outweighs double the reach", gt(fxLiked, fxBigDisliked)},
	{"approval is bounded: 120 viewers half disliked beat 10 viewers all liking", gt(fxCrowdDislike, fxTenLikes)},
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

func newHub(t *testing.T, ch signal.Conn, database, tenant string, scorer signal.Scorer) *contentkit.EmbeddedHub {
	t.Helper()
	hub, err := contentkit.NewEmbedded(contentkit.EmbeddedConfig{
		PG: lazyPool(t), PGSchema: "hub", CH: ch, CHDatabase: database, Tenant: tenant,
		Scorers: map[string]signal.Scorer{fxKind: scorer},
	})
	if err != nil {
		t.Fatal(err)
	}
	return hub
}

// metricsByID reads work-level metrics keyed by content id.
func metricsByID(t *testing.T, hub contentkit.Hub, ids []string, w signal.Window) map[string]signal.ContentMetrics {
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
// hub into two ClickHouse databases (v1 session scorer and the legacy
// scorer), ranks it under the corrected baseline and the policy variants, and
// requires PolicyV1 to satisfy every judgment under viewer-weighted ranking.
// The ClickHouse RankExpr and the Go formula must agree; public counts are
// the raw metrics; windows are literal; a second tenant in the same database
// never leaks; taxonomy popularity derives through the Catalog port; cache
// entries are policy-named.
func TestIntegrationPolicyFixture(t *testing.T) {
	ctx := context.Background()
	env := signaltest.FromEnv(t)
	now := time.Now()
	fx := buildFixture(now)
	ids := fx.workIDs()

	v1Conn := env.Fresh(t, fxDB)
	v1Hub := newHub(t, v1Conn, fxDB, fxTenant, popularity.SessionScorer{SecondsPerUnit: 8, CoveredKey: "unique_pages_viewed"})
	legacyConn := env.Fresh(t, fxLegacyDB)
	legacyHub := newHub(t, legacyConn, fxLegacyDB, fxTenant, legacyScorer{})
	otherHub := newHub(t, v1Conn, fxDB, "hentai0", popularity.SessionScorer{SecondsPerUnit: 8})

	started := time.Now()
	fx.record(t, v1Hub)
	fx.record(t, legacyHub)
	t.Logf("recorded %d sessions and %d votes twice in %s", len(fx.sessions), len(fx.votes), time.Since(started).Round(time.Millisecond))
	// Another tenant reuses the no_votes id in the same database.
	var foreign []signal.Signal
	for i := 0; i < 3; i++ {
		foreign = append(foreign, signal.Signal{ContentRef: contentref.New("hentai0", fxKind, fxNoVotes), Subject: signal.Subject{UserID: fmt.Sprintf("h-%d", i)},
			Type: signal.TypeView, EventID: fmt.Sprintf("h-%d", i), OccurredAt: fx.window.From.AddDate(0, 0, 10), DurationS: 120, Progress: 10, ProgressMax: 10})
	}
	recordBatched(t, otherHub, foreign)

	v1Metrics := metricsByID(t, v1Hub, ids, fx.window)
	legacyMetrics := metricsByID(t, legacyHub, ids, fx.window)

	// Public counts are the metrics as recorded, whatever the ranking; work
	// consumption counts once across versions.
	counts := map[string][3]uint64{ // viewers, sessions, completers
		fxNoVotes: {20, 20, 0}, fxBigDisliked: {40, 40, 0}, fxRepeat: {1, 50, 1}, fxFive: {5, 5, 5}, fxReturn: {10, 20, 10}, fxOnce: {10, 10, 10},
		fxShort: {20, 20, 20}, fxLongSkim: {20, 20, 0}, fxLongFull: {20, 20, 20}, fxMulti: {13, 18, 10}, fxDup: {10, 10, 0}, fxSingle: {10, 10, 0},
		fxEarly: {10, 10, 10}, fxLate: {10, 10, 10},
	}
	for id, want := range counts {
		m := v1Metrics[id]
		if m.Viewers != want[0] || m.Views != want[1] || m.Completers != want[2] {
			t.Errorf("%s counts: viewers=%d views=%d completers=%d, want %v", fxNames[id], m.Viewers, m.Views, m.Completers, want)
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
	if a, b := v1Metrics[fxDup], v1Metrics[fxSingle]; a.ScoreSum != b.ScoreSum || a.ActiveS != b.ActiveS || a.Events != b.Events || a.ViewerEngagementSum != b.ViewerEngagementSum || a.ReturningViewers != b.ReturningViewers {
		t.Errorf("duplicate delivery changed metrics: %+v vs %+v", a, b)
	}
	// Feedback is the current preference per subject on the day it last
	// changed: unvotes drop out, changes count once at their final value, a
	// vote cast before the window counts only in windows that include its day.
	if m := v1Metrics[fxVoteChanges]; m.PositiveSubjects != 2 || m.NegativeSubjects != 1 || m.SignalCounts[typeReact] != 4 {
		t.Errorf("vote changes in window: %+v", m)
	}
	if m := metricsByID(t, v1Hub, []string{fxVoteChanges}, signal.AllTime())[fxVoteChanges]; m.PositiveSubjects != 3 || m.NegativeSubjects != 1 || m.SignalCounts[typeReact] != 5 {
		t.Errorf("vote changes all time: %+v", m)
	}
	// Coverage is measured against the selected version: the es half-readers
	// score 50 (6/12), never 60 (6/10); each version carries its own counts.
	versions, err := v1Hub.Metrics(ctx, []signal.ContentRef{ref(fxMulti).WithVersion("v-en"), ref(fxMulti).WithVersion("v-es")}, fx.window)
	if err != nil {
		t.Fatal(err)
	}
	if en := versions[ref(fxMulti).WithVersion("v-en").Key()]; en.Viewers != 10 || en.Completers != 10 || en.ScoreSum != 1000 {
		t.Errorf("en version metrics: %+v", en)
	}
	if es := versions[ref(fxMulti).WithVersion("v-es").Key()]; es.Viewers != 8 || es.Completers != 5 || es.ScoreSum != 5*100+3*50 {
		t.Errorf("es version metrics: %+v", es)
	}
	if multi := v1Metrics[fxMulti]; multi.ScoreSum != 10*100+5*100+3*50 || multi.ViewerEngagementSum != 11.5 || multi.ReturningViewers != 5 {
		t.Errorf("work engagement across versions: %+v", multi)
	}

	variants := []rankingVariant{
		{name: "baseline: default rank over legacy scorer", legacy: true, score: defaultRank},
		{name: "default rank over v1 scorer", score: defaultRank},
		{name: "v1 (additive, chosen)", score: popularity.PolicyV1.Score},
		{name: "D: multiplicative approval", score: func(m signal.ContentMetrics) float64 {
			p := popularity.PolicyV1
			p.Weights.Approval = 0
			if m.Viewers == 0 {
				return 0
			}
			positive, negative := float64(m.PositiveSubjects), float64(m.NegativeSubjects)
			approve := (positive + p.Priors.Approval*p.Priors.ApprovalWeight) / (positive + negative + p.Priors.ApprovalWeight)
			return p.Score(m) * approve / p.Priors.Approval
		}},
		{name: "E: approval 0.6, no revisit", score: func(m signal.ContentMetrics) float64 {
			p := popularity.PolicyV1
			p.Weights.Approval, p.Weights.Revisit = 0.6, 0
			return p.Score(m)
		}},
		{name: "F: flat priors (k=1)", score: func(m signal.ContentMetrics) float64 {
			p := popularity.PolicyV1
			p.Priors.ApprovalWeight, p.Priors.EngagementWeight, p.Priors.CompletionWeight = 1, 1, 1
			return p.Score(m)
		}},
	}
	var report strings.Builder
	fmt.Fprintf(&report, "\n%-72s", "judgment")
	for i := range variants {
		fmt.Fprintf(&report, " | %s", string(rune('A'+i)))
	}
	report.WriteString("\n")
	results := make([]map[string]float64, len(variants))
	for i, v := range variants {
		metrics := v1Metrics
		if v.legacy {
			metrics = legacyMetrics
		}
		results[i] = map[string]float64{}
		for id, m := range metrics {
			results[i][id] = v.score(m)
		}
	}
	failed := map[string][]string{}
	for _, j := range judgments {
		fmt.Fprintf(&report, "%-72s", j.name)
		for i, v := range variants {
			mark := "pass"
			if !j.check(results[i]) {
				mark = "FAIL"
				failed[v.name] = append(failed[v.name], j.name)
			}
			fmt.Fprintf(&report, " | %s", mark)
		}
		report.WriteString("\n")
	}
	for i, v := range variants {
		fmt.Fprintf(&report, "%s = %s (%d/%d)\n", string(rune('A'+i)), v.name, len(judgments)-len(failed[v.name]), len(judgments))
	}
	chosen := results[2]
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
	report.WriteString("\nv1 scores: ")
	for _, id := range ranked {
		fmt.Fprintf(&report, "%s=%.4f ", fxNames[id], chosen[id])
	}
	t.Log(report.String())
	if f := failed["v1 (additive, chosen)"]; len(f) > 0 {
		t.Fatalf("chosen policy fails judgments: %v", f)
	}
	if len(failed["baseline: default rank over legacy scorer"]) == 0 {
		t.Fatal("the baseline passes every judgment; the fixture no longer discriminates")
	}

	// The ranking is checked tier by tier independently of the former host policy.
	var wantOrder []string
	for _, tier := range expectedOrder {
		for _, id := range tier[1:] {
			if chosen[id] != chosen[tier[0]] {
				t.Errorf("%s and %s must score exactly equal: %v vs %v", fxNames[tier[0]], fxNames[id], chosen[tier[0]], chosen[id])
			}
		}
		tied := append([]string{}, tier...)
		sort.Strings(tied) // ties break on content id
		wantOrder = append(wantOrder, tied...)
	}
	if got := strings.Join(ranked, ","); got != strings.Join(wantOrder, ",") {
		t.Errorf("ranking differs from judged order:\n got %v\nwant %v", ranked, wantOrder)
	}
	for id, want := range expectedScores {
		if math.Round(chosen[id]*100)/100 != want {
			t.Errorf("%s scores %.4f, judged fixture expects %.2f", fxNames[id], chosen[id], want)
		}
	}

	// The ranker ranks through the ClickHouse expression (global) and the Go
	// formula (candidate sets); both must agree with the metrics, and public
	// counts must be the raw metrics.
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
		if m := v1Metrics[h.ContentID]; h.Viewers != m.Viewers || h.Views != m.Views || h.MeanEngagement() != float64(m.ScoreSum)/(100*float64(m.Views)) {
			t.Errorf("%s public counts %+v differ from metrics %+v", fxNames[h.ContentID], h, m)
		}
	}
	byID := scoresByID(global)
	for _, pair := range [][2]string{{fxShort, fxLongFull}, {fxDup, fxSingle}, {fxEarly, fxLate}} {
		if byID[pair[0]] != byID[pair[1]] {
			t.Errorf("ClickHouse ranks %s and %s differently: %v vs %v", fxNames[pair[0]], fxNames[pair[1]], byID[pair[0]], byID[pair[1]])
		}
	}
	candidates, err := ranker.Candidates(ctx, fxKind, append(append([]string{}, ids...), fxOutside, " ", "missing"), fx.window)
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
	if m := v1Metrics[fxNoVotes]; m.Viewers != 20 {
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
	if len(other) != 1 || other[0].ContentID != fxNoVotes || other[0].Viewers != 3 {
		t.Errorf("other tenant ranking: %+v", other)
	}

	// Taxonomy popularity derives through the Catalog join over the ranked
	// works; a record with no ranked member is absent; a work assigned twice
	// to one record counts once; no taxonomy row ever reaches the signal plane.
	var catalogCalls []string
	catalog := popularity.CatalogFunc(func(_ context.Context, tenant, contentKind, taxonomyKind string, contentIDs []string) (map[string][]contentref.TaxonomyID, error) {
		catalogCalls = append(catalogCalls, fmt.Sprintf("%s/%s/%s/%d", tenant, contentKind, taxonomyKind, len(contentIDs)))
		return map[string][]contentref.TaxonomyID{
			fxCrowdDislike: {"a1"}, fxFive: {"a1"}, fxShort: {"a2", "a4", "a4"}, fxLongFull: {"a2"}, fxOutside: {"a3"},
		}, nil
	})
	taxRanker, err := popularity.New(popularity.Config{Source: v1Hub, Policy: popularity.PolicyV1, Catalog: catalog})
	if err != nil {
		t.Fatal(err)
	}
	artists, err := taxRanker.Taxonomy(ctx, fxKind, "artist", fx.window)
	if err != nil {
		t.Fatal(err)
	}
	wantArtists := []popularity.TaxonomyHit{
		{TaxonomyID: "a2", TaxonomyKind: "artist", Score: byID[fxShort] + byID[fxLongFull], ContentCount: 2, Viewers: 40, MeanScore: (byID[fxShort] + byID[fxLongFull]) / 2},
		{TaxonomyID: "a1", TaxonomyKind: "artist", Score: byID[fxCrowdDislike] + byID[fxFive], ContentCount: 2, Viewers: 125, MeanScore: (byID[fxCrowdDislike] + byID[fxFive]) / 2},
		{TaxonomyID: "a4", TaxonomyKind: "artist", Score: byID[fxShort], ContentCount: 1, Viewers: 20, MeanScore: byID[fxShort]},
	}
	if fmt.Sprint(artists) != fmt.Sprint(wantArtists) {
		t.Errorf("artist ranking:\n got %+v\nwant %+v", artists, wantArtists)
	}
	if len(catalogCalls) != 1 || catalogCalls[0] != fmt.Sprintf("%s/%s/artist/%d", fxTenant, fxKind, len(chosen)) {
		t.Errorf("catalog calls %v", catalogCalls)
	}
	allTimeArtists, err := taxRanker.Taxonomy(ctx, fxKind, "artist", signal.AllTime())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range allTimeArtists {
		if a.TaxonomyID == "a3" && a.ContentCount == 1 && a.Score > 0 {
			found = true
		}
	}
	if !found {
		t.Errorf("all-time artist ranking lacks a3: %+v", allTimeArtists)
	}
	bounded, err := popularity.New(popularity.Config{Source: v1Hub, Policy: popularity.PolicyV1, Catalog: catalog, TaxonomyCandidateLimit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if top, err := bounded.Taxonomy(ctx, fxKind, "artist", fx.window); err != nil || len(top) != 1 || top[0].TaxonomyID != "a1" || top[0].ContentCount != 1 {
		t.Errorf("candidate bound 1: %+v %v", top, err)
	}
	if _, err := ranker.Taxonomy(ctx, fxKind, "artist", fx.window); err == nil {
		t.Error("taxonomy ranking without a Catalog must fail")
	}
	inventory, err := v1Hub.Inventory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range inventory {
		if row.ContentKind != fxKind {
			t.Errorf("signal plane holds a %q row: taxonomy popularity must not fan out", row.ContentKind)
		}
	}

	// The cache key names the policy: another policy sharing the cache never
	// reads v1's entry, and v1 reads its own back.
	cache := popularity.NewMemoryCache()
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
