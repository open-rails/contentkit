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
	firstID, middleID, lastID, beforeID, afterID := cid(1), cid(2), cid(3), cid(4), cid(5)
	now := time.Date(2026, 5, 20, 15, 0, 0, 0, time.UTC)
	window := LastDays(7, now) // [2026-05-14, 2026-05-21)
	sec := func(day, h, m, s int) time.Time { return time.Date(2026, 5, day, h, m, s, 0, time.UTC) }
	record := func(id, subject string, at time.Time) {
		t.Helper()
		sig := Signal{
			ContentRef: gallery(tenant, id),
			Subject:    Subject{UserID: subject}, Type: "view", EventID: id + ":" + subject,
			OccurredAt: at, Progress: 10, ProgressMax: 10, Score: 60, Completed: true,
		}
		if err := st.RecordSignals(ctx, tenant, []Signal{sig}); err != nil {
			t.Fatal(err)
		}
	}
	for _, subject := range []string{"u1", "u2"} {
		record(firstID, subject, sec(14, 0, 0, 0))
		record(middleID, subject, sec(17, 12, 30, 0))
		record(lastID, subject, sec(20, 23, 59, 59))
		record(beforeID, subject, sec(13, 23, 59, 59))
		record(afterID, subject, sec(21, 0, 0, 0))
	}

	hits, err := st.Popular(ctx, tenant, "gallery", PopularOptions{Window: window, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 3 {
		t.Fatalf("only in-window works rank: %+v", hits)
	}
	for _, h := range hits[1:] {
		if h.Score != hits[0].Score || h.Viewers != 2 || h.Views != 2 || h.Completions != 2 {
			t.Fatalf("contribution depends on position in window: %+v", hits)
		}
	}
	ids := []string{firstID, middleID, lastID, beforeID, afterID}
	metrics := metricsByID(t, st, tenant, ids, window)
	counts := map[string]uint64{}
	for id, m := range metrics {
		counts[id] = m.Viewers
	}
	if want := map[string]uint64{firstID: 2, middleID: 2, lastID: 2}; !reflect.DeepEqual(counts, want) {
		t.Fatalf("subject counts %v want %v", counts, want)
	}
	scores, err := st.PopularityFor(ctx, tenant, "gallery", ids, window)
	if err != nil {
		t.Fatal(err)
	}
	if len(scores) != 3 || scores[firstID] != scores[lastID] || scores[middleID] != scores[firstID] {
		t.Fatalf("popularity-for must treat window days equally and exclude neighbours: %v", scores)
	}

	// Co-engagement uses the same half-open days.
	co, err := st.CoEngaged(ctx, tenant, gallery(tenant, middleID), CoEngagedOptions{Window: window, SkipRollup: true, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int64{}
	for _, h := range co {
		got[h.ContentID] = h.Strength
	}
	if want := map[string]int64{firstID: 2, lastID: 2}; !reflect.DeepEqual(got, want) {
		t.Fatalf("windowed co-engagement %v want %v", got, want)
	}
	if err := st.RefreshCoEngagement(ctx, tenant, RefreshCoEngagementOptions{Window: window}); err != nil {
		t.Fatal(err)
	}
	co, err = st.CoEngaged(ctx, tenant, gallery(tenant, middleID), CoEngagedOptions{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	got = map[string]int64{}
	for _, h := range co {
		got[h.ContentID] = h.Strength
	}
	if want := map[string]int64{firstID: 2, lastID: 2}; !reflect.DeepEqual(got, want) {
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
