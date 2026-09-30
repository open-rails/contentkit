package signal

import (
	"context"
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
