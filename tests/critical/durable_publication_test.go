package critical_test

import (
	"context"
	"testing"
	"time"

	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/catalog"
	riverhelpers "github.com/open-rails/helpers/river"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

func TestDurableTextPublication(t *testing.T) {
	w := newWorld(t)
	a := w.apps[0]
	admin, reader := a.engine.Client(), a.client("")
	fleet, err := riverhelpers.New(t.Context(), w.pool, &river.Config{Schema: w.schema + "_river", Queues: map[string]river.QueueConfig{}}, a.engine.RiverJobs())
	must(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		must(t, fleet.Stop(ctx))
	})
	must(t, a.engine.Start(t.Context(), ck.WithRiverClient(fleet)))
	item, err := admin.PostContents(t.Context(), catalog.ContentInput{Kind: "gallery", Text: map[string]catalog.Text{
		"en": {Title: "Durable release", Body: "Publication follows completed work"},
	}})
	must(t, err)
	due := w.clock.now().Add(time.Hour)
	queued, err := admin.PostContentPublish(t.Context(), item.ID, catalog.PublishInput{PublishAt: &due})
	must(t, err)
	if queued.Live || queued.PublishedAt != nil {
		t.Fatal("accepted publication was reported as completed before its worker ran")
	}
	var jobs int
	must(t, w.pool.QueryRow(t.Context(), `SELECT count(*) FROM `+w.schema+`_river.river_job
 WHERE kind='contentkit_content_publish_due' AND args->>'content'=$1 AND scheduled_at=$2`, item.ID, due).Scan(&jobs))
	if jobs != 1 {
		t.Fatalf("accepted publication has %d durable scheduled jobs, want one", jobs)
	}
	actual := due.Add(2 * time.Hour)
	w.clock.set(actual)
	_, err = reader.GetContent(t.Context(), item.ID, ck.GetContentQuery{})
	requireError(t, err, 404, "not_found")
	// An ordinary metadata/access edit used to stamp overdue publication as a
	// side effect. It must leave the durable request pending instead.
	_, err = admin.PatchContent(t.Context(), item.ID, catalog.ContentPatch{Access: &access.Rule{Level: access.TierLevel, Tiers: []string{"gold"}}})
	must(t, err)
	_, err = reader.GetContent(t.Context(), item.ID, ck.GetContentQuery{})
	requireError(t, err, 404, "not_found")
	log, err := admin.ListEvents(t.Context(), catalog.EventQuery{})
	must(t, err)
	for _, event := range log.Events {
		if event.ContentID == item.ID && (event.Type == catalog.EventContentPublished || event.Type == catalog.EventVersionPublished) {
			t.Fatalf("unfinished publication emitted a completed fact: %+v", event)
		}
	}
	// The fixture clock advances independently from River's wall clock. An
	// immediate run of the real registered worker catches up the overdue work.
	job, err := fleet.Insert(t.Context(), scheduledContentJob{kind: "contentkit_content_publish_due"}, &river.InsertOpts{Queue: catalog.Queue})
	must(t, err)
	must(t, fleet.Start(t.Context()))
	waitFor(t, func() bool {
		row, err := fleet.JobGet(t.Context(), job.Job.ID)
		must(t, err)
		if row.State == rivertype.JobStateDiscarded {
			t.Fatalf("publication worker failed: %+v", row.Errors)
		}
		return row.State == rivertype.JobStateCompleted
	})
	published, err := reader.GetContent(t.Context(), item.ID, ck.GetContentQuery{})
	must(t, err)
	if !published.Live || !published.Decision.Allowed || published.Body != "Publication follows completed work" {
		t.Fatalf("completed release or copied edition policy is wrong: %+v", published)
	}
	log, err = admin.ListEvents(t.Context(), catalog.EventQuery{})
	must(t, err)
	counts := map[string]int{}
	for _, event := range log.Events {
		if event.ContentID != item.ID || event.Type != catalog.EventContentPublished && event.Type != catalog.EventVersionPublished {
			continue
		}
		counts[event.Type]++
		if !event.OccurredAt.Equal(actual) {
			t.Fatalf("release used scheduled date instead of actual completion: %+v", event)
		}
	}
	if counts[catalog.EventContentPublished] != 1 || counts[catalog.EventVersionPublished] != 1 {
		t.Fatalf("completed facts = %+v", counts)
	}
	// Changing the gallery default must not alter an existing edition. A new
	// edition copies the current default as its own independent access policy.
	edition, err := admin.GetContentVersion(t.Context(), item.ID, item.Versions[0].ID, ck.GetContentVersionQuery{})
	must(t, err)
	if edition.Access == nil || edition.Access.Level != access.Public {
		t.Fatalf("parent default changed existing edition policy: %+v", edition.Access)
	}
	added, err := admin.PostContentVersions(t.Context(), item.ID, catalog.VersionInput{Languages: []string{"de"}})
	must(t, err)
	if added.Access == nil || added.Access.Level != access.TierLevel {
		t.Fatalf("new edition did not copy its creation default: %+v", added.Access)
	}
	publicAt := actual.Add(time.Hour)
	protected, err := admin.PostContents(t.Context(), catalog.ContentInput{Kind: "gallery",
		Access: &access.Rule{Level: access.TierLevel, Tiers: []string{"gold"}, Releases: []access.Release{{At: publicAt, Level: access.Public}}},
		Text:   map[string]catalog.Text{"en": {Title: "Audience promotion", Body: "Still protected until publication work completes"}},
	})
	must(t, err)
	_, err = admin.PostContentPublish(t.Context(), protected.ID, catalog.PublishInput{})
	must(t, err)
	dueJob, err := fleet.Insert(t.Context(), scheduledContentJob{kind: "contentkit_content_publish_due"}, &river.InsertOpts{Queue: catalog.Queue})
	must(t, err)
	waitFor(t, func() bool {
		row, err := fleet.JobGet(t.Context(), dueJob.Job.ID)
		must(t, err)
		if row.State == rivertype.JobStateDiscarded {
			t.Fatalf("protected publication worker failed: %+v", row.Errors)
		}
		return row.State == rivertype.JobStateCompleted
	})
	locked, err := reader.GetContent(t.Context(), protected.ID, ck.GetContentQuery{})
	must(t, err)
	if !locked.Live || locked.Decision.Allowed || locked.Body != "" {
		t.Fatalf("initial protected publication is incorrect: %+v", locked)
	}
	must(t, fleet.Stop(t.Context()))
	w.clock.set(publicAt.Add(time.Second))
	locked, err = reader.GetContent(t.Context(), protected.ID, ck.GetContentQuery{})
	must(t, err)
	if locked.Decision.Allowed || locked.Body != "" {
		t.Fatal("reader clock opened a future audience without its publication worker")
	}
}
