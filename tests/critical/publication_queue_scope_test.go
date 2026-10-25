package critical_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/media/versions"
	riverhelpers "github.com/open-rails/helpers/river"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertype"
)

func TestPublicationQueuesIsolateSharedCatalogApps(t *testing.T) {
	w := newWorld(t)
	a, b := w.apps[0], w.apps[1]
	makeFleet := func(app *application) *river.Client[pgx.Tx] {
		t.Helper()
		fleet, err := riverhelpers.New(t.Context(), w.pool, &river.Config{Schema: w.schema + "_river", Queues: map[string]river.QueueConfig{}}, app.engine.RiverJobs())
		must(t, err)
		must(t, app.engine.Start(t.Context(), ck.WithRiverClient(fleet)))
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
		item, err := app.engine.Client().PostContents(t.Context(), catalog.ContentInput{Kind: "article", Text: map[string]catalog.Text{"en": {Title: name, Body: name}}})
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
		must(t, w.pool.QueryRow(t.Context(), `SELECT queue FROM `+w.schema+`_river.river_job
 WHERE kind='contentkit_content_publish_due' AND args->>'content'=$1 ORDER BY id LIMIT 1`, id).Scan(queue))
	}
	if qa != catalog.QueueFor(a.cfg.Scope.AppID.String()) || qb != catalog.QueueFor(b.cfg.Scope.AppID.String()) || qa == qb {
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
	must(t, w.pool.QueryRow(t.Context(), `SELECT state FROM `+w.schema+`_river.river_job
 WHERE kind='contentkit_content_publish_due' AND args->>'content'=$1 ORDER BY id LIMIT 1`, ib.ID).Scan(&state))
	if state != rivertype.JobStateAvailable && state != rivertype.JobStateScheduled {
		t.Fatalf("app A consumed app B's durable publication: %s", state)
	}
	must(t, fb.Start(t.Context()))
	waitFor(t, func() bool {
		got, err := b.client("").GetContent(t.Context(), ib.ID, ck.GetContentQuery{})
		return err == nil && got.Live
	})
}

type publicationMigrationJob struct {
	kind string
	args json.RawMessage
}

func (j publicationMigrationJob) Kind() string                 { return j.kind }
func (j publicationMigrationJob) MarshalJSON() ([]byte, error) { return j.args, nil }

func TestPublicationQueueMigrationPreservesOwners(t *testing.T) {
	w := newWorld(t)
	a, b := w.apps[0], w.apps[1]
	create := func(app *application) catalog.Content {
		t.Helper()
		item, err := app.engine.Client().PostContents(t.Context(), catalog.ContentInput{Kind: "gallery", Text: map[string]catalog.Text{"en": {Title: "Queue migration"}}})
		must(t, err)
		return item
	}
	ia, ib := create(a), create(b)
	client, err := river.NewClient(riverpgxv5.New(w.pool), &river.Config{Schema: w.schema + "_river"})
	must(t, err)
	insert := func(kind, queue string, args any) int64 {
		t.Helper()
		raw, err := json.Marshal(args)
		must(t, err)
		job, err := client.Insert(t.Context(), publicationMigrationJob{kind: kind, args: raw}, &river.InsertOpts{Queue: queue})
		must(t, err)
		return job.Job.ID
	}
	jA := insert("contentkit_version_publish", versions.Queue, map[string]string{"content": ia.ID, "version": ia.Versions[0].ID})
	jB := insert("contentkit_version_publish", versions.Queue, map[string]string{"content": ib.ID, "version": ib.Versions[0].ID})
	foreign := insert("contentkit_version_publish", "other_application_queue", map[string]string{"content": ia.ID, "version": ia.Versions[0].ID})
	periodic := insert("contentkit_content_publish_due", catalog.Queue, map[string]string{})
	completed := insert("contentkit_version_publish", versions.Queue, map[string]string{"content": ia.ID, "version": ia.Versions[0].ID})
	_, err = w.pool.Exec(t.Context(), `UPDATE `+w.schema+`_river.river_job SET state='completed',finalized_at=now() WHERE id=$1`, completed)
	must(t, err)
	_, err = w.pool.Exec(t.Context(), `UPDATE `+w.schema+`_river.river_job SET state='running',attempted_at=now() WHERE id=$1`, jA)
	must(t, err)
	err = ck.MigratePostgres(t.Context(), w.pool, a.cfg)
	if err == nil || !strings.Contains(err.Error(), "stop legacy publication workers") {
		t.Fatalf("running legacy publisher did not block migration: %v", err)
	}
	_, err = w.pool.Exec(t.Context(), `UPDATE `+w.schema+`_river.river_job SET state='available' WHERE id=$1`, jA)
	must(t, err)
	must(t, ck.MigratePostgres(t.Context(), w.pool, a.cfg))
	for id, queue := range map[int64]string{
		jA: versions.QueueFor(a.cfg.Scope.AppID.String()), jB: versions.QueueFor(b.cfg.Scope.AppID.String()),
		foreign: "other_application_queue", completed: versions.Queue,
	} {
		job, err := client.JobGet(t.Context(), id)
		must(t, err)
		if job.Queue != queue {
			t.Fatalf("job %d moved to %s, expected %s", id, job.Queue, queue)
		}
	}
	old, err := client.JobGet(t.Context(), periodic)
	must(t, err)
	if old.State != rivertype.JobStateCancelled {
		t.Fatalf("unscoped periodic row was not retired after replacement: %s", old.State)
	}
	var count int
	must(t, w.pool.QueryRow(t.Context(), `SELECT count(DISTINCT queue) FROM `+w.schema+`_river.river_job
 WHERE kind='contentkit_content_publish_due' AND queue<>$1 AND state='available'`, catalog.Queue).Scan(&count))
	if count != len(w.apps) {
		t.Fatalf("periodic obligation assigned to %d app queues, want %d", count, len(w.apps))
	}
	must(t, ck.MigratePostgres(t.Context(), w.pool, a.cfg))
}
