package signal

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/open-rails/searchkit/internal/signaltest"
)

// Integration tests are opt-in: set SEARCHKIT_TEST_CH_ADDR to a Keeper-enabled
// ClickHouse native address (replicated engines); SEARCHKIT_TEST_CH_CLUSTER
// enables ON CLUSTER DDL. Optional: SEARCHKIT_TEST_CH_USER /
// SEARCHKIT_TEST_CH_PASSWORD. The tests own the database named below.
const testDB = "searchkit_signal_test"

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

func view(entityID string, sub Subject, day, hour int, progress, progressMax uint32, score int16, completed bool) Signal {
	return Signal{
		EntityRef:   EntityRef{EntityType: "gallery", EntityID: entityID},
		Subject:     sub,
		Type:        "view",
		OccurredAt:  at(day, hour),
		Progress:    progress,
		ProgressMax: progressMax,
		Score:       score,
		Completed:   completed,
		Resume:      fmt.Sprintf("p:%d", progress),
		EventID:     fmt.Sprintf("%s:%s:%d:%d", entityID, sub.Key(), day, hour),
	}
}

func TestIntegrationStateLifecycleAndReplay(t *testing.T) {
	st, _ := freshStore(t)
	ctx := context.Background()
	tenant := "doujins"
	user := Subject{UserID: "u1"}
	ref := EntityRef{EntityType: "gallery", EntityID: "g1"}

	// First session: page 5 of 20.
	if err := st.RecordSignals(ctx, tenant, []Signal{view("g1", user, 1, 10, 5, 20, 30, false)}); err != nil {
		t.Fatal(err)
	}
	// Second session: page 19 of 20, completed.
	if err := st.RecordSignals(ctx, tenant, []Signal{view("g1", user, 2, 11, 19, 20, 90, true)}); err != nil {
		t.Fatal(err)
	}
	// REPLAY of the second session (identical content -> same event_id).
	if err := st.RecordSignals(ctx, tenant, []Signal{view("g1", user, 2, 11, 19, 20, 90, true)}); err != nil {
		t.Fatal(err)
	}

	states, err := st.States(ctx, tenant, user, []EntityRef{ref})
	if err != nil {
		t.Fatal(err)
	}
	s, ok := states[ref]
	if !ok {
		t.Fatalf("missing state: %v", states)
	}
	if s.TotalEvents != 2 {
		t.Fatalf("replay must not double-count: total_events=%d want 2", s.TotalEvents)
	}
	if !s.Seen || s.MaxProgress != 19 || s.ProgressMax != 20 || !s.Completed {
		t.Fatalf("unexpected state: %+v", s)
	}
	if s.Resume != "p:19" {
		t.Fatalf("resume should be the latest pointer, got %q", s.Resume)
	}
	if s.LastScore != 90 {
		t.Fatalf("last_score=%d want 90", s.LastScore)
	}
	if !s.FirstSeenAt.Equal(at(1, 10)) || !s.LastSignalAt.Equal(at(2, 11)) {
		t.Fatalf("first/last: %v / %v", s.FirstSeenAt, s.LastSignalAt)
	}

	// A later partial re-read must not regress completed/max_progress.
	if err := st.RecordSignals(ctx, tenant, []Signal{view("g1", user, 3, 9, 3, 20, 10, false)}); err != nil {
		t.Fatal(err)
	}
	states, err = st.States(ctx, tenant, user, []EntityRef{ref})
	if err != nil {
		t.Fatal(err)
	}
	s = states[ref]
	if !s.Completed || s.MaxProgress != 19 || s.TotalEvents != 3 {
		t.Fatalf("monotonic fields regressed: %+v", s)
	}
	if s.Resume != "p:3" || s.LastScore != 10 {
		t.Fatalf("latest fields must follow the newest event: %+v", s)
	}
}

// insertRawEvent appends a canonical-table row WITHOUT projecting it — the
// residue of a crash between RecordSignals' insert and projection statements.
func insertRawEvent(t *testing.T, conn Conn, tenant, entityID string, sub Subject, eventID string, occurredAt time.Time, progress, progressMax uint32, score int16, completed bool, resume string) {
	t.Helper()
	q := fmt.Sprintf(`INSERT INTO %s.events
(tenant, entity_type, entity_id, subject_kind, subject, signal_type, event_id, occurred_at, progress, progress_max, score, completed, resume)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, testDB)
	if err := conn.Exec(context.Background(), q,
		tenant, "gallery", entityID, sub.Kind(), sub.Key(), TypeView, eventID, occurredAt.UTC(),
		progress, progressMax, score, completed, resume,
	); err != nil {
		t.Fatalf("insert raw event %s: %v", eventID, err)
	}
}

func TestIntegrationRepairProjectionsHealsCrashResidue(t *testing.T) {
	st, conn := freshStore(t)
	ctx := context.Background()
	tenant := "doujins"
	user := Subject{UserID: "u1"}

	if _, err := st.RepairProjections(ctx, "", RepairOptions{}); err == nil {
		t.Fatal("empty tenant must error")
	}
	if _, err := st.RepairProjections(ctx, tenant, RepairOptions{Window: Between(at(1, 3), at(2, 0))}); err == nil {
		t.Fatal("sub-day window must error")
	}

	// g1: projected at one event, then a second event lands without projection.
	if err := st.RecordSignals(ctx, tenant, []Signal{view("g1", user, 1, 10, 5, 20, 30, false)}); err != nil {
		t.Fatal(err)
	}
	insertRawEvent(t, conn, tenant, "g1", user, "g1-evt-2", at(2, 11), 19, 20, 90, true, "p:19")
	// g2: never projected at all.
	insertRawEvent(t, conn, tenant, "g2", user, "g2-evt-1", at(3, 9), 7, 20, 40, false, "p:7")
	// g3: healthy, must not be rewritten.
	if err := st.RecordSignals(ctx, tenant, []Signal{view("g3", user, 3, 10, 2, 20, 10, false)}); err != nil {
		t.Fatal(err)
	}

	ref1, ref2 := EntityRef{EntityType: "gallery", EntityID: "g1"}, EntityRef{EntityType: "gallery", EntityID: "g2"}
	pre, err := st.States(ctx, tenant, user, []EntityRef{ref1, ref2})
	if err != nil {
		t.Fatal(err)
	}
	if pre[ref1].TotalEvents != 1 {
		t.Fatalf("precondition: g1 stale state %+v", pre[ref1])
	}
	if _, ok := pre[ref2]; ok {
		t.Fatalf("precondition: g2 state must be missing: %+v", pre[ref2])
	}

	// Bounded pages: two keys, then one, then done.
	page1, err := st.RepairProjections(ctx, tenant, RepairOptions{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if page1.Examined != 2 || page1.Repaired != 2 || page1.Next == nil || page1.Next.EntityID != "g2" {
		t.Fatalf("page 1: %+v", page1)
	}
	page2, err := st.RepairProjections(ctx, tenant, RepairOptions{Limit: 2, After: page1.Next})
	if err != nil {
		t.Fatal(err)
	}
	if page2.Examined != 1 || page2.Repaired != 0 || page2.Next != nil {
		t.Fatalf("page 2 (healthy g3): %+v next=%v", page2, page2.Next)
	}

	post, err := st.States(ctx, tenant, user, []EntityRef{ref1, ref2})
	if err != nil {
		t.Fatal(err)
	}
	if s := post[ref1]; s.TotalEvents != 2 || !s.LastSignalAt.Equal(at(2, 11)) || !s.Completed || s.Resume != "p:19" {
		t.Fatalf("g1 not healed: %+v", s)
	}
	if s, ok := post[ref2]; !ok || s.TotalEvents != 1 || s.Resume != "p:7" {
		t.Fatalf("g2 not healed: %+v (ok=%v)", s, ok)
	}
	m, err := st.Metrics(ctx, tenant, "gallery", []string{"g1", "g2"}, AllTime())
	if err != nil {
		t.Fatal(err)
	}
	if m["g1"].Views != 2 || m["g2"].Views != 1 || m["g1"].Completions != 1 {
		t.Fatalf("daily projection not healed: %+v", m)
	}

	res, err := st.RepairProjections(ctx, tenant, RepairOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Examined != 3 || res.Repaired != 0 {
		t.Fatalf("second sweep must be a no-op: %+v", res)
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
	st, _ := freshStore(t)
	ctx := context.Background()
	tenant := "t"
	user := Subject{UserID: "u1"}
	anon := Subject{AnonKey: "abc123"}

	if err := st.RecordSignals(ctx, tenant, []Signal{view("g1", user, 1, 10, 5, 10, 50, false)}); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordSignals(ctx, tenant, []Signal{view("g1", anon, 1, 11, 10, 10, 80, true)}); err != nil {
		t.Fatal(err)
	}

	ref := EntityRef{EntityType: "gallery", EntityID: "g1"}
	us, err := st.States(ctx, tenant, user, []EntityRef{ref})
	if err != nil {
		t.Fatal(err)
	}
	as, err := st.States(ctx, tenant, anon, []EntityRef{ref})
	if err != nil {
		t.Fatal(err)
	}
	if us[ref].MaxProgress != 5 || as[ref].MaxProgress != 10 {
		t.Fatalf("subject isolation broken: user=%+v anon=%+v", us[ref], as[ref])
	}

	m, err := st.Metrics(ctx, tenant, "gallery", []string{"g1"}, AllTime())
	if err != nil {
		t.Fatal(err)
	}
	g1 := m["g1"]
	if g1.UserViewers != 1 || g1.AnonViewers != 1 || g1.Viewers != 2 || g1.Views != 2 {
		t.Fatalf("metrics: %+v", g1)
	}
	if g1.Completers != 1 || g1.Completions != 1 || g1.SignalCounts[TypeView] != 2 || g1.ScoreSum != 130 {
		t.Fatalf("metrics: %+v", g1)
	}
}

func TestIntegrationHistoryAndSeen(t *testing.T) {
	st, _ := freshStore(t)
	ctx := context.Background()
	tenant := "t"
	user := Subject{UserID: "u1"}

	// g1 completed, g2 in progress, g3 zero progress (event but unseen),
	// blog b1 in progress.
	signals := []Signal{
		view("g1", user, 1, 10, 20, 20, 90, true),
		view("g2", user, 2, 10, 5, 20, 40, false),
		view("g3", user, 3, 10, 0, 20, 0, false),
		{
			EntityRef:   EntityRef{EntityType: "blog_post", EntityID: "b1"},
			Subject:     user,
			Type:        "view",
			OccurredAt:  at(4, 10),
			Progress:    50,
			ProgressMax: 100,
			Score:       30,
			EventID:     "b1-session",
		},
	}
	if err := st.RecordSignals(ctx, tenant, signals); err != nil {
		t.Fatal(err)
	}

	// Full history, most recent first, across types.
	all, err := st.History(ctx, tenant, user, HistoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 4 {
		t.Fatalf("history len=%d want 4: %+v", len(all), all)
	}
	if all[0].EntityID != "b1" || all[3].EntityID != "g1" {
		t.Fatalf("history order wrong: %+v", all)
	}

	// Galleries only, in progress.
	inProg, err := st.History(ctx, tenant, user, HistoryOptions{EntityType: "gallery", Status: HistoryInProgress})
	if err != nil {
		t.Fatal(err)
	}
	if len(inProg) != 1 || inProg[0].EntityID != "g2" {
		t.Fatalf("in-progress: %+v", inProg)
	}

	completed, err := st.History(ctx, tenant, user, HistoryOptions{EntityType: "gallery", Status: HistoryCompleted})
	if err != nil {
		t.Fatal(err)
	}
	if len(completed) != 1 || completed[0].EntityID != "g1" {
		t.Fatalf("completed: %+v", completed)
	}

	// Seen-set: g3 had progress 0 -> NOT seen.
	seen, err := st.SeenIDs(ctx, tenant, user, "gallery")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := seen["g1"]; !ok {
		t.Fatalf("g1 must be seen: %v", seen)
	}
	if _, ok := seen["g2"]; !ok {
		t.Fatalf("g2 must be seen: %v", seen)
	}
	if _, ok := seen["g3"]; ok {
		t.Fatalf("g3 (zero progress) must not be seen: %v", seen)
	}

	// TopStates orders by last_score.
	top, err := st.TopStates(ctx, tenant, user, TopStatesOptions{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(top) != 2 || top[0].EntityID != "g1" || top[1].EntityID != "g2" {
		t.Fatalf("top states: %+v", top)
	}
}

func TestIntegrationPopular(t *testing.T) {
	st, _ := freshStore(t)
	ctx := context.Background()
	tenant := "t"

	// gHot: 10 subjects, good scores. gNiche: 1 subject, perfect score.
	// gMeh: 5 subjects, low scores. gOld: 8 subjects but outside the window.
	for i := 0; i < 10; i++ {
		sub := Subject{AnonKey: fmt.Sprintf("hot%d", i)}
		if err := st.RecordSignals(ctx, tenant, []Signal{view("gHot", sub, 10, 8+i%4, 18, 20, 80, true)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.RecordSignals(ctx, tenant, []Signal{view("gNiche", Subject{AnonKey: "n1"}, 10, 9, 20, 20, 100, true)}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		sub := Subject{AnonKey: fmt.Sprintf("meh%d", i)}
		if err := st.RecordSignals(ctx, tenant, []Signal{view("gMeh", sub, 11, 8+i%4, 2, 20, 10, false)}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 8; i++ {
		sub := Subject{AnonKey: fmt.Sprintf("old%d", i)}
		if err := st.RecordSignals(ctx, tenant, []Signal{view("gOld", sub, 1, 8+i%4, 18, 20, 90, true)}); err != nil {
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
		t.Fatalf("expected 3 in-window entities, got %+v", hits)
	}
	if hits[0].EntityID != "gHot" {
		t.Fatalf("gHot must rank first: %+v", hits)
	}
	// Bayesian prior: 1 perfect-score subject must not outrank 10 good ones.
	rank := map[string]int{}
	for i, h := range hits {
		rank[h.EntityID] = i
	}
	if rank["gNiche"] < rank["gMeh"] {
		// gNiche (1 subject) ranking above gMeh (5 subjects) is acceptable
		// only because gMeh's scores are terrible; but it must never beat gHot.
		if rank["gNiche"] == 0 {
			t.Fatalf("tiny-sample entity outranked high-volume: %+v", hits)
		}
	}
	if hits[0].Viewers != 10 {
		t.Fatalf("gHot viewers=%d want 10", hits[0].Viewers)
	}

	// All-time includes gOld.
	all, err := st.Popular(ctx, tenant, "gallery", PopularOptions{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 4 {
		t.Fatalf("all-time should rank 4 entities: %+v", all)
	}

	// Host rank expression: raw subject count.
	byVolume, err := st.Popular(ctx, tenant, "gallery", PopularOptions{
		RankExpr: "toFloat64(viewers)",
		Limit:    10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if byVolume[0].EntityID != "gHot" || byVolume[1].EntityID != "gOld" {
		t.Fatalf("volume ranking: %+v", byVolume)
	}

	// PopularityFor scores only the requested candidates.
	scores, err := st.PopularityFor(ctx, tenant, "gallery", []string{"gHot", "gNiche", "missing"}, AllTime())
	if err != nil {
		t.Fatal(err)
	}
	if len(scores) != 2 || scores["gHot"] <= 0 {
		t.Fatalf("popularity-for: %v", scores)
	}
}

func TestIntegrationCoEngaged(t *testing.T) {
	st, _ := freshStore(t)
	ctx := context.Background()
	tenant := "t"

	// s1, s2 engage X and Y; s3 engages X and Z; s4 engages only Y.
	pairs := []struct {
		sub string
		ids []string
	}{
		{"s1", []string{"X", "Y"}},
		{"s2", []string{"X", "Y"}},
		{"s3", []string{"X", "Z"}},
		{"s4", []string{"Y"}},
	}
	hour := 0
	for _, p := range pairs {
		for _, id := range p.ids {
			hour++
			if err := st.RecordSignals(ctx, tenant, []Signal{view(id, Subject{UserID: p.sub}, 1, 8+hour%10, 10, 10, 50, true)}); err != nil {
				t.Fatal(err)
			}
		}
	}

	co, err := st.CoEngaged(ctx, tenant, EntityRef{EntityType: "gallery", EntityID: "X"}, CoEngagedOptions{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(co) != 2 {
		t.Fatalf("co-engaged: %+v", co)
	}
	if co[0].EntityID != "Y" || co[0].Strength != 2 {
		t.Fatalf("Y should lead with strength 2: %+v", co)
	}
	if co[1].EntityID != "Z" || co[1].Strength != 1 {
		t.Fatalf("Z should follow with strength 1: %+v", co)
	}
}

func TestIntegrationNegativeSignalsAndItemPairs(t *testing.T) {
	st, _ := freshStore(t)
	ctx := context.Background()
	tenant := "t"

	react := func(sub Subject, id string, kind string, value float64, day int) Signal {
		return Signal{
			EntityRef:  EntityRef{EntityType: "gallery", EntityID: id},
			Subject:    sub,
			Type:       kind,
			OccurredAt: at(day, 12),
			Value:      value,
			EventID:    kind + ":" + id + ":" + sub.Key(),
		}
	}

	u1, u2, u3 := Subject{UserID: "u1"}, Subject{UserID: "u2"}, Subject{UserID: "u3"}
	signals := []Signal{
		// u1 likes A and B; u2 likes A and C; u3 likes A but DISLIKES B.
		react(u1, "A", "like", 1, 1), react(u1, "B", "like", 1, 1),
		react(u2, "A", "like", 1, 2), react(u2, "C", "like", 1, 2),
		react(u3, "A", "like", 1, 3), react(u3, "B", "dislike", -1, 3),
	}
	if err := st.RecordSignals(ctx, tenant, signals); err != nil {
		t.Fatal(err)
	}

	// State carries net sentiment.
	states, err := st.States(ctx, tenant, u3, []EntityRef{{EntityType: "gallery", EntityID: "B"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := states[EntityRef{EntityType: "gallery", EntityID: "B"}].NetValue; got >= 0 {
		t.Fatalf("disliked B must have negative NetValue, got %v", got)
	}

	// NegativeIDs returns the dislike set.
	neg, err := st.NegativeIDs(ctx, tenant, u3, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := neg[EntityRef{EntityType: "gallery", EntityID: "B"}]; !ok || len(neg) != 1 {
		t.Fatalf("negative ids: %v", neg)
	}

	// TopStates with ExcludeNegative drops B for u3.
	top, err := st.TopStates(ctx, tenant, u3, TopStatesOptions{ExcludeNegative: true, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range top {
		if r.EntityID == "B" {
			t.Fatalf("disliked entity must not seed: %+v", top)
		}
	}

	// Query-time co-engagement from anchor A: B has 2 positive co-subjects
	// (u1) and 1 negative (u3) -> net 1; C has 1.
	co, err := st.CoEngaged(ctx, tenant, EntityRef{EntityType: "gallery", EntityID: "A"}, CoEngagedOptions{Limit: 10, SkipRollup: true})
	if err != nil {
		t.Fatal(err)
	}
	strengthOf := func(hits []CoEngagedHit, id string) int64 {
		for _, h := range hits {
			if h.EntityID == id {
				return h.Strength
			}
		}
		return 0
	}
	if strengthOf(co, "B") != 0 { // u1 likes B (+1), u3 dislikes B (-1) -> wait, u1 is 1 positive, u3 is 1 negative => net 0 -> excluded by HAVING
		t.Fatalf("B net strength should be 0 (excluded): %+v", co)
	}
	if strengthOf(co, "C") != 1 {
		t.Fatalf("C net strength should be 1: %+v", co)
	}

	// Rollup path: refresh item_pairs and read through it.
	if err := st.RefreshCoEngagement(ctx, tenant, RefreshCoEngagementOptions{}); err != nil {
		t.Fatal(err)
	}
	coR, err := st.CoEngaged(ctx, tenant, EntityRef{EntityType: "gallery", EntityID: "A"}, CoEngagedOptions{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if strengthOf(coR, "C") != 1 {
		t.Fatalf("rollup co-engagement for C should be 1: %+v", coR)
	}
	for _, h := range coR {
		if h.EntityID == "B" && h.Strength > 0 {
			t.Fatalf("rollup must net out the dislike on B: %+v", coR)
		}
	}

	// Refresh is idempotent (DELETE + INSERT).
	if err := st.RefreshCoEngagement(ctx, tenant, RefreshCoEngagementOptions{}); err != nil {
		t.Fatal(err)
	}
	coR2, err := st.CoEngaged(ctx, tenant, EntityRef{EntityType: "gallery", EntityID: "A"}, CoEngagedOptions{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(coR2) != len(coR) {
		t.Fatalf("refresh not idempotent: %d vs %d", len(coR2), len(coR))
	}
}
