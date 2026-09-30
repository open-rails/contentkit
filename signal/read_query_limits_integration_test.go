package signal

import (
	"context"
	"reflect"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2"
)

func TestIntegrationMetricsLargeReferenceQueryLimit(t *testing.T) {
	st, _ := queryLimitStore(t)
	ctx := clickhouse.Context(context.Background(),
		clickhouse.WithSettings(clickhouse.Settings{"max_query_size": 262144}),
	)
	refs := make([]ContentRef, 6001)
	for i := range refs {
		refs[i] = gallery("metrics", cid(i+1))
	}
	edition := refs[0].WithVersion("edition'\\日本語")
	refs = append(refs, edition, refs[0])
	for _, tenant := range []string{"metrics", "other"} {
		first := view(tenant, cid(1), Subject{UserID: "reader"}, 1, 10, 1, 2, 50, false)
		last := view(tenant, cid(6001), Subject{UserID: "reader"}, 1, 10, 1, 2, 70, false)
		version := first
		version.ContentRef = gallery(tenant, cid(1)).WithVersion(edition.Version())
		version.Score = 90
		if err := st.RecordSignals(ctx, tenant, []Signal{first, last, version}); err != nil {
			t.Fatal(err)
		}
	}
	metrics, err := st.Metrics(ctx, "metrics", refs, AllTime())
	if err != nil {
		t.Fatal(err)
	}
	if len(metrics) != 3 {
		t.Fatalf("missing refs must stay absent, duplicate refs must not double-count: %+v", metrics)
	}
	for ref, score := range map[ContentKey]int64{refs[0].Key(): 50, refs[6000].Key(): 70, edition.Key(): 90} {
		m := metrics[ref]
		if m.Views != 1 || m.Viewers != 1 || m.ScoreSum != score {
			t.Fatalf("metrics for %+v: %+v, want one view and score %d", ref, m, score)
		}
	}
}

func TestIntegrationBulkSearchReadsQueryLimit(t *testing.T) {
	st, _ := queryLimitStore(t)
	ctx := clickhouse.Context(t.Context(), clickhouse.WithSettings(clickhouse.Settings{"max_query_size": 262144}))
	refs := make([]ContentRef, 7001)
	ids := make([]string, len(refs))
	for i := range refs {
		refs[i] = gallery("bulk-search", cid(i+1))
		ids[i] = refs[i].ContentID
	}
	edition := refs[0].WithVersion("edition'\\日本語")
	refs = append(refs, edition, refs[0])
	for _, tenant := range []string{"bulk-search", "other"} {
		first := view(tenant, cid(1), Subject{UserID: "reader"}, 1, 10, 1, 2, 50, false)
		last := view(tenant, cid(7001), Subject{UserID: "reader"}, 1, 10, 1, 2, 70, false)
		version := first
		version.ContentRef = gallery(tenant, cid(1)).WithVersion(edition.Version())
		version.Score = 90
		otherReader := first
		otherReader.Subject = Subject{UserID: "another-reader"}
		otherReader.Score = 10
		if err := st.RecordSignals(ctx, tenant, []Signal{first, last, version, otherReader}); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("states", func(t *testing.T) {
		states, err := st.States(ctx, "bulk-search", Subject{UserID: "reader"}, refs)
		if err != nil {
			t.Fatal(err)
		}
		if len(states) != 3 {
			t.Fatalf("missing references must stay absent: %+v", states)
		}
		for ref, score := range map[ContentKey]int16{refs[0].Key(): 50, refs[7000].Key(): 70, edition.Key(): 90} {
			if state := states[ref]; state.LastScore != score || state.Views != 1 {
				t.Fatalf("subject/work/version standing for %+v: %+v", ref, state)
			}
		}
	})
	t.Run("popularity", func(t *testing.T) {
		small, err := st.PopularityFor(ctx, "bulk-search", "gallery", []string{ids[0], ids[7000]}, AllTime())
		if err != nil || len(small) != 2 {
			t.Fatalf("small candidate scores: %+v, %v", small, err)
		}
		large, err := st.PopularityFor(ctx, "bulk-search", "gallery", append(ids, ids[0]), AllTime())
		if err != nil || !reflect.DeepEqual(large, small) {
			t.Fatalf("large candidate scores must match small work-level scores: %+v, want %+v, err=%v", large, small, err)
		}
	})
}
