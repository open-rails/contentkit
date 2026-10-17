package signal

import (
	"context"
	"fmt"
	"math"
	"reflect"
	"testing"
)

func TestIntegrationPopularityCountsConsumptionWithoutAgeBias(t *testing.T) {
	st, conn := freshStore(t)
	ctx := context.Background()
	earlyID, lateID, outsideID, reactionID := cid(1), cid(2), cid(3), cid(4)
	// Keep duplicate rows physically present so FINAL, not merge timing, must
	// make retries contribute once to scores and completions.
	if err := conn.Exec(ctx, "SYSTEM STOP MERGES "+testDB+".engagement_segments"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Exec(ctx, "SYSTEM START MERGES "+testDB+".engagement_segments") })
	for _, cohort := range []struct {
		id  string
		day int
	}{{earlyID, 2}, {lateID, 28}} {
		for i, seconds := range []int{80, 40} {
			s := view("t", cohort.id, Subject{UserID: fmt.Sprintf("u%d", i)}, cohort.day, seconds, 10, 10, 0, true)
			s.EventID = fmt.Sprintf("%s-%d", cohort.id, i)
			if err := recordFixtureSignals(st, ctx, "t", []readingFixture{s, s}); err != nil {
				t.Fatal(err)
			}
		}
	}
	click := view("t", earlyID, Subject{UserID: "click-only"}, 10, 8, 0, 0, 100, false)
	click.Type, click.EventID = "click", "click"
	like := click
	like.ContentRef, like.Type, like.EventID = gallery("t", reactionID), "like", "like"
	outside := view("t", outsideID, Subject{UserID: "u3"}, 1, 8, 10, 10, 100, true)
	// The same person reading a second edition remains one person for the work.
	edition := view("t", earlyID, Subject{UserID: "u0"}, 10, 80, 10, 10, 0, true)
	edition.ContentRef, edition.EventID = edition.WithVersion("v2"), "edition"
	if err := recordFixtureSignals(st, ctx, "t", []readingFixture{click, like, outside, edition}); err != nil {
		t.Fatal(err)
	}
	if err := recordFixtureSignals(st, ctx, "other-tenant", []readingFixture{view("other-tenant", earlyID, Subject{UserID: "foreign"}, 20, 8, 10, 10, 100, true)}); err != nil {
		t.Fatal(err)
	}
	window := Between(at(2, 0), at(29, 0))
	read := func() []PopularHit {
		t.Helper()
		hits, err := st.Popular(ctx, "t", "gallery", PopularOptions{Window: window})
		if err != nil {
			t.Fatal(err)
		}
		if len(hits) != 2 {
			t.Fatalf("expected only consuming works in tenant/window: %+v", hits)
		}
		for _, hit := range hits {
			if hit.Readers != 2 || hit.Version() != "" {
				t.Fatalf("clicks, versions or duplicate deliveries inflated consumption: %+v", hit)
			}
			// Actual 8s and 4s/page attention produces qualities 1 and .5;
			// publication age and repeated editions cannot add extra quality votes.
			want := math.Log10(3) * (150.0 / 12)
			if math.Abs(hit.Score-want) > 1e-9 {
				t.Fatalf("score for %s: got %g want %g", hit.ContentID, hit.Score, want)
			}
		}
		return hits
	}
	before := read()
	metrics := metricsByID(t, st, "t", []string{earlyID, lateID, outsideID, reactionID}, window)
	counts := map[string]uint64{}
	for id, m := range metrics {
		counts[id] = m.Readers
	}
	if !reflect.DeepEqual(counts, map[string]uint64{earlyID: 2, lateID: 2, reactionID: 0}) {
		t.Fatalf("card counts disagree with popularity: %v", counts)
	}
	if err := conn.Exec(ctx, "SYSTEM START MERGES "+testDB+".engagement_segments"); err != nil {
		t.Fatal(err)
	}
	if err := conn.Exec(ctx, "OPTIMIZE TABLE "+testDB+".engagement_segments FINAL"); err != nil {
		t.Fatal(err)
	}
	if after := read(); !reflect.DeepEqual(before, after) {
		t.Fatalf("background compaction changed rank: before=%+v after=%+v", before, after)
	}
}
