package critical_test

import (
	"reflect"
	"testing"
	"time"

	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/kinds"
)

func TestPreparedPopularityBuckets(t *testing.T) {
	w := newWorld(t, func(cfg *ck.Config) {
		kind := cfg.Content.Kinds["article"]
		kind.Popularity = []kinds.PopularityBucket{
			{ID: "week", Days: 7, RefreshEvery: 24 * time.Hour},
			{ID: "november", From: "2025-11-01", Until: "2025-11-24"},
		}
		cfg.Content.Kinds["article"] = kind
	})
	a := w.apps[0]
	a.server.Close()
	must(t, a.engine.Close(t.Context()))
	configureDiscoverySignals(t, w, a)
	var err error
	a.engine, err = ck.New(t.Context(), a.cfg, a.deps)
	must(t, err)
	a.server = mountEngine(t, a.engine)
	client := a.engine.Client()
	model, err := client.GetKinds(t.Context())
	must(t, err)
	if got := model.Kinds["article"].Popularity; len(got) != 2 || got[0].ID != "week" || got[1].Until != "2025-11-24" {
		t.Fatalf("public choices differ from host configuration: %+v", got)
	}
	pending, err := client.GetContents(t.Context(), ck.GetContentsQuery{Kind: "article", Sort: "popular"})
	must(t, err)
	if pending.Popularity == nil || pending.Popularity.Ready || pending.Popularity.Bucket != "week" || len(pending.Contents) != 0 {
		t.Fatalf("cold bucket is not explicitly pending: %+v", pending)
	}
	var rows int
	must(t, w.pool.QueryRow(t.Context(), `SELECT count(*) FROM `+w.schema+`.content_popular_rankings`).Scan(&rows))
	if rows != 0 {
		t.Fatalf("public read created %d ranking rows", rows)
	}
	must(t, a.engine.Start(t.Context()))
	// Neither bucket is requested from a worker API: RunOnStart prepares all
	// configured choices, including the fixed bucket nobody has read yet.
	waitForWithin(t, time.Minute, func() bool {
		must(t, w.pool.QueryRow(t.Context(), `SELECT count(*) FROM `+w.schema+`.content_popular_rankings WHERE computed_at IS NOT NULL`).Scan(&rows))
		return rows == 2
	})
	ready, err := client.GetContents(t.Context(), ck.GetContentsQuery{Kind: "article", Sort: "popular"})
	must(t, err)
	if ready.Popularity == nil || !ready.Popularity.Ready || len(ready.Contents) != 0 {
		t.Fatalf("prepared empty bucket is not ready: %+v", ready)
	}
	before := *ready.Popularity
	if before.WindowFrom == nil || before.WindowUntil == nil || before.ComputedAt == nil {
		t.Fatalf("prepared bucket has no resolved dates: %+v", before)
	}
	w.clock.set(before.WindowUntil.Add(time.Minute))
	old, err := client.GetContents(t.Context(), ck.GetContentsQuery{Kind: "article", Sort: "popular"})
	must(t, err)
	if !reflect.DeepEqual(old.Popularity, &before) {
		t.Fatal("read relabelled yesterday's stored snapshot as today's window")
	}
	refreshPopularRankings(t, w)
	updated, err := client.GetContents(t.Context(), ck.GetContentsQuery{Kind: "article", Sort: "popular"})
	must(t, err)
	if updated.Popularity == nil || updated.Popularity.WindowUntil == nil || !updated.Popularity.WindowUntil.Equal(before.WindowUntil.AddDate(0, 0, 1)) {
		t.Fatalf("UTC rollover failed to prepare the new window before the 24-hour cadence: %+v", updated.Popularity)
	}
	// Prepared PostgreSQL rankings remain usable without a live signal runtime.
	a.server.Close()
	must(t, a.engine.Close(t.Context()))
	a.cfg.ClickHouse = ck.ClickHouseConfig{}
	a.deps.ClickHouse = nil
	a.engine, err = ck.New(t.Context(), a.cfg, a.deps)
	must(t, err)
	a.server = mountEngine(t, a.engine)
	stored, err := a.engine.Client().GetContents(t.Context(), ck.GetContentsQuery{Kind: "article", Sort: "popular"})
	must(t, err)
	if !reflect.DeepEqual(stored.Popularity, updated.Popularity) {
		t.Fatal("stored ranking was unavailable without ClickHouse")
	}
}
