package signal

import (
	"context"
	"fmt"
	"math/rand"
	"reflect"
	"sync"
	"testing"
	"time"
)

// deliveries is a multiset of source deliveries: superseded revisions, exact
// retries, a conflicting same-revision replay with a re-stamped time (also
// across a month partition), and replaceable feedback delivered out of order.
func deliveries(tenant string) []Signal {
	day := func(d, h int) time.Time { return time.Date(2026, 5, d, h, 0, 0, 0, time.UTC) }
	u1, u2, u3, a1 := Subject{UserID: "u1"}, Subject{UserID: "u2"}, Subject{UserID: "u3"}, Subject{AnonKey: "a1"}
	g := func(id string) ContentRef { return gallery(tenant, lid(id)) }
	var out []Signal
	for r := uint64(1); r <= 4; r++ {
		out = append(out, Signal{ContentRef: g("g1"), Subject: u1, Type: TypeView, EventID: "s1", Revision: r,
			OccurredAt: day(5, 10), DurationS: uint32(60 * r), Progress: uint32(2 * r), ProgressMax: 10,
			Score: int16(10 * r), Completed: r == 4, Resume: fmt.Sprintf("p:%d", 2*r)})
	}
	for r, v := range []float64{1, -1, 1, -1} {
		out = append(out, Signal{ContentRef: g("g1"), Subject: u1, Type: "reaction", EventID: "pref", Revision: uint64(r + 1),
			OccurredAt: day(5, 11), Value: v})
	}
	out = append(out,
		Signal{ContentRef: g("g1"), Subject: u2, Type: TypeView, EventID: "v2", OccurredAt: day(6, 10), DurationS: 30, Progress: 3, ProgressMax: 10, Score: 20},
		Signal{ContentRef: g("g1"), Subject: u2, Type: TypeView, EventID: "v2", OccurredAt: day(7, 9), DurationS: 30, Progress: 3, ProgressMax: 10, Score: 20},
		Signal{ContentRef: g("g1"), Subject: u2, Type: "click", EventID: "c2", OccurredAt: day(6, 10)},
		Signal{ContentRef: g("g2"), Subject: a1, Type: TypeView, EventID: "va", OccurredAt: time.Date(2026, 5, 31, 23, 0, 0, 0, time.UTC), Progress: 1, ProgressMax: 5, Score: 5},
		Signal{ContentRef: g("g2"), Subject: a1, Type: TypeView, EventID: "va", OccurredAt: time.Date(2026, 6, 1, 1, 0, 0, 0, time.UTC), Progress: 1, ProgressMax: 5, Score: 5},
		Signal{ContentRef: g("g2"), Subject: u3, Type: "like", EventID: "l3", OccurredAt: day(8, 8), Value: 1},
		Signal{ContentRef: g("g2"), Subject: u3, Type: TypeView, EventID: "v3", OccurredAt: day(8, 7), Progress: 5, ProgressMax: 5, Score: 90, Completed: true},
		Signal{ContentRef: g("g3"), Subject: u1, Type: TypeView, EventID: "v4", OccurredAt: day(9, 7), Progress: 2, ProgressMax: 4, Score: 30},
	)
	return out
}

// tenantless strips the tenant so snapshots of different tenants compare equal.
type storeSnapshot struct {
	States  map[string]map[string]State
	History map[string][]string
	Metrics map[string]map[string]ContentMetrics
	Popular map[string][]string
	CoEng   map[string]int64
}

func snapshot(t *testing.T, st *Store, tenant string) storeSnapshot {
	t.Helper()
	ctx := context.Background()
	subjects := []Subject{{UserID: "u1"}, {UserID: "u2"}, {UserID: "u3"}, {AnonKey: "a1"}}
	snap := storeSnapshot{States: map[string]map[string]State{}, History: map[string][]string{}, Metrics: map[string]map[string]ContentMetrics{}, Popular: map[string][]string{}, CoEng: map[string]int64{}}
	for _, s := range subjects {
		states, err := st.States(ctx, tenant, s, refs(tenant, lid("g1"), lid("g2"), lid("g3")))
		if err != nil {
			t.Fatal(err)
		}
		snap.States[s.Key()] = map[string]State{}
		for k, v := range states {
			snap.States[s.Key()][lname(k.ContentID)] = v
		}
		hist, err := st.History(ctx, tenant, s, HistoryOptions{})
		if err != nil {
			t.Fatal(err)
		}
		for _, h := range hist {
			snap.History[s.Key()] = append(snap.History[s.Key()], fmt.Sprintf("%s:%+v", lname(h.ContentID), h.State))
		}
	}
	for _, w := range []Window{AllTime(), Between(time.Date(2026, 5, 6, 0, 0, 0, 0, time.UTC), time.Date(2026, 5, 7, 0, 0, 0, 0, time.UTC)), LastDays(30, time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC))} {
		snap.Metrics[w.String()] = map[string]ContentMetrics{}
		for id, m := range metricsByID(t, st, tenant, []string{lid("g1"), lid("g2"), lid("g3")}, w) {
			snap.Metrics[w.String()][lname(id)] = m
		}
		hits, err := st.Popular(ctx, tenant, "gallery", PopularOptions{Window: w, Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		for _, h := range hits {
			snap.Popular[w.String()] = append(snap.Popular[w.String()], fmt.Sprintf("%s:%g:%+v", lname(h.ContentID), h.Score, h.ContentMetrics))
		}
	}
	co, err := st.CoEngaged(ctx, tenant, gallery(tenant, lid("g1")), CoEngagedOptions{SkipRollup: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range co {
		snap.CoEng[lname(h.ContentID)] = h.Strength
	}
	return snap
}

// #874: identical intended results for any delivery order, batching,
// duplication, merge state or projection rebuild.
func TestIntegrationDeliveriesConvergeAcrossOrderMergesAndRebuild(t *testing.T) {
	st, conn := freshStore(t)
	ctx := context.Background()
	for _, table := range []string{"signals", "subject_content_state", "subject_content_daily"} {
		if err := conn.Exec(ctx, "SYSTEM STOP MERGES "+testDB+"."+table); err != nil {
			t.Fatal(err)
		}
	}
	rng := rand.New(rand.NewSource(874))
	orders := map[string][]Signal{"ordered": deliveries("ordered")}
	base := deliveries("reversed")
	reversed := make([]Signal, len(base))
	for i := range base {
		reversed[len(base)-1-i] = base[i]
	}
	orders["reversed"] = reversed
	base = deliveries("chaos")
	chaos := append(append([]Signal{}, base...), base...)
	chaos = append(chaos, base[:5]...)
	rng.Shuffle(len(chaos), func(i, j int) { chaos[i], chaos[j] = chaos[j], chaos[i] })
	orders["chaos"] = chaos
	for tenant, sigs := range orders {
		batch := 1
		if tenant == "ordered" {
			batch = len(sigs)
		}
		for i := 0; i < len(sigs); {
			n := batch
			if tenant == "chaos" {
				n = 1 + rng.Intn(4)
			}
			end := min(i+n, len(sigs))
			if err := st.RecordSignals(ctx, tenant, sigs[i:end]); err != nil {
				t.Fatal(err)
			}
			i = end
		}
	}

	want := snapshot(t, st, "ordered")
	for _, tenant := range []string{"reversed", "chaos"} {
		if got := snapshot(t, st, tenant); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s diverged:\n got %+v\nwant %+v", tenant, got, want)
		}
	}

	// Intended semantics on the converged result.
	u1g1 := want.States["u1"]["g1"]
	if u1g1.TotalEvents != 2 || u1g1.Views != 1 || u1g1.ActiveS != 240 || u1g1.MaxProgress != 8 ||
		!u1g1.Completed || u1g1.Completions != 1 || u1g1.Resume != "p:8" || u1g1.LastScore != 40 ||
		u1g1.NetValue != -1 || u1g1.Feedback != 1 {
		t.Fatalf("u1/g1 must be revision 4 of s1 plus current preference -1: %+v", u1g1)
	}
	all := want.Metrics[AllTime().String()]
	if g1 := all["g1"]; g1.Viewers != 2 || g1.Views != 2 || g1.Events != 4 || g1.NegativeSubjects != 1 || g1.SignalCounts["click"] != 1 {
		t.Fatalf("g1 metrics: %+v", g1)
	}
	if g2 := all["g2"]; g2.Views != 2 || g2.Viewers != 2 || g2.Completers != 1 || g2.PositiveSubjects != 1 {
		t.Fatalf("a re-stamped replay across a month boundary must stay one view: %+v", g2)
	}

	for _, table := range []string{"signals", "subject_content_state", "subject_content_daily"} {
		if err := conn.Exec(ctx, "SYSTEM START MERGES "+testDB+"."+table); err != nil {
			t.Fatal(err)
		}
		if err := conn.Exec(ctx, "OPTIMIZE TABLE "+testDB+"."+table+" FINAL"); err != nil {
			t.Fatal(err)
		}
	}
	for tenant := range orders {
		if got := snapshot(t, st, tenant); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s changed after merges:\n got %+v\nwant %+v", tenant, got, want)
		}
		for _, table := range []string{"subject_content_state", "subject_content_daily"} {
			if err := conn.Exec(ctx, "DELETE FROM "+testDB+"."+table+" WHERE tenant = ?", tenant); err != nil {
				t.Fatal(err)
			}
		}
		for cursor := (*ProjectionKey)(nil); ; {
			res, err := st.RepairProjections(ctx, tenant, RepairOptions{Limit: 3, After: cursor})
			if err != nil {
				t.Fatal(err)
			}
			if cursor = res.Next; cursor == nil {
				break
			}
		}
		if got := snapshot(t, st, tenant); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s rebuilt from canonical signals diverged:\n got %+v\nwant %+v", tenant, got, want)
		}
	}
}

// #878: each named metric counts only what it names; a work's metrics never
// include its version rows, and versions are read by their own reference.
func TestIntegrationMetricsAreTruthful(t *testing.T) {
	st, _ := freshStore(t)
	ctx := context.Background()
	day := func(d int) time.Time { return time.Date(2026, 5, d, 9, 0, 0, 0, time.UTC) }
	w := gallery("t", lid("w"))
	edition := func(v string) ContentRef { return w.WithVersion(v) }
	var sigs []Signal
	session := func(sub Subject, id, version string, d int, completed bool) {
		base := Signal{Subject: sub, Type: TypeView, OccurredAt: day(d), DurationS: 100, Progress: 10, ProgressMax: 10, Score: 50, Completed: completed}
		work, ver := base, base
		work.ContentRef, work.EventID = w, id
		ver.ContentRef, ver.EventID = edition(version), id
		sigs = append(sigs, work, ver)
	}
	repeat := Subject{UserID: "repeat"}
	session(repeat, "r1", "ed-en", 1, true)
	session(repeat, "r2", "ed-en", 1, false)
	session(repeat, "r3", "ed-ja", 2, true)
	session(Subject{AnonKey: "guest"}, "g1", "ed-ja", 2, false)
	sigs = append(sigs,
		Signal{ContentRef: w, Subject: Subject{UserID: "clicker"}, Type: "click", EventID: "c", OccurredAt: day(1)},
		Signal{ContentRef: w, Subject: Subject{UserID: "fan"}, Type: "reaction", EventID: "pref", OccurredAt: day(1), Value: 1},
		Signal{ContentRef: w, Subject: Subject{UserID: "critic"}, Type: "reaction", EventID: "pref", OccurredAt: day(2), Value: -1},
	)
	// Every delivery arrives twice.
	if err := st.RecordSignals(ctx, "t", append(append([]Signal{}, sigs...), sigs...)); err != nil {
		t.Fatal(err)
	}
	m, err := st.Metrics(ctx, "t", []ContentRef{w, edition("ed-en"), edition("ed-ja")}, AllTime())
	if err != nil {
		t.Fatal(err)
	}
	want := ContentMetrics{
		Viewers: 2, UserViewers: 1, AnonViewers: 1, Views: 4, Completions: 2, Completers: 1, ActiveS: 400,
		ScoreSum: 200, ViewerEngagementSum: 1, ReturningViewers: 1, Events: 7, ValueSum: 0, PositiveSubjects: 1, NegativeSubjects: 1,
		SignalCounts: map[string]uint64{TypeView: 4, "click": 1, "reaction": 2},
	}
	if got := m[w.Key()]; !reflect.DeepEqual(got, want) {
		t.Fatalf("work metrics:\n got %+v\nwant %+v", got, want)
	}
	en, ja := m[edition("ed-en").Key()], m[edition("ed-ja").Key()]
	if en.Views != 2 || en.Viewers != 1 || en.ViewerEngagementSum != .5 || en.ReturningViewers != 1 || ja.Views != 2 || ja.Viewers != 2 || ja.ViewerEngagementSum != 1 || ja.ReturningViewers != 0 {
		t.Fatalf("version projections count separately from the work: %+v %+v", en, ja)
	}
	day2, err := st.Metrics(ctx, "t", []ContentRef{w}, Between(time.Date(2026, 5, 2, 0, 0, 0, 0, time.UTC), time.Date(2026, 5, 3, 0, 0, 0, 0, time.UTC)))
	if err != nil {
		t.Fatal(err)
	}
	if got := day2[w.Key()]; got.Viewers != 2 || got.Views != 2 || got.NegativeSubjects != 1 || got.PositiveSubjects != 0 || got.ViewerEngagementSum != 1 || got.ReturningViewers != 0 {
		t.Fatalf("day-2 window: %+v", got)
	}
	hits, err := st.Popular(ctx, "t", "gallery", PopularOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || !hits[0].Equal(w) || !reflect.DeepEqual(hits[0].ContentMetrics, want) {
		t.Fatalf("popular ranks works only, with the same metrics: %+v", hits)
	}
	clickOnly, err := st.Popular(ctx, "t", "gallery", PopularOptions{RankExpr: "toFloat64(events)"})
	if err != nil || len(clickOnly) != 1 {
		t.Fatalf("works without views never rank: %+v %v", clickOnly, err)
	}
}

// A revision that moves a session to another day zeroes the old day; a stale
// orphan day row is zeroed by repair; concurrent writers of one key converge.
func TestIntegrationProjectionVersionsAndOrphanDays(t *testing.T) {
	st, conn := freshStore(t)
	ctx := context.Background()
	sub := Subject{UserID: "u"}
	ref := gallery("t", lid("g"))
	d := func(day int) time.Time { return time.Date(2026, 5, day, 0, 0, 0, 0, time.UTC) }
	rev := func(r uint64, occurred time.Time) Signal {
		return Signal{ContentRef: ref, Subject: sub, Type: TypeView, EventID: "s", Revision: r, OccurredAt: occurred, DurationS: uint32(r), Progress: uint32(r), ProgressMax: 64}
	}
	viewsOn := func(day int) uint64 {
		t.Helper()
		return metricsByID(t, st, "t", []string{lid("g")}, Between(d(day), d(day+1)))[lid("g")].Views
	}
	if err := st.RecordSignals(ctx, "t", []Signal{rev(1, d(5).Add(time.Hour))}); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordSignals(ctx, "t", []Signal{rev(2, d(6).Add(time.Hour))}); err != nil {
		t.Fatal(err)
	}
	if viewsOn(5) != 0 || viewsOn(6) != 1 {
		t.Fatalf("moved session: day5=%d day6=%d", viewsOn(5), viewsOn(6))
	}

	// A stale nonzero day row left by an interrupted projection.
	if err := conn.Exec(ctx, `INSERT INTO `+testDB+`.subject_content_daily (tenant, content_kind, content_id, subject_kind, subject, day, events, views, completions, active_s, score_sum, value_sum, type_counts, version)
VALUES ('t', 'gallery', '`+lid("g")+`', 'user', 'u', '2026-05-03', 1, 1, 0, 0, 0, 0, map('view', 1), '2000-01-01 00:00:00')`); err != nil {
		t.Fatal(err)
	}
	if viewsOn(3) != 1 {
		t.Fatal("precondition: orphan row visible")
	}
	if _, err := st.RepairProjections(ctx, "t", RepairOptions{Rebuild: true}); err != nil {
		t.Fatal(err)
	}
	if viewsOn(3) != 0 || viewsOn(6) != 1 {
		t.Fatalf("repair must zero the orphan day: day3=%d day6=%d", viewsOn(3), viewsOn(6))
	}

	// Concurrent writers of revisions 3..34: the highest revision always wins.
	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for r := uint64(3); r < 35; r++ {
		wg.Add(1)
		go func(r uint64) {
			defer wg.Done()
			errs <- st.RecordSignals(ctx, "t", []Signal{rev(r, d(6).Add(time.Hour))})
		}(r)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	states, err := st.States(ctx, "t", sub, []ContentRef{ref})
	if err != nil {
		t.Fatal(err)
	}
	if s := states[ref.Key()]; s.MaxProgress != 34 || s.ActiveS != 34 || s.Views != 1 || s.TotalEvents != 1 {
		t.Fatalf("concurrent projections must converge on revision 34: %+v", s)
	}
	if res, err := st.RepairProjections(ctx, "t", RepairOptions{}); err != nil || res.Repaired != 0 {
		t.Fatalf("no projection may be left stale: %+v %v", res, err)
	}
}
