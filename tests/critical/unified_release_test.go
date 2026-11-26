package critical_test

import (
	"context"
	"testing"
	"time"

	versionstore "github.com/open-rails/contentkit/internal/versionstore"

	"github.com/open-rails/contentkit/internal/jobhook"

	"github.com/jackc/pgx/v5"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/catalog"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

// A post with no storage configuration has one release job, as a
// media-backed edition does: it waits for each audience deadline, survives
// an engine restart, opens each audience only when it runs, and a replay
// emits nothing.
func TestReleaseRequestsSurviveRestartAndSequentialDeadlines(t *testing.T) {
	w := newWorld(t)
	a := w.apps[0]
	var fleet *river.Client[pgx.Tx]
	bind := func() {
		t.Helper()
		var err error
		fleet, err = jobhook.Fleet(t.Context(), a.engine, &river.Config{Schema: publicationRiverSchema(a), Queues: map[string]river.QueueConfig{}})
		must(t, err)
	}
	bind()
	t.Cleanup(func() { must(t, fleet.Stop(context.Background())) })
	first, second := w.clock.now().Add(time.Hour), w.clock.now().Add(2*time.Hour)
	item, err := a.engine.Client().PostContents(t.Context(), catalog.ContentInput{Kind: "article",
		Access: &access.Rule{Level: access.PPV, Releases: []access.Release{
			{At: first, Level: access.TierLevel, Tiers: []string{"gold"}}, {At: second, Level: access.Public},
		}}, Versions: []catalog.VersionInput{{Languages: []string{"en"}, Text: map[string]catalog.VersionText{"en": {Title: "A post through two releases", Body: "No storage service is configured."}}}},
	})
	must(t, err)
	_, err = a.engine.Client().PostContentPublish(t.Context(), item.ID, catalog.PublishInput{})
	must(t, err)
	version := item.Versions[0].ID
	runPublicationWorker(t, fleet, a)

	var firstPublished time.Time
	must(t, w.pool.QueryRow(t.Context(), `SELECT published_at FROM `+w.schema+`.content_versions WHERE tenant_id=$1 AND id=$2`, publicationTenant(a), version).Scan(&firstPublished))
	waiting := func() publicationJob {
		t.Helper()
		job := currentPublicationJob(t, a, version)
		if job.ID == 0 || job.State != "scheduled" || !job.Due.After(time.Now()) {
			t.Fatalf("the release is not waiting for its next deadline: %+v", job)
		}
		return job
	}
	next := waiting()
	// Reaccepting unchanged publication runs the release now; it plans the
	// same deadline and waits again.
	_, err = a.engine.Client().PostContentPublish(t.Context(), item.ID, catalog.PublishInput{})
	must(t, err)
	runPublicationWorker(t, fleet, a)
	next = waiting()
	restart := func() {
		t.Helper()
		must(t, fleet.Stop(t.Context()))
		must(t, a.engine.Close(t.Context()))
		a.server.Close()
		cfg := a.cfg
		cfg.Database.SkipMigrations = true
		a.engine, err = ck.New(t.Context(), cfg, a.deps)
		must(t, err)
		a.server = mountEngine(t, a.engine)
		bind()
	}
	for index, due := range []time.Time{first, second} {
		// No publication polling exists: the waiting job survives a restart.
		restart()
		if retained := currentPublicationJob(t, a, version); retained.ID != next.ID {
			t.Fatalf("restart replaced the release job: got=%+v want=%+v", retained, next)
		}
		w.clock.set(due.Add(time.Second))
		before, err := a.client("").GetVersion(t.Context(), version, ck.GetVersionQuery{})
		must(t, err)
		if before.Decision.Allowed {
			t.Fatal("elapsed deadline granted public access before worker completion")
		}
		runPublicationWorker(t, fleet, a)
		var published time.Time
		must(t, w.pool.QueryRow(t.Context(), `SELECT published_at FROM `+w.schema+`.content_versions WHERE tenant_id=$1 AND id=$2`, publicationTenant(a), version).Scan(&published))
		if !published.Equal(firstPublished) {
			t.Fatalf("a later release changed the first-publication timestamp: %v", published)
		}
		if index == 0 {
			next = waiting()
		}
	}
	open, err := a.client("").GetVersion(t.Context(), version, ck.GetVersionQuery{})
	must(t, err)
	if !open.Decision.Allowed || open.Body != "No storage service is configured." {
		t.Fatalf("final exact release did not open post: %+v", open)
	}
	facts := func() map[string]int {
		t.Helper()
		page, err := a.engine.Client().ListEvents(t.Context(), catalog.EventQuery{})
		must(t, err)
		counts := map[string]int{}
		for _, event := range page.Events {
			if event.ContentID == item.ID {
				counts[event.Type]++
			}
		}
		return counts
	}
	before := facts()
	if before[catalog.EventContentPublished] != 1 || before[catalog.EventVersionPublished] != 1 || before[catalog.EventAudienceReleased] != 2 || before[catalog.EventPublicationCompleted] != 3 {
		t.Fatalf("sequential release facts=%v", before)
	}
	// At-least-once delivery of the last release must emit nothing.
	job, err := fleet.Insert(t.Context(), releaseArgs{Content: item.ID, Version: version}, &river.InsertOpts{Queue: versionstore.QueueFor(publicationTenant(a))})
	must(t, err)
	must(t, fleet.Start(t.Context()))
	waitFor(t, func() bool {
		row, err := fleet.JobGet(t.Context(), job.Job.ID)
		must(t, err)
		if row.State == rivertype.JobStateDiscarded {
			t.Fatalf("completed replay failed: %+v", row.Errors)
		}
		return row.State == rivertype.JobStateCompleted
	})
	must(t, fleet.Stop(t.Context()))
	after := facts()
	for kind, n := range before {
		if after[kind] != n {
			t.Fatalf("release replay duplicated %s: before=%d after=%d", kind, n, after[kind])
		}
	}
}
