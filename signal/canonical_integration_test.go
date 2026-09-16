package signal

import (
	"context"
	"fmt"
	"math/rand"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/open-rails/searchkit/internal/signaltest"
)

// deliveries is a multiset of source deliveries: superseded revisions, exact
// retries, a conflicting same-revision replay with a re-stamped time (also
// across a month partition), and replaceable feedback delivered out of order.
func deliveries() []Signal {
	day := func(d, h int) time.Time { return time.Date(2026, 5, d, h, 0, 0, 0, time.UTC) }
	u1, u2, u3, a1 := Subject{UserID: "u1"}, Subject{UserID: "u2"}, Subject{UserID: "u3"}, Subject{AnonKey: "a1"}
	g := func(id string) EntityRef { return EntityRef{EntityType: "gallery", EntityID: id} }
	var out []Signal
	for r := uint64(1); r <= 4; r++ {
		out = append(out, Signal{EntityRef: g("g1"), Subject: u1, Type: TypeView, EventID: "s1", Revision: r,
			OccurredAt: day(5, 10), DurationS: uint32(60 * r), Progress: uint32(2 * r), ProgressMax: 10,
			Score: int16(10 * r), Completed: r == 4, Resume: fmt.Sprintf("p:%d", 2*r)})
	}
	for r, v := range []float64{1, -1, 1, -1} {
		out = append(out, Signal{EntityRef: g("g1"), Subject: u1, Type: "reaction", EventID: "pref", Revision: uint64(r + 1),
			OccurredAt: day(5, 11), Value: v})
	}
	out = append(out,
		Signal{EntityRef: g("g1"), Subject: u2, Type: TypeView, EventID: "v2", OccurredAt: day(6, 10), DurationS: 30, Progress: 3, ProgressMax: 10, Score: 20},
		Signal{EntityRef: g("g1"), Subject: u2, Type: TypeView, EventID: "v2", OccurredAt: day(7, 9), DurationS: 30, Progress: 3, ProgressMax: 10, Score: 20},
		Signal{EntityRef: g("g1"), Subject: u2, Type: "click", EventID: "c2", OccurredAt: day(6, 10)},
		Signal{EntityRef: g("g2"), Subject: a1, Type: TypeView, EventID: "va", OccurredAt: time.Date(2026, 5, 31, 23, 0, 0, 0, time.UTC), Progress: 1, ProgressMax: 5, Score: 5},
		Signal{EntityRef: g("g2"), Subject: a1, Type: TypeView, EventID: "va", OccurredAt: time.Date(2026, 6, 1, 1, 0, 0, 0, time.UTC), Progress: 1, ProgressMax: 5, Score: 5},
		Signal{EntityRef: g("g2"), Subject: u3, Type: "like", EventID: "l3", OccurredAt: day(8, 8), Value: 1},
		Signal{EntityRef: g("g2"), Subject: u3, Type: TypeView, EventID: "v3", OccurredAt: day(8, 7), Progress: 5, ProgressMax: 5, Score: 90, Completed: true},
		Signal{EntityRef: g("g3"), Subject: u1, Type: TypeView, EventID: "v4", OccurredAt: day(9, 7), Progress: 2, ProgressMax: 4, Score: 30},
	)
	return out
}

type storeSnapshot struct {
	States  map[string]map[EntityRef]State
	History map[string][]StateRow
	Metrics map[string]map[string]EntityMetrics
	Popular map[string][]PopularHit
	CoEng   []CoEngagedHit
}

func snapshot(t *testing.T, st *Store, tenant string) storeSnapshot {
	t.Helper()
	ctx := context.Background()
	subjects := []Subject{{UserID: "u1"}, {UserID: "u2"}, {UserID: "u3"}, {AnonKey: "a1"}}
	refs := []EntityRef{{EntityType: "gallery", EntityID: "g1"}, {EntityType: "gallery", EntityID: "g2"}, {EntityType: "gallery", EntityID: "g3"}}
	snap := storeSnapshot{States: map[string]map[EntityRef]State{}, History: map[string][]StateRow{}, Metrics: map[string]map[string]EntityMetrics{}, Popular: map[string][]PopularHit{}}
	for _, s := range subjects {
		states, err := st.States(ctx, tenant, s, refs)
		if err != nil {
			t.Fatal(err)
		}
		snap.States[s.Key()] = states
		hist, err := st.History(ctx, tenant, s, HistoryOptions{})
		if err != nil {
			t.Fatal(err)
		}
		snap.History[s.Key()] = hist
	}
	for _, w := range []Window{AllTime(), Between(time.Date(2026, 5, 6, 0, 0, 0, 0, time.UTC), time.Date(2026, 5, 7, 0, 0, 0, 0, time.UTC)), LastDays(30, time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC))} {
		m, err := st.Metrics(ctx, tenant, "gallery", []string{"g1", "g2", "g3"}, w)
		if err != nil {
			t.Fatal(err)
		}
		snap.Metrics[w.String()] = m
		hits, err := st.Popular(ctx, tenant, "gallery", PopularOptions{Window: w, Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		snap.Popular[w.String()] = hits
	}
	co, err := st.CoEngaged(ctx, tenant, EntityRef{EntityType: "gallery", EntityID: "g1"}, CoEngagedOptions{SkipRollup: true})
	if err != nil {
		t.Fatal(err)
	}
	snap.CoEng = co
	return snap
}

// #874: identical intended results for any delivery order, batching,
// duplication, merge state or projection rebuild.
func TestIntegrationDeliveriesConvergeAcrossOrderMergesAndRebuild(t *testing.T) {
	st, conn := freshStore(t)
	ctx := context.Background()
	for _, table := range []string{"events", "subject_state", "subject_daily"} {
		if err := conn.Exec(ctx, "SYSTEM STOP MERGES "+testDB+"."+table); err != nil {
			t.Fatal(err)
		}
	}
	base := deliveries()
	rng := rand.New(rand.NewSource(874))
	orders := map[string][]Signal{"ordered": base}
	reversed := make([]Signal, len(base))
	for i := range base {
		reversed[len(base)-1-i] = base[i]
	}
	orders["reversed"] = reversed
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
	u1g1 := want.States["u1"][EntityRef{EntityType: "gallery", EntityID: "g1"}]
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

	for _, table := range []string{"events", "subject_state", "subject_daily"} {
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
		for _, table := range []string{"subject_state", "subject_daily"} {
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
			t.Fatalf("%s rebuilt from canonical events diverged:\n got %+v\nwant %+v", tenant, got, want)
		}
	}
}

// #878: each named metric counts only what it names.
func TestIntegrationMetricsAreTruthful(t *testing.T) {
	st, _ := freshStore(t)
	ctx := context.Background()
	day := func(d int) time.Time { return time.Date(2026, 5, d, 9, 0, 0, 0, time.UTC) }
	work := func(id string) EntityRef { return EntityRef{EntityType: "gallery", EntityID: id} }
	edition := func(id string) EntityRef { return EntityRef{EntityType: "gallery_version", EntityID: id} }
	var sigs []Signal
	session := func(sub Subject, id, version string, d int, completed bool) {
		base := Signal{Subject: sub, Type: TypeView, OccurredAt: day(d), DurationS: 100, Progress: 10, ProgressMax: 10, Score: 50, Completed: completed}
		w, v := base, base
		w.EntityRef, w.EventID = work("w"), id
		v.EntityRef, v.EventID = edition(version), id+":gallery_version:"+version
		sigs = append(sigs, w, v)
	}
	repeat := Subject{UserID: "repeat"}
	session(repeat, "r1", "ed-en", 1, true)
	session(repeat, "r2", "ed-en", 1, false)
	session(repeat, "r3", "ed-ja", 2, true)
	session(Subject{AnonKey: "guest"}, "g1", "ed-ja", 2, false)
	sigs = append(sigs,
		Signal{EntityRef: work("w"), Subject: Subject{UserID: "clicker"}, Type: "click", EventID: "c", OccurredAt: day(1)},
		Signal{EntityRef: work("w"), Subject: Subject{UserID: "fan"}, Type: "reaction", EventID: "pref", OccurredAt: day(1), Value: 1},
		Signal{EntityRef: work("w"), Subject: Subject{UserID: "critic"}, Type: "reaction", EventID: "pref", OccurredAt: day(2), Value: -1},
	)
	// Every delivery arrives twice.
	if err := st.RecordSignals(ctx, "t", append(append([]Signal{}, sigs...), sigs...)); err != nil {
		t.Fatal(err)
	}
	m, err := st.Metrics(ctx, "t", "gallery", []string{"w"}, AllTime())
	if err != nil {
		t.Fatal(err)
	}
	want := EntityMetrics{
		Viewers: 2, UserViewers: 1, AnonViewers: 1, Views: 4, Completions: 2, Completers: 1, ActiveS: 400,
		ScoreSum: 200, Events: 7, ValueSum: 0, PositiveSubjects: 1, NegativeSubjects: 1,
		SignalCounts: map[string]uint64{TypeView: 4, "click": 1, "reaction": 2},
	}
	if got := m["w"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("work metrics:\n got %+v\nwant %+v", got, want)
	}
	ed, err := st.Metrics(ctx, "t", "gallery_version", []string{"ed-en", "ed-ja"}, AllTime())
	if err != nil {
		t.Fatal(err)
	}
	if ed["ed-en"].Views != 2 || ed["ed-en"].Viewers != 1 || ed["ed-ja"].Views != 2 || ed["ed-ja"].Viewers != 2 {
		t.Fatalf("version projections count separately from the work: %+v", ed)
	}
	day2, err := st.Metrics(ctx, "t", "gallery", []string{"w"}, Between(time.Date(2026, 5, 2, 0, 0, 0, 0, time.UTC), time.Date(2026, 5, 3, 0, 0, 0, 0, time.UTC)))
	if err != nil {
		t.Fatal(err)
	}
	if got := day2["w"]; got.Viewers != 2 || got.Views != 2 || got.NegativeSubjects != 1 || got.PositiveSubjects != 0 {
		t.Fatalf("day-2 window: %+v", got)
	}
	hits, err := st.Popular(ctx, "t", "gallery", PopularOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || !reflect.DeepEqual(hits[0].EntityMetrics, want) {
		t.Fatalf("popular must report the same metrics: %+v", hits)
	}
	clickOnly, err := st.Popular(ctx, "t", "gallery", PopularOptions{RankExpr: "toFloat64(events)"})
	if err != nil || len(clickOnly) != 1 {
		t.Fatalf("entities without views never rank: %+v %v", clickOnly, err)
	}
}

// A revision that moves a session to another day zeroes the old day; a stale
// orphan day row is zeroed by repair; concurrent writers of one key converge.
func TestIntegrationProjectionVersionsAndOrphanDays(t *testing.T) {
	st, conn := freshStore(t)
	ctx := context.Background()
	sub := Subject{UserID: "u"}
	ref := EntityRef{EntityType: "gallery", EntityID: "g"}
	d := func(day int) time.Time { return time.Date(2026, 5, day, 0, 0, 0, 0, time.UTC) }
	rev := func(r uint64, occurred time.Time) Signal {
		return Signal{EntityRef: ref, Subject: sub, Type: TypeView, EventID: "s", Revision: r, OccurredAt: occurred, DurationS: uint32(r), Progress: uint32(r), ProgressMax: 64}
	}
	viewsOn := func(day int) uint64 {
		t.Helper()
		m, err := st.Metrics(ctx, "t", "gallery", []string{"g"}, Between(d(day), d(day+1)))
		if err != nil {
			t.Fatal(err)
		}
		return m["g"].Views
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
	if err := conn.Exec(ctx, `INSERT INTO `+testDB+`.subject_daily (tenant, entity_type, entity_id, subject_kind, subject, day, events, views, completions, active_s, score_sum, value_sum, type_counts, version)
VALUES ('t', 'gallery', 'g', 'user', 'u', '2026-05-03', 1, 1, 0, 0, 0, 0, map('view', 1), '2000-01-01 00:00:00')`); err != nil {
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
	states, err := st.States(ctx, "t", sub, []EntityRef{ref})
	if err != nil {
		t.Fatal(err)
	}
	if s := states[ref]; s.MaxProgress != 34 || s.ActiveS != 34 || s.Views != 1 || s.TotalEvents != 1 {
		t.Fatalf("concurrent projections must converge on revision 34: %+v", s)
	}
	if res, err := st.RepairProjections(ctx, "t", RepairOptions{}); err != nil || res.Repaired != 0 {
		t.Fatalf("no projection may be left stale: %+v %v", res, err)
	}
}

// Rows written by the removed schema survive 0002 and rebuild into projections.
func TestIntegrationLegacyEventsMigrateIntoCanonicalProjections(t *testing.T) {
	env := signaltest.FromEnv(t)
	ctx := context.Background()
	conn := env.Empty(t, testDB)
	env.ApplyRange(t, conn, 0, 1)
	legacy := `INSERT INTO signal_events (tenant, entity_type, entity_id, subject_kind, subject, signal_type, event_id, occurred_at, progress, progress_max, value, label, weight, score, completed, resume, recorded_at) VALUES
('doujins', 'gallery', '7', 'user', 'u1', 'view', 'legacy-view', '2026-04-01 10:00:00', 5, 10, 0, '', 1, 40, false, 'p:5', '2026-04-01 10:00:01'),
('doujins', 'gallery', '7', 'user', 'u1', 'view', 'legacy-view', '2026-04-01 10:00:00', 5, 10, 0, '', 1, 40, false, 'p:5', '2026-04-01 10:00:02'),
('doujins', 'gallery', '7', 'user', 'u1', 'like', 'legacy-like', '2026-04-01 10:05:00', 0, 0, 1, 'like', 1, 0, false, '', '2026-04-01 10:05:01'),
('doujins', 'tag', '3', 'user', 'u1', 'view', 'legacy-view:tag:3', '2026-04-01 10:00:00', 0, 0, 0, '', 0.25, 0, false, '', '2026-04-01 10:00:01')`
	if err := conn.Exec(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	env.ApplyRange(t, conn, 1, len(env.Migrations(t)))
	if err := CheckSchema(ctx, conn, testDB); err != nil {
		t.Fatal(err)
	}
	st, err := NewStore(conn, testDB)
	if err != nil {
		t.Fatal(err)
	}
	res, err := st.RepairProjections(ctx, "doujins", RepairOptions{Rebuild: true})
	if err != nil || res.Repaired != 2 {
		t.Fatalf("rebuild legacy keys: %+v %v", res, err)
	}
	ref := EntityRef{EntityType: "gallery", EntityID: "7"}
	states, err := st.States(ctx, "doujins", Subject{UserID: "u1"}, []EntityRef{ref})
	if err != nil {
		t.Fatal(err)
	}
	if s := states[ref]; s.TotalEvents != 2 || s.Views != 1 || s.NetValue != 1 || s.Resume != "p:5" || s.LastScore != 40 {
		t.Fatalf("legacy state: %+v", s)
	}
	// Re-running the copy (a partially applied migration) must not add events.
	env.ApplyRange(t, conn, 1, 2)
	if _, err := st.RepairProjections(ctx, "doujins", RepairOptions{Rebuild: true}); err != nil {
		t.Fatal(err)
	}
	m, err := st.Metrics(ctx, "doujins", "gallery", []string{"7"}, AllTime())
	if err != nil || m["7"].Views != 1 || m["7"].Events != 2 {
		t.Fatalf("re-applied copy duplicated events: %+v %v", m, err)
	}
}
