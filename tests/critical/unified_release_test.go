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

// A post with no storage configuration must retain the same request identity,
// completed reader snapshot and next deadline as a media-backed edition.
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
	initial := currentPublicationJob(t, a, version)
	runPublicationWorker(t, fleet, a)

	var firstPublished time.Time
	must(t, w.pool.QueryRow(t.Context(), `SELECT published_at FROM `+w.schema+`.content_versions WHERE tenant_id=$1 AND id=$2`, publicationTenant(a), version).Scan(&firstPublished))
	assertNext := func(completed publicationJob, at time.Time) publicationJob {
		t.Helper()
		next := currentPublicationJob(t, a, version)
		if next.ID == completed.ID || !next.Due.Equal(at) {
			t.Fatalf("completed request did not preserve its next deadline: previous=%+v next=%+v want=%v", completed, next, at)
		}
		var linked bool
		must(t, w.pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM `+w.schema+`_river.river_job j
 WHERE j.kind='contentkit_publication' AND j.queue=$1 AND j.args->>'id'=$2 AND j.scheduled_at=$3
 AND j.state IN ('available','pending','running','retryable','scheduled'))`, versionstore.QueueFor(publicationTenant(a)), next.ID, at).Scan(&linked))
		if !linked {
			t.Fatal("next release has no durable exact-deadline job")
		}
		var currentReader string
		must(t, w.pool.QueryRow(t.Context(), `SELECT release_id::text FROM `+w.schema+`.content_versions WHERE tenant_id=$1 AND id=$2`, publicationTenant(a), version).Scan(&currentReader))
		if currentReader != completed.ID {
			t.Fatalf("pending future request replaced completed reader: %s, want %s", currentReader, completed.ID)
		}
		return next
	}
	next := assertNext(initial, first)
	// Reaccept unchanged publication while its future audience is pending.
	// Completing an equivalent plan must retain the reader and rearm the date.
	_, err = a.engine.Client().PostContentPublish(t.Context(), item.ID, catalog.PublishInput{})
	must(t, err)
	noop := currentPublicationJob(t, a, version)
	if noop.Due.After(w.clock.now()) {
		t.Fatal("unchanged publication did not accept immediate reconciliation")
	}
	runPublicationWorker(t, fleet, a)
	next = assertNext(initial, first)
	// The completed no-op holds no copy of the release, and the next
	// obligation superseding it removes its row.
	var noopKept bool
	must(t, w.pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM `+w.schema+`.content_publication_requests
 WHERE tenant_id=$1 AND id=$2)`, publicationTenant(a), noop.ID).Scan(&noopKept))
	if noopKept || next.ID == noop.ID {
		t.Fatal("no-op release did not complete and give way to a separate next obligation")
	}
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
		// Startup can reconstruct bindings, but no publication polling exists.
		// The original exact request must survive an engine/fleet restart.
		restart()
		if retained := currentPublicationJob(t, a, version); retained != next {
			t.Fatalf("restart replaced admitted request: got=%+v want=%+v", retained, next)
		}
		w.clock.set(due.Add(time.Second))
		before, err := a.client("").GetVersion(t.Context(), version, ck.GetVersionQuery{})
		must(t, err)
		if before.Decision.Allowed {
			t.Fatal("elapsed deadline granted public access before worker completion")
		}
		runPublicationWorker(t, fleet, a)
		var released string
		var published time.Time
		must(t, w.pool.QueryRow(t.Context(), `SELECT release_id::text,published_at FROM `+w.schema+`.content_versions WHERE tenant_id=$1 AND id=$2`, publicationTenant(a), version).Scan(&released, &published))
		if released != next.ID || !published.Equal(firstPublished) {
			t.Fatalf("release changed identity or first-publication timestamp: id=%s published=%v", released, published)
		}
		if index == 0 {
			next = assertNext(next, second)
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
	// At-least-once delivery of the last completed request must emit nothing.
	job, err := fleet.Insert(t.Context(), next, &river.InsertOpts{Queue: versionstore.QueueFor(publicationTenant(a))})
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
			t.Fatalf("request replay duplicated %s: before=%d after=%d", kind, n, after[kind])
		}
	}
}
