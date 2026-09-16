package signal

import (
	"context"
	"reflect"
	"testing"
	"time"
)

// #887: a window is a literal set of whole UTC days; position inside it never
// changes a contribution and the adjacent instants outside never count.
func TestIntegrationWindowBoundariesAreLiteral(t *testing.T) {
	st, _ := freshStore(t)
	ctx := context.Background()
	tenant := "t"
	now := time.Date(2026, 5, 20, 15, 0, 0, 0, time.UTC)
	window := LastDays(7, now) // [2026-05-14, 2026-05-21)
	sec := func(day, h, m, s int) time.Time { return time.Date(2026, 5, day, h, m, s, 0, time.UTC) }
	record := func(id, subject string, at time.Time) {
		t.Helper()
		sig := Signal{
			EntityRef: EntityRef{EntityType: "gallery", EntityID: id},
			Subject:   Subject{UserID: subject}, Type: "view", EventID: id + ":" + subject,
			OccurredAt: at, Progress: 10, ProgressMax: 10, Score: 60, Completed: true,
		}
		if err := st.RecordSignals(ctx, tenant, []Signal{sig}); err != nil {
			t.Fatal(err)
		}
	}
	for _, subject := range []string{"u1", "u2"} {
		record("first-instant", subject, sec(14, 0, 0, 0))
		record("middle", subject, sec(17, 12, 30, 0))
		record("last-instant", subject, sec(20, 23, 59, 59))
		record("before", subject, sec(13, 23, 59, 59))
		record("after", subject, sec(21, 0, 0, 0))
	}

	hits, err := st.Popular(ctx, tenant, "gallery", PopularOptions{Window: window, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 3 {
		t.Fatalf("only in-window entities rank: %+v", hits)
	}
	for _, h := range hits[1:] {
		if h.Score != hits[0].Score || h.Viewers != 2 || h.Views != 2 || h.Completions != 2 {
			t.Fatalf("contribution depends on position in window: %+v", hits)
		}
	}
	ids := []string{"first-instant", "middle", "last-instant", "before", "after"}
	metrics, err := st.Metrics(ctx, tenant, "gallery", ids, window)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]uint64{}
	for id, m := range metrics {
		counts[id] = m.Viewers
	}
	if want := map[string]uint64{"first-instant": 2, "middle": 2, "last-instant": 2}; !reflect.DeepEqual(counts, want) {
		t.Fatalf("subject counts %v want %v", counts, want)
	}
	scores, err := st.PopularityFor(ctx, tenant, "gallery", ids, window)
	if err != nil {
		t.Fatal(err)
	}
	if len(scores) != 3 || scores["first-instant"] != scores["last-instant"] || scores["middle"] != scores["first-instant"] {
		t.Fatalf("popularity-for must treat window days equally and exclude neighbours: %v", scores)
	}

	// Co-engagement uses the same half-open days.
	co, err := st.CoEngaged(ctx, tenant, EntityRef{EntityType: "gallery", EntityID: "middle"}, CoEngagedOptions{Window: window, SkipRollup: true, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int64{}
	for _, h := range co {
		got[h.EntityID] = h.Strength
	}
	if want := map[string]int64{"first-instant": 2, "last-instant": 2}; !reflect.DeepEqual(got, want) {
		t.Fatalf("windowed co-engagement %v want %v", got, want)
	}
	if err := st.RefreshCoEngagement(ctx, tenant, RefreshCoEngagementOptions{Window: window}); err != nil {
		t.Fatal(err)
	}
	co, err = st.CoEngaged(ctx, tenant, EntityRef{EntityType: "gallery", EntityID: "middle"}, CoEngagedOptions{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	got = map[string]int64{}
	for _, h := range co {
		got[h.EntityID] = h.Strength
	}
	if want := map[string]int64{"first-instant": 2, "last-instant": 2}; !reflect.DeepEqual(got, want) {
		t.Fatalf("windowed pair rollup %v want %v", got, want)
	}

	// The 7/30/90/365/all windows are distinct keys; rejected windows never query.
	keys := map[string]bool{}
	for _, w := range []Window{LastDays(7, now), LastDays(30, now), LastDays(90, now), LastDays(365, now), AllTime()} {
		keys[w.String()] = true
	}
	if len(keys) != 5 {
		t.Fatalf("window keys collide: %v", keys)
	}
	if _, err := st.Popular(ctx, tenant, "gallery", PopularOptions{Window: Between(sec(14, 6, 0, 0), sec(15, 0, 0, 0))}); err == nil {
		t.Fatal("sub-day window must be rejected")
	}
}
