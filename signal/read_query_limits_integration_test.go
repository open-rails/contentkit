package signal

import (
	"context"
	"reflect"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2"
)

func TestIntegrationCoEngagedLargeReaderQueryLimit(t *testing.T) {
	st, conn := queryLimitStore(t)
	ctx := clickhouse.Context(t.Context(), clickhouse.WithSettings(clickhouse.Settings{"max_query_size": 262144}))
	anchor, candidate := gallery("readers", cid(1)), gallery("readers", cid(2))
	// The default 10,000-reader sample is larger than the SQL text limit when
	// identities are interpolated. Query-local data must keep it out of SQL.
	seed := `SELECT 'readers' AS tenant,if(number%2=0,?,?) AS content_id,
 'edition' AS version_id,concat('reader-',leftPad(toString(intDiv(number,2)),36,'0')) AS subject,
 toString(number) AS event_id,toDateTime64('2026-10-01',6,'UTC') AS at FROM numbers(22000)`
	if err := seedMeasured(ctx, conn, st, seed, anchor.ContentID, candidate.ContentID); err != nil {
		t.Fatal(err)
	}
	hits, err := st.CoEngaged(ctx, "readers", anchor, CoEngagedOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].ContentID != candidate.ContentID || hits[0].Strength != 10000 {
		t.Fatalf("default sample must cap 11,000 readers at 10,000: %+v", hits)
	}
}

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
		if err := recordFixtureSignals(st, ctx, tenant, []readingFixture{first, last, version}); err != nil {
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
		if m.Readers != 1 {
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
		if err := recordFixtureSignals(st, ctx, tenant, []readingFixture{first, last, version, otherReader}); err != nil {
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
		for _, ref := range []ContentKey{refs[0].Key(), refs[7000].Key(), edition.Key()} {
			if value := states[ref]; value.ActiveMS == 0 || value.Quality <= 0 || value.Quality > 1 {
				t.Fatalf("measured standing missing: %+v", value)
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
