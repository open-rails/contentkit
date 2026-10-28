package critical_test

import (
	"encoding/json"
	"errors"
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
	day := w.clock.now().UTC().Truncate(24 * time.Hour)
	// Instrumentation began partway through the day before yesterday. Only
	// yesterday onward is fully trustworthy, even when a preset asks for a week.
	tenant := a.cfg.Scope.AppID.String()
	_, seedErr := w.pool.Exec(t.Context(), `INSERT INTO `+w.schema+`.content_access_history_start (started_at,tenant_id) VALUES($1,$2)
 ON CONFLICT(tenant_id) DO UPDATE SET started_at=excluded.started_at`, day.AddDate(0, 0, -3), tenant)
	must(t, seedErr)
	must(t, a.deps.ClickHouse.Exec(t.Context(), `INSERT INTO `+a.cfg.ClickHouse.Database+`.measured_population_coverage (tenant,content_kind,domain,language,started_at) VALUES(?,?,?,?,?)`, tenant, "article", "", "", day.AddDate(0, 0, -2).Add(12*time.Hour)))
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
	must(t, a.engine.StartJobs(t.Context()))
	// Neither bucket is requested from a worker API: RunOnStart prepares all
	// configured choices, including the fixed bucket nobody has read yet.
	waitForWithin(t, time.Minute, func() bool {
		must(t, w.pool.QueryRow(t.Context(), `SELECT count(*) FROM `+w.schema+`.content_popular_rankings WHERE computed_at IS NOT NULL`).Scan(&rows))
		return rows == 2
	})
	ready, err := client.GetContents(t.Context(), ck.GetContentsQuery{Kind: "article", Sort: "popular"})
	must(t, err)
	if ready.Popularity == nil || !ready.Popularity.Ready || !ready.Popularity.Provisional || len(ready.Contents) != 0 {
		t.Fatalf("prepared empty bucket is not ready: %+v", ready.Popularity)
	}
	before := *ready.Popularity
	if before.AvailableFrom == nil || !before.AvailableFrom.Equal(day.AddDate(0, 0, -1)) || before.From == nil || !before.From.Equal(*before.AvailableFrom) {
		t.Fatalf("partial instrumentation was reported as complete history: %+v", before)
	}
	_, err = client.GetContents(t.Context(), ck.GetContentsQuery{Kind: "article", Sort: "popular:november"})
	requireError(t, err, 400, "invalid_request")
	var refused *ck.Error
	if !errors.As(err, &refused) {
		t.Fatal("missing structured range error")
	}
	var details map[string]string
	must(t, json.Unmarshal(refused.Details, &details))
	if details["available_from"] != day.AddDate(0, 0, -1).Format(time.DateOnly) {
		t.Fatalf("missing cutoff: %+v", details)
	}
	first := publishedArticle(t, a, "First daily result")
	second := publishedArticle(t, a, "Second daily result")
	// The read contract uses prepared scalar rows. These IDs have no raw
	// engagement receipts, so a raw-ranking fallback would return no results.
	for i, id := range []string{first.ID, second.ID} {
		must(t, a.deps.ClickHouse.Exec(t.Context(), `INSERT INTO `+a.cfg.ClickHouse.Database+`.popularity_daily (tenant,content_kind,domain,language,day,content_id,readers,eligible) VALUES(?,?,?,?,?,?,?,?)`, tenant, "article", "", "", day.AddDate(0, 0, -1), id, uint64(2-i), uint64(10)))
	}
	query := ck.GetContentsQuery{Kind: "article", Sort: "popular", ActivityFrom: day.AddDate(0, 0, -1).Format(time.DateOnly), ActivityUntil: day.Format(time.DateOnly), Limit: 1}
	ranged, err := client.GetContents(t.Context(), query)
	must(t, err)
	if ranged.Popularity == nil || !ranged.Popularity.Provisional {
		t.Fatal("yesterday's prepared range was not marked provisional")
	}
	if len(ranged.Contents) != 1 || ranged.Contents[0].ID != first.ID || ranged.NextCursor == "" {
		t.Fatalf("prepared range ordering/page: %+v", ranged)
	}
	query.Cursor = ranged.NextCursor
	next, err := client.GetContents(t.Context(), query)
	must(t, err)
	if len(next.Contents) != 1 || next.Contents[0].ID != second.ID {
		t.Fatalf("prepared range next page: %+v", next)
	}
	query.Cursor = ""
	query.ActivityFrom = day.AddDate(0, 0, -2).Format(time.DateOnly)
	_, err = client.GetContents(t.Context(), query)
	requireError(t, err, 400, "invalid_request")
	query.ActivityFrom = day.Format(time.DateOnly)
	query.ActivityUntil = day.AddDate(0, 0, 2).Format(time.DateOnly)
	_, err = client.GetContents(t.Context(), query)
	requireError(t, err, 400, "invalid_request")
	must(t, w.pool.QueryRow(t.Context(), `SELECT count(*) FROM `+w.schema+`.content_popular_rankings`).Scan(&rows))
	if rows != 2 {
		t.Fatalf("arbitrary ranges created cache rows: %d", rows)
	}
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
