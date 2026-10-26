package critical_test

import (
	"math"
	"testing"
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertype"

	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/signal"
)

// refreshPopularRankings runs one popular refresh on the engine's own River.
func refreshPopularRankings(t *testing.T, w *world) {
	t.Helper()
	queue, err := river.NewClient(riverpgxv5.New(w.pool), &river.Config{Schema: w.schema + "_river"})
	must(t, err)
	job, err := queue.Insert(t.Context(), scheduledContentJob{kind: "contentkit_popular_refresh"}, &river.InsertOpts{Queue: ck.QueueContentKit})
	must(t, err)
	waitForWithin(t, time.Minute, func() bool {
		row, err := queue.JobGet(t.Context(), job.Job.ID)
		must(t, err)
		if row.State == rivertype.JobStateRetryable || row.State == rivertype.JobStateDiscarded {
			t.Fatalf("popular refresh failed: %+v", row.Errors)
		}
		return row.State == rivertype.JobStateCompleted
	})
}

func TestDailyPopularityCountsReadersExactly(t *testing.T) {
	w := newWorld(t)
	a := w.apps[0]
	configureDiscoverySignals(t, w, a)
	database := w.schema + "_signals"
	store, err := signal.NewStore(a.deps.ClickHouse, database)
	must(t, err)
	ctx := t.Context()
	// A large work and a small work share one daily population. The other
	// six available days have no readers and still contribute to the mean.
	must(t, a.deps.ClickHouse.Exec(ctx, `INSERT INTO `+database+`.engagement_segments
 (tenant,content_kind,content_id,content_version_id,subject_kind,subject,receipt_id,revision,visit_id,segment_id,
 occurred_at,active_ms,covered_units,unit_attention,unit_total,unit_scale,resume,access_context,measurement)
 SELECT 'batch','gallery',if(number<25000,'big','small'),'v','user',concat('u',toString(number%25000)),concat('r',toString(number)),1,'v','s',
 toDateTime64('2026-10-20 12:00:00',6,'UTC'),5000,range(10),map(),10,1,'',
 '{"viewer_access":"full","eligibility_known":true,"domain":"main","language":"en"}','{}'
 FROM numbers(25010)`))
	must(t, a.deps.ClickHouse.Exec(ctx, `INSERT INTO `+database+`.measured_population_observations
 (tenant,subject_kind,subject,content_kind,observation_id,observed_at,domain,language,eligible_tiers,eligibility_known)
 SELECT 'batch','user',concat('u',toString(number)),'gallery',toString(number),toDateTime64('2026-10-20 13:00:00',6,'UTC'),'main','en',[],true
 FROM numbers(50000)`))
	from, until := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	scope := signal.ActivityScope{Domain: "main", Language: "en"}
	_, err = store.PrepareDailyPopularity(ctx, "batch", "gallery", signal.Availability{Known: true, From: from, Until: until, Intervals: []signal.AccessInterval{
		{ContentID: "big", From: from, Until: until, Audience: "public"},
		{ContentID: "small", From: from, Until: until, Audience: "public"}}}, scope, false)
	must(t, err)
	hits, err := store.DailyPopular(ctx, "batch", "gallery", signal.Between(from, until), scope, 10)
	must(t, err)
	want := map[string]float64{"big": 25000, "small": 10}
	if len(hits) != len(want) {
		t.Fatalf("ranked %d works; want %d", len(hits), len(want))
	}
	for _, h := range hits {
		readers := want[h.ContentID]
		wantScore := math.Round(readers/50020/7*1e12) / 1e12
		if math.Abs(h.Score-wantScore) > 1e-12*wantScore {
			t.Fatalf("%s score = %v; want %v", h.ContentID, h.Score, wantScore)
		}
	}
	if hits[0].ContentID != "big" {
		t.Fatalf("ranked %s first; want the most-read work", hits[0].ContentID)
	}
}
