package critical_test

import (
	"context"
	"testing"
	"time"

	versionstore "github.com/open-rails/contentkit/internal/versionstore"

	"github.com/open-rails/contentkit/internal/jobhook"

	"github.com/jackc/pgx/v5"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/catalog"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

func TestPublicationQueuesIsolateSharedCatalogApps(t *testing.T) {
	w := newWorld(t)
	a, b := w.apps[0], w.apps[1]
	makeFleet := func(app *application) *river.Client[pgx.Tx] {
		t.Helper()
		fleet, err := jobhook.Fleet(t.Context(), app.engine, &river.Config{Schema: w.schema + "_river", Queues: map[string]river.QueueConfig{}})
		must(t, err)
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			must(t, fleet.Stop(ctx))
		})
		return fleet
	}
	fa, fb := makeFleet(a), makeFleet(b)
	create := func(app *application, name string) catalog.Content {
		t.Helper()
		item, err := app.engine.Client().PostContents(t.Context(), catalog.ContentInput{Kind: "article", Versions: []catalog.VersionInput{{Languages: []string{"en"}, Text: map[string]catalog.VersionText{"en": {Title: name, Body: name}}}}})
		must(t, err)
		_, err = app.engine.Client().PostContentPublish(t.Context(), item.ID, catalog.PublishInput{})
		must(t, err)
		return item
	}
	// B's job is older. With the legacy shared queue A would consume it first
	// and complete/cancel it using A's bound catalog instead of doing B's work.
	ib, ia := create(b, "App B publication"), create(a, "App A publication")
	var qa, qb string
	for id, queue := range map[string]*string{ia.ID: &qa, ib.ID: &qb} {
		must(t, w.pool.QueryRow(t.Context(), `SELECT j.queue FROM `+w.schema+`_river.river_job j
 JOIN `+w.schema+`.content_publication_requests r ON r.id::text=j.args->>'id'
 WHERE j.kind='contentkit_version_release' AND r.content_id=$1 ORDER BY j.id LIMIT 1`, id).Scan(queue))
	}
	if qa != versionstore.QueueFor(publicationTenant(a)) || qb != versionstore.QueueFor(publicationTenant(b)) || qa == qb {
		t.Fatalf("publication queues do not preserve app ownership: A=%s B=%s", qa, qb)
	}
	must(t, fa.Start(t.Context()))
	waitFor(t, func() bool {
		got, err := a.client("").GetContent(t.Context(), ia.ID, ck.GetContentQuery{})
		return err == nil && got.Live
	})
	_, err := b.client("").GetContent(t.Context(), ib.ID, ck.GetContentQuery{})
	requireError(t, err, 404, "not_found")
	var state rivertype.JobState
	must(t, w.pool.QueryRow(t.Context(), `SELECT j.state FROM `+w.schema+`_river.river_job j
 JOIN `+w.schema+`.content_publication_requests r ON r.id::text=j.args->>'id'
 WHERE j.kind='contentkit_version_release' AND r.content_id=$1 ORDER BY j.id LIMIT 1`, ib.ID).Scan(&state))
	if state != rivertype.JobStateAvailable && state != rivertype.JobStateScheduled {
		t.Fatalf("app A consumed app B's durable publication: %s", state)
	}
	must(t, fb.Start(t.Context()))
	waitFor(t, func() bool {
		got, err := b.client("").GetContent(t.Context(), ib.ID, ck.GetContentQuery{})
		return err == nil && got.Live
	})
}
