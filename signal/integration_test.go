package signal

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/signaltest"
)

// Integration tests are opt-in: set CONTENTKIT_TEST_CH_ADDR to a Keeper-enabled
// ClickHouse native address (replicated engines); CONTENTKIT_TEST_CH_CLUSTER
// enables ON CLUSTER DDL. Optional: CONTENTKIT_TEST_CH_USER /
// CONTENTKIT_TEST_CH_PASSWORD. The tests own the database named below.
const testDB = "contentkit_signal_test"

func freshStore(t *testing.T) (*Store, Conn) {
	t.Helper()
	conn := signaltest.FromEnv(t).Fresh(t, testDB)
	st, err := NewStore(conn, testDB)
	if err != nil {
		t.Fatal(err)
	}
	return st, conn
}

func at(day, hour int) time.Time {
	return time.Date(2026, 5, day, hour, 0, 0, 0, time.UTC)
}

func gallery(tenant, id string) ContentRef { return contentref.New(tenant, "gallery", id) }

func view(tenant, contentID string, sub Subject, day, hour int, progress, progressMax uint32, score int16, completed bool) readingFixture {
	return readingFixture{
		ContentRef:  gallery(tenant, contentID),
		Subject:     sub,
		Type:        "view",
		OccurredAt:  at(day, hour),
		Progress:    progress,
		ProgressMax: progressMax,
		Score:       score,
		Completed:   completed,
		Resume:      fmt.Sprintf("p:%d", progress),
		EventID:     fmt.Sprintf("%s:%s:%d:%d", contentID, sub.Key(), day, hour),
	}
}

func refs(tenant string, ids ...string) []ContentRef {
	out := make([]ContentRef, 0, len(ids))
	for _, id := range ids {
		out = append(out, gallery(tenant, id))
	}
	return out
}

// metricsByID reads work-level gallery metrics keyed by content id.
func metricsByID(t *testing.T, st *Store, tenant string, ids []string, w Window) map[string]ContentMetrics {
	t.Helper()
	m, err := st.Metrics(context.Background(), tenant, refs(tenant, ids...), w)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]ContentMetrics{}
	for k, v := range m {
		out[k.ContentID] = v
	}
	return out
}

func TestIntegrationStateLifecycleAndReplay(t *testing.T) {
	g1 := cid(1)
	st, _ := freshStore(t)
	ctx := context.Background()
	tenant := "doujins"
	user := Subject{UserID: "u1"}
	ref := gallery(tenant, g1)

	// First session: page 5 of 20.
	if err := recordFixtureSignals(st, ctx, tenant, []readingFixture{view(tenant, g1, user, 1, 10, 5, 20, 30, false)}); err != nil {
		t.Fatal(err)
	}
	// Second session: page 19 of 20, completed.
	if err := recordFixtureSignals(st, ctx, tenant, []readingFixture{view(tenant, g1, user, 2, 11, 19, 20, 90, true)}); err != nil {
		t.Fatal(err)
	}
	// REPLAY of the second session (identical content -> same event_id).
	if err := recordFixtureSignals(st, ctx, tenant, []readingFixture{view(tenant, g1, user, 2, 11, 19, 20, 90, true)}); err != nil {
		t.Fatal(err)
	}

	states, err := st.States(ctx, tenant, user, []ContentRef{ref})
	if err != nil {
		t.Fatal(err)
	}
	s, ok := states[ref.Key()]
	if !ok {
		t.Fatalf("missing state: %v", states)
	}
	if s.ActiveMS != 24000 || s.Quality <= 0 || s.Quality > 1 || !s.Seen || s.MaxProgress != 1900 || s.ProgressMax != 2000 || !s.Completed {
		t.Fatalf("unexpected state: %+v", s)
	}
	if s.Resume != "p:19" {
		t.Fatalf("resume should be the latest pointer, got %q", s.Resume)
	}
	if !s.FirstSeenAt.Equal(at(1, 10)) || !s.LastSignalAt.Equal(at(2, 11)) {
		t.Fatalf("first/last: %v / %v", s.FirstSeenAt, s.LastSignalAt)
	}

	// A later partial re-read must not regress completed/max_progress.
	if err := recordFixtureSignals(st, ctx, tenant, []readingFixture{view(tenant, g1, user, 3, 9, 3, 20, 10, false)}); err != nil {
		t.Fatal(err)
	}
	states, err = st.States(ctx, tenant, user, []ContentRef{ref})
	if err != nil {
		t.Fatal(err)
	}
	s = states[ref.Key()]
	if !s.Completed || s.MaxProgress != 1900 {
		t.Fatalf("monotonic fields regressed: %+v", s)
	}
	if s.Resume != "p:3" {
		t.Fatalf("latest fields must follow the newest event: %+v", s)
	}

	// A version of the work has its own state; the work's is untouched.
	edition := ref.WithVersion("en-1")
	if err := recordFixtureSignals(st, ctx, tenant, []readingFixture{{ContentRef: edition, Subject: user, Type: TypeView, EventID: "ed", OccurredAt: at(4, 9), Progress: 2, ProgressMax: 20, Score: 5, Resume: "ed:2"}}); err != nil {
		t.Fatal(err)
	}
	states, err = st.States(ctx, tenant, user, []ContentRef{ref, edition})
	if err != nil {
		t.Fatal(err)
	}
	if states[edition.Key()].Resume != "ed:2" {
		t.Fatalf("version state must be separate: %+v", states)
	}
	hist, err := st.History(ctx, tenant, user, HistoryOptions{})
	if err != nil || len(hist) != 1 || hist[0].Version() != "" {
		t.Fatalf("history is work-level: %+v %v", hist, err)
	}
}

// insertRawSignal appends a canonical-table row WITHOUT projecting it — the
// residue of a crash between RecordSignals' insert and projection statements.
func insertRawSignal(t *testing.T, conn Conn, tenant, contentID string, sub Subject, eventID string, occurredAt time.Time, progress, progressMax uint32, score int16, completed bool, resume string) {
	t.Helper()
	event := readingFixture{ContentRef: gallery(tenant, contentID), Subject: sub, Type: TypeView, EventID: eventID, OccurredAt: occurredAt,
		Progress: progress, ProgressMax: progressMax, Score: score, Completed: completed, Resume: resume}.native()
	if err := insertFixtureSegment(context.Background(), conn, tenant, event); err != nil {
		t.Fatalf("insert raw measured segment %s: %v", eventID, err)
	}

}

func TestIntegrationRepairProjectionsHealsCrashResidue(t *testing.T) {
	g1, g2, g3 := cid(1), cid(2), cid(3)
	st, conn := freshStore(t)
	ctx := context.Background()
	tenant := "doujins"
	user := Subject{UserID: "u1"}

	// A crash residue can share the timestamp of an already-projected event.
	if err := conn.Exec(ctx, "ALTER TABLE "+testDB+".engagement_segments MODIFY COLUMN ingested_at DateTime64(6, 'UTC') DEFAULT toDateTime64('2026-05-01 00:00:00', 6, 'UTC')"); err != nil {
		t.Fatal(err)
	}

	if _, err := st.RepairProjections(ctx, "", RepairOptions{}); err == nil {
		t.Fatal("empty tenant must error")
	}
	if _, err := st.RepairProjections(ctx, tenant, RepairOptions{Window: Between(at(1, 3), at(2, 0))}); err == nil {
		t.Fatal("sub-day window must error")
	}

	// g1: projected at one event, then a second event lands without projection.
	if err := recordFixtureSignals(st, ctx, tenant, []readingFixture{view(tenant, g1, user, 1, 10, 5, 20, 30, false)}); err != nil {
		t.Fatal(err)
	}
	insertRawSignal(t, conn, tenant, g1, user, "g1-evt-2", at(2, 11), 19, 20, 90, true, "p:19")
	// g2: never projected at all.
	insertRawSignal(t, conn, tenant, g2, user, "g2-evt-1", at(3, 9), 7, 20, 40, false, "p:7")
	// g3: healthy, must not be rewritten.
	if err := recordFixtureSignals(st, ctx, tenant, []readingFixture{view(tenant, g3, user, 3, 10, 2, 20, 10, false)}); err != nil {
		t.Fatal(err)
	}

	ref1, ref2 := gallery(tenant, g1), gallery(tenant, g2)
	pre, err := st.States(ctx, tenant, user, []ContentRef{ref1, ref2})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := pre[ref2.Key()]; ok {
		t.Fatalf("precondition: g2 state must be missing: %+v", pre[ref2.Key()])
	}

	// Bounded pages: two keys, then one, then done.
	page1, err := st.RepairProjections(ctx, tenant, RepairOptions{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if page1.Examined != 2 || page1.Repaired != 2 || page1.Next == nil || page1.Next.ContentID != g2 || page1.Next.TenantID != tenant {
		t.Fatalf("page 1: %+v", page1)
	}
	page2, err := st.RepairProjections(ctx, tenant, RepairOptions{Limit: 2, After: page1.Next})
	if err != nil {
		t.Fatal(err)
	}
	if page2.Examined != 1 || page2.Repaired != 0 || page2.Next != nil {
		t.Fatalf("page 2 (healthy g3): %+v next=%v", page2, page2.Next)
	}

	post, err := st.States(ctx, tenant, user, []ContentRef{ref1, ref2})
	if err != nil {
		t.Fatal(err)
	}
	if s := post[ref1.Key()]; !s.LastSignalAt.Equal(at(2, 11)) || !s.Completed || s.Resume != "p:19" {
		t.Fatalf("g1 not healed: %+v", s)
	}
	if s, ok := post[ref2.Key()]; !ok || s.Resume != "p:7" {
		t.Fatalf("g2 not healed: %+v (ok=%v)", s, ok)
	}
	m := metricsByID(t, st, tenant, []string{g1, g2}, AllTime())
	if m[g1].Readers != 1 || m[g2].Readers != 1 {
		t.Fatalf("replayed/repaired visits must remain one person perwork: %+v", m)
	}

	res, err := st.RepairProjections(ctx, tenant, RepairOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Examined != 3 || res.Repaired != 0 {
		t.Fatalf("second sweep must be a no-op: %+v", res)
	}
	// Losing one day must be detected even when another day has the current generation.
	if err := conn.Exec(ctx, "DELETE FROM "+testDB+".engagement_daily WHERE tenant = ? AND content_id = ? AND day = toDate(?)", tenant, g1, at(1, 0)); err != nil {
		t.Fatal(err)
	}
	res, err = st.RepairProjections(ctx, tenant, RepairOptions{})
	if err != nil || res.Repaired != 1 {
		t.Fatalf("missing daily row: %+v %v", res, err)
	}
	res, err = st.RepairProjections(ctx, tenant, RepairOptions{Rebuild: true, Window: Between(at(3, 0), at(4, 0))})
	if err != nil {
		t.Fatal(err)
	}
	if res.Examined != 2 || res.Repaired != 2 {
		t.Fatalf("windowed rebuild must reproject g2 and g3 only: %+v", res)
	}
}

func TestIntegrationAnonVsUserSubjects(t *testing.T) {
	g1 := cid(1)
	st, _ := freshStore(t)
	ctx := context.Background()
	tenant := "t"
	user := Subject{UserID: "u1"}
	anon := Subject{AnonKey: "abc123"}

	if err := recordFixtureSignals(st, ctx, tenant, []readingFixture{view(tenant, g1, user, 1, 10, 5, 10, 50, false)}); err != nil {
		t.Fatal(err)
	}
	if err := recordFixtureSignals(st, ctx, tenant, []readingFixture{view(tenant, g1, anon, 1, 11, 10, 10, 80, true)}); err != nil {
		t.Fatal(err)
	}

	ref := gallery(tenant, g1)
	us, err := st.States(ctx, tenant, user, []ContentRef{ref})
	if err != nil {
		t.Fatal(err)
	}
	as, err := st.States(ctx, tenant, anon, []ContentRef{ref})
	if err != nil {
		t.Fatal(err)
	}
	if us[ref.Key()].MaxProgress != 500 || as[ref.Key()].MaxProgress != 1000 {
		t.Fatalf("subject isolation broken: user=%+v anon=%+v", us[ref.Key()], as[ref.Key()])
	}

	m1 := metricsByID(t, st, tenant, []string{g1}, AllTime())[g1]
	if m1.UserReaders != 1 || m1.GuestReaders != 1 || m1.Readers != 2 {
		t.Fatalf("metrics: %+v", m1)
	}
	if m1.Completers != 1 {
		t.Fatalf("metrics: %+v", m1)
	}
}

func TestIntegrationHistoryAndSeen(t *testing.T) {
	g1, g2, g3, b1 := cid(1), cid(2), cid(3), cid(4)
	st, _ := freshStore(t)
	ctx := context.Background()
	tenant := "t"
	user := Subject{UserID: "u1"}

	// g1 completed, g2 in progress, g3 zero progress (event but unseen),
	// blog b1 in progress.
	signals := []readingFixture{
		view(tenant, g1, user, 1, 10, 20, 20, 90, true),
		view(tenant, g2, user, 2, 10, 5, 20, 40, false),
		view(tenant, g3, user, 3, 10, 0, 20, 0, false),
		{
			ContentRef:  contentref.New(tenant, "blog_post", b1),
			Subject:     user,
			Type:        "view",
			OccurredAt:  at(4, 10),
			Progress:    50,
			ProgressMax: 100,
			Score:       30,
			EventID:     "b1-session",
		},
	}
	if err := recordFixtureSignals(st, ctx, tenant, signals); err != nil {
		t.Fatal(err)
	}

	// Full history, most recent first, across kinds.
	all, err := st.History(ctx, tenant, user, HistoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("history len=%d want 3: %+v", len(all), all)
	}
	if all[0].ContentID != b1 || all[2].ContentID != g1 || all[0].TenantID != tenant {
		t.Fatalf("history order wrong: %+v", all)
	}
	if !all[0].LastReadAt.Equal(at(4, 10)) || !all[2].LastReadAt.Equal(at(1, 10)) {
		t.Fatalf("last view times wrong: %+v", all)
	}

	// Galleries only, in progress.
	inProg, err := st.History(ctx, tenant, user, HistoryOptions{ContentKind: "gallery", Status: HistoryInProgress})
	if err != nil {
		t.Fatal(err)
	}
	if len(inProg) != 1 || inProg[0].ContentID != g2 {
		t.Fatalf("in-progress: %+v", inProg)
	}

	completed, err := st.History(ctx, tenant, user, HistoryOptions{ContentKind: "gallery", Status: HistoryCompleted})
	if err != nil {
		t.Fatal(err)
	}
	if len(completed) != 1 || completed[0].ContentID != g1 {
		t.Fatalf("completed: %+v", completed)
	}

	// Seen-set: g3 had progress 0 -> NOT seen.
	seen, err := st.SeenIDs(ctx, tenant, user, "gallery")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := seen[g1]; !ok {
		t.Fatalf("g1 must be seen: %v", seen)
	}
	if _, ok := seen[g2]; !ok {
		t.Fatalf("g2 must be seen: %v", seen)
	}
	if _, ok := seen[g3]; ok {
		t.Fatalf("g3 (zero progress) must not be seen: %v", seen)
	}

	// Later non-view activity affects the all-signals feed, but it must not
	// resurrect an item cleared from watch history or change watch ordering.
	click := readingFixture{
		ContentRef: gallery(tenant, g1),
		Subject:    user,
		Type:       TypeClick,
		EventID:    "g1-click",
		OccurredAt: at(5, 10),
	}
	if err := recordFixtureSignals(st, ctx, tenant, []readingFixture{click}); err != nil {
		t.Fatal(err)
	}
	all, err = st.History(ctx, tenant, user, HistoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if all[0].ContentID != b1 {
		t.Fatalf("clicks must not replace reading recency: %+v", all)
	}
	seenAfterClear, err := st.History(ctx, tenant, user, HistoryOptions{Status: HistorySeen, Since: at(2, 0)})
	if err != nil {
		t.Fatal(err)
	}
	if len(seenAfterClear) != 2 || seenAfterClear[0].ContentID != b1 || seenAfterClear[1].ContentID != g2 {
		t.Fatalf("watch history used non-view activity: %+v", seenAfterClear)
	}

	// TopStates orders by measured reading quality; the retired legacy score
	// (g2 40 > b1 30) cannot outrank b1's larger measured coverage.
	top, err := st.TopStates(ctx, tenant, user, TopStatesOptions{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(top) != 2 || top[0].ContentID != g1 || top[1].ContentID != b1 || top[0].Quality < top[1].Quality || top[1].Quality <= 0 {
		t.Fatalf("top states: %+v", top)
	}
}

func TestIntegrationPopular(t *testing.T) {
	gHot, gNiche, gMeh, gOld, missing := cid(11), cid(12), cid(13), cid(14), cid(99)
	st, _ := freshStore(t)
	ctx := context.Background()
	tenant := "t"

	// gHot: 10 subjects, good scores. gNiche: 1 subject, perfect score.
	// gMeh: 5 subjects, low scores. gOld: 8 subjects but outside the window.
	for i := 0; i < 10; i++ {
		sub := Subject{AnonKey: fmt.Sprintf("hot%d", i)}
		if err := recordFixtureSignals(st, ctx, tenant, []readingFixture{view(tenant, gHot, sub, 10, 8+i%4, 18, 20, 80, true)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := recordFixtureSignals(st, ctx, tenant, []readingFixture{view(tenant, gNiche, Subject{AnonKey: "n1"}, 10, 9, 20, 20, 100, true)}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		sub := Subject{AnonKey: fmt.Sprintf("meh%d", i)}
		if err := recordFixtureSignals(st, ctx, tenant, []readingFixture{view(tenant, gMeh, sub, 11, 8+i%4, 2, 20, 10, false)}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 8; i++ {
		sub := Subject{AnonKey: fmt.Sprintf("old%d", i)}
		if err := recordFixtureSignals(st, ctx, tenant, []readingFixture{view(tenant, gOld, sub, 1, 8+i%4, 18, 20, 90, true)}); err != nil {
			t.Fatal(err)
		}
	}

	// Literal window beginning on day 9 and ending at midnight on day 12.
	win := Between(time.Date(2026, 5, 9, 0, 0, 0, 0, time.UTC), time.Date(2026, 5, 12, 0, 0, 0, 0, time.UTC))
	hits, err := st.Popular(ctx, tenant, "gallery", PopularOptions{Window: win, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 3 {
		t.Fatalf("expected 3 in-window works, got %+v", hits)
	}
	if hits[0].ContentID != gHot || hits[0].TenantID != tenant || hits[0].ContentKind != "gallery" {
		t.Fatalf("gHot must rank first: %+v", hits)
	}
	// Bayesian prior: 1 perfect-score subject must not outrank 10 good ones.
	rank := map[string]int{}
	for i, h := range hits {
		rank[h.ContentID] = i
	}
	if rank[gNiche] < rank[gMeh] {
		// gNiche (1 subject) ranking above gMeh (5 subjects) is acceptable
		// only because gMeh's scores are terrible; but it must never beat gHot.
		if rank[gNiche] == 0 {
			t.Fatalf("tiny-sample work outranked high-volume: %+v", hits)
		}
	}
	if hits[0].Readers != 10 {
		t.Fatalf("gHot readers=%d want 10", hits[0].Readers)
	}

	// All-time includes gOld.
	all, err := st.Popular(ctx, tenant, "gallery", PopularOptions{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 4 {
		t.Fatalf("all-time should rank 4 works: %+v", all)
	}

	// Host rank expression: raw subject count.
	byVolume, err := st.Popular(ctx, tenant, "gallery", PopularOptions{
		RankExpr: "toFloat64(readers)",
		Limit:    10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if byVolume[0].ContentID != gHot || byVolume[1].ContentID != gOld {
		t.Fatalf("volume ranking: %+v", byVolume)
	}

	// PopularityFor scores only the requested candidates.
	scores, err := st.PopularityFor(ctx, tenant, "gallery", []string{gHot, gNiche, missing}, AllTime())
	if err != nil {
		t.Fatal(err)
	}
	if len(scores) != 2 || scores[gHot] <= 0 {
		t.Fatalf("popularity-for: %v", scores)
	}
}

func TestIntegrationCoEngaged(t *testing.T) {
	X, Y, Z := cid(21), cid(22), cid(23)
	st, _ := freshStore(t)
	ctx := context.Background()
	tenant := "t"

	// s1, s2 engage X and Y; s3 engages X and Z; s4 engages only Y.
	pairs := []struct {
		sub string
		ids []string
	}{
		{"s1", []string{X, Y}},
		{"s2", []string{X, Y}},
		{"s3", []string{X, Z}},
		{"s4", []string{Y}},
	}
	hour := 0
	for _, p := range pairs {
		for _, id := range p.ids {
			hour++
			if err := recordFixtureSignals(st, ctx, tenant, []readingFixture{view(tenant, id, Subject{UserID: p.sub}, 1, 8+hour%10, 10, 10, 50, true)}); err != nil {
				t.Fatal(err)
			}
		}
	}

	co, err := st.CoEngaged(ctx, tenant, gallery(tenant, X), CoEngagedOptions{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(co) != 2 {
		t.Fatalf("co-engaged: %+v", co)
	}
	if co[0].ContentID != Y || co[0].Strength != 2 || co[0].TenantID != tenant {
		t.Fatalf("Y should lead with strength 2: %+v", co)
	}
	if co[1].ContentID != Z || co[1].Strength != 1 {
		t.Fatalf("Z should follow with strength 1: %+v", co)
	}
}

func TestIntegrationCoEngagedReaderSample(t *testing.T) {
	st, conn := freshStore(t)
	ctx := t.Context()
	anchor := cid(80)
	var kinds, keys, ids []string
	var subjects []Subject
	for i := range 24 {
		key := fmt.Sprintf("reader-%02d", i/2)
		subject := Subject{UserID: key}
		if i%2 != 0 {
			subject = Subject{AnonKey: key}
		}
		subjects = append(subjects, subject)
		kinds, keys, ids = append(kinds, subject.Kind()), append(keys, subject.Key()), append(ids, cid(100+i))
	}
	// The expected sample comes from the input identities, not receipt order,
	// duplicate editions, or the query's aggregation pipeline.
	var ordered []string
	rows, err := conn.Query(ctx, `SELECT identity.3 FROM
 (SELECT arrayJoin(arrayZip(?,?,?)) AS identity)
 ORDER BY cityHash64(identity.1,identity.2),identity.1,identity.2`, kinds, keys, ids)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		ordered = append(ordered, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for _, tenant := range []string{"forward", "reverse"} {
		var events []readingFixture
		for i, subject := range subjects {
			first := view(tenant, anchor, subject, 1, 10, 1, 2, 0, false)
			edition := first
			edition.ContentRef = edition.ContentRef.WithVersion("second-edition")
			events = append(events, first, edition, view(tenant, ids[i], subject, 1, 10, 1, 2, 0, false))
		}
		if tenant == "reverse" {
			slices.Reverse(events)
		}
		if err := recordFixtureSignals(st, ctx, tenant, events); err != nil {
			t.Fatal(err)
		}
		for _, cap := range []int{1, 7, 24, 0} {
			n := cap
			if n == 0 {
				n = len(subjects)
			}
			want := slices.Clone(ordered[:n])
			slices.Sort(want)
			for _, threads := range []int{1, 4} {
				call := clickhouse.Context(ctx, clickhouse.WithSettings(clickhouse.Settings{"max_threads": threads}))
				hits, err := st.CoEngaged(call, tenant, gallery(tenant, anchor), CoEngagedOptions{MaxSubjects: cap})
				if err != nil {
					t.Fatal(err)
				}
				var got []string
				for _, hit := range hits {
					if hit.Strength != 1 || hit.TenantID != tenant {
						t.Fatalf("reader kind, tenant or edition isolation failed: %+v", hit)
					}
					got = append(got, hit.ContentID)
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("%s cap=%d threads=%d: %v, want %v", tenant, cap, threads, got, want)
				}
			}
		}
	}
	for _, tc := range []struct {
		tenant, anchor string
		kinds          []string
	}{
		{"forward", anchor, []string{"video"}},
		{"forward", cid(999), nil},
		{"missing", anchor, nil},
	} {
		hits, err := st.CoEngaged(ctx, tc.tenant, gallery(tc.tenant, tc.anchor), CoEngagedOptions{ContentKinds: tc.kinds})
		if err != nil || len(hits) != 0 {
			t.Fatalf("empty selection %+v: %+v, %v", tc, hits, err)
		}
	}
}

func TestIntegrationNegativeSignalsAndContentPairs(t *testing.T) {
	A, B, C := cid(31), cid(32), cid(33)
	_, conn := freshStore(t)
	ctx := context.Background()
	tenant := "t"
	// Fix the snapshot clock so identical rebuilds exercise insert deduplication.
	gate := newGate(conn, "INSERT INTO "+testDB+".engagement_pairs")
	close(gate.release)
	gate.dispatch = func(ctx context.Context, query string, args ...any) error {
		return conn.Exec(ctx, strings.ReplaceAll(query, "now()", "toDateTime(0)"), args...)
	}
	st, err := NewStore(gate, testDB)
	if err != nil {
		t.Fatal(err)
	}

	react := func(sub Subject, id string, kind string, value float64, day int) readingFixture {
		return readingFixture{
			ContentRef: gallery(tenant, id),
			Subject:    sub,
			Type:       kind,
			OccurredAt: at(day, 12),
			Value:      value,
			EventID:    kind + ":" + id + ":" + sub.Key(),
		}
	}

	u1, u2, u3 := Subject{UserID: "u1"}, Subject{UserID: "u2"}, Subject{UserID: "u3"}
	read := func(sub Subject, id string, day int) readingFixture {
		return view(tenant, id, sub, day, 10, 10, 10, 0, true)
	}
	signals := []readingFixture{
		// u1 reads and likes A and B; u2 reads and likes A and C; u3 reads A and B
		// but DISLIKES B. Co-engagement counts measured readers, netted by feedback.
		read(u1, A, 1), read(u1, B, 1), read(u2, A, 2), read(u2, C, 2), read(u3, A, 3), read(u3, B, 3),
		react(u1, A, "like", 1, 1), react(u1, B, "like", 1, 1),
		react(u2, A, "like", 1, 2), react(u2, C, "like", 1, 2),
		react(u3, A, "like", 1, 3), react(u3, B, "dislike", -1, 3),
	}
	if err := recordFixtureSignals(st, ctx, tenant, signals); err != nil {
		t.Fatal(err)
	}

	// State carries net sentiment.
	b := gallery(tenant, B)
	states, err := st.States(ctx, tenant, u3, []ContentRef{b})
	if err != nil {
		t.Fatal(err)
	}
	if got := states[b.Key()].NetValue; got >= 0 {
		t.Fatalf("disliked B must have negative NetValue, got %v", got)
	}

	// NegativeIDs returns the dislike set.
	neg, err := st.NegativeIDs(ctx, tenant, u3, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := neg[b.Key()]; !ok || len(neg) != 1 {
		t.Fatalf("negative ids: %v", neg)
	}

	// TopStates with ExcludeNegative drops B for u3.
	top, err := st.TopStates(ctx, tenant, u3, TopStatesOptions{ExcludeNegative: true, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range top {
		if r.ContentID == B {
			t.Fatalf("disliked work must not seed: %+v", top)
		}
	}

	// Query-time co-engagement from anchor A: B has 1 positive co-subject
	// (u1) and 1 negative (u3) -> net 0 -> excluded; C has 1.
	co, err := st.CoEngaged(ctx, tenant, gallery(tenant, A), CoEngagedOptions{Limit: 10, SkipRollup: true})
	if err != nil {
		t.Fatal(err)
	}
	strengthOf := func(hits []CoEngagedHit, id string) int64 {
		for _, h := range hits {
			if h.ContentID == id {
				return h.Strength
			}
		}
		return 0
	}
	if strengthOf(co, B) != 0 {
		t.Fatalf("B net strength should be 0 (excluded): %+v", co)
	}
	if strengthOf(co, C) != 1 {
		t.Fatalf("C net strength should be 1: %+v", co)
	}

	// Rollup path: refresh engagement_pairs and read through it.
	if err := st.RefreshCoEngagement(ctx, tenant, RefreshCoEngagementOptions{}); err != nil {
		t.Fatal(err)
	}
	coR, err := st.CoEngaged(ctx, tenant, gallery(tenant, A), CoEngagedOptions{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if strengthOf(coR, C) != 1 {
		t.Fatalf("rollup co-engagement for C should be 1: %+v", coR)
	}
	for _, h := range coR {
		if h.ContentID == B && h.Strength > 0 {
			t.Fatalf("rollup must net out the dislike on B: %+v", coR)
		}
	}

	// Refresh is idempotent (DELETE + INSERT).
	storedPairs := countWhere(t, conn, "engagement_pairs", "tenant = ?", tenant)
	if storedPairs == 0 {
		t.Fatal("first refresh did not store any pairs")
	}
	if err := st.RefreshCoEngagement(ctx, tenant, RefreshCoEngagementOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := countWhere(t, conn, "engagement_pairs", "tenant = ?", tenant); got != storedPairs {
		t.Fatalf("identical refresh lost stored pairs: got %d, want %d", got, storedPairs)
	}
	coR2, err := st.CoEngaged(ctx, tenant, gallery(tenant, A), CoEngagedOptions{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(coR2) != len(coR) {
		t.Fatalf("refresh not idempotent: %d vs %d", len(coR2), len(coR))
	}
}
