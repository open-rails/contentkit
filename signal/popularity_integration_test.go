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
	// Keep duplicate rows physically present so FINAL, not merge timing, must
	// make retries contribute once to scores and completions.
	if err := conn.Exec(ctx, "SYSTEM STOP MERGES "+testDB+".signal_events"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Exec(ctx, "SYSTEM START MERGES "+testDB+".signal_events") })
	for _, cohort := range []struct {
		id  string
		day int
	}{{"early", 2}, {"late", 28}} {
		for i, score := range []int16{80, 0} {
			s := view(cohort.id, Subject{UserID: fmt.Sprintf("u%d", i)}, cohort.day, 8, 10, 10, score, true)
			s.EventID = fmt.Sprintf("%s-%d", cohort.id, i)
			if err := st.RecordSignals(ctx, "t", []Signal{s, s}); err != nil {
				t.Fatal(err)
			}
		}
	}
	click := view("early", Subject{UserID: "click-only"}, 10, 8, 0, 0, 100, false)
	click.Type, click.EventID = "click", "click"
	like := click
	like.EntityID, like.Type, like.EventID = "reaction-only", "like", "like"
	outside := view("outside", Subject{UserID: "u3"}, 1, 8, 10, 10, 100, true)
	if err := st.RecordSignals(ctx, "t", []Signal{click, like, outside}); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordSignal(ctx, "other-tenant", view("early", Subject{UserID: "foreign"}, 20, 8, 10, 10, 100, true)); err != nil {
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
			t.Fatalf("expected only consuming entities in tenant/window: %+v", hits)
		}
		for _, hit := range hits {
			if hit.Subjects != 2 || hit.Signals != 2 || hit.Completions != 2 {
				t.Fatalf("clicks or duplicate deliveries inflated consumption: %+v", hit)
			}
			// The zero-score view counts in the mean; early and late have equal weight.
			want := math.Log10(3) * (80.0 / 12)
			if math.Abs(hit.Score-want) > 1e-9 {
				t.Fatalf("score for %s: got %g want %g", hit.EntityID, hit.Score, want)
			}
		}
		return hits
	}
	before := read()
	counts, err := st.SubjectCounts(ctx, "t", "gallery", []string{"early", "late", "outside", "reaction-only"}, window)
	if err != nil || !reflect.DeepEqual(counts, map[string]uint64{"early": 2, "late": 2}) {
		t.Fatalf("card counts disagree with popularity: %v, %v", counts, err)
	}
	if err := conn.Exec(ctx, "SYSTEM START MERGES "+testDB+".signal_events"); err != nil {
		t.Fatal(err)
	}
	if err := conn.Exec(ctx, "OPTIMIZE TABLE "+testDB+".signal_events FINAL"); err != nil {
		t.Fatal(err)
	}
	if after := read(); !reflect.DeepEqual(before, after) {
		t.Fatalf("background compaction changed rank: before=%+v after=%+v", before, after)
	}
}
