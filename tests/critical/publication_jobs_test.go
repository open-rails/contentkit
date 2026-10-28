package critical_test

import (
	"context"
	"testing"
	"time"

	versionstore "github.com/open-rails/contentkit/internal/versionstore"

	"github.com/open-rails/contentkit/internal/jobhook"

	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/catalog"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

type scheduledContentJob struct{ kind string }

func (j scheduledContentJob) Kind() string { return j.kind }

func TestPublicationRecorderAndParentBoundary(t *testing.T) {
	w := newWorld(t)
	a := w.apps[0]
	admin, reader := a.engine.Client(), a.client("")
	fleet, err := jobhook.Fleet(t.Context(), a.engine, &river.Config{Schema: w.schema + "_river", Queues: map[string]river.QueueConfig{}})
	must(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		must(t, fleet.Stop(ctx))
	})
	queue := versionstore.QueueFor(publicationTenant(a))
	base := w.clock.now()
	versionAt, parentAt, audienceAt := base.Add(30*time.Minute), base.Add(time.Hour), base.Add(2*time.Hour)
	item, err := admin.PostContents(t.Context(), catalog.ContentInput{Kind: "gallery",
		Access:   &access.Rule{Level: access.TierLevel, Tiers: []string{"gold"}, Releases: []access.Release{{At: audienceAt, Level: access.Public}}},
		Versions: []catalog.VersionInput{{State: "published", PublishAt: &versionAt, Languages: []string{"en"}, Text: map[string]catalog.VersionText{"en": {Title: "Embargoed version", Body: "Embargoed edition"}}}}})
	must(t, err)
	_, err = admin.PostContentPublish(t.Context(), item.ID, catalog.PublishInput{PublishAt: &parentAt})
	must(t, err)
	t.Run("CK-J05-S03_version_does_not_publish_before_parent", func(t *testing.T) {
		for _, at := range []time.Time{base, parentAt.Add(-time.Microsecond)} {
			w.clock.set(at)
			_, err := reader.GetVersion(t.Context(), item.Versions[0].ID, ck.GetVersionQuery{})
			requireError(t, err, 404, "not_found")
			_, err = reader.GetContentVersion(t.Context(), item.ID, item.Versions[0].ID, ck.GetContentVersionQuery{})
			requireError(t, err, 404, "not_found")
		}
		w.clock.set(parentAt)
		_, err := reader.GetVersion(t.Context(), item.Versions[0].ID, ck.GetVersionQuery{})
		requireError(t, err, 404, "not_found")
		// Reschedule before execution. A previously queued date must never
		// publish against the newer accepted parent boundary.
		later := parentAt.Add(30 * time.Minute)
		_, err = admin.PostContentPublish(t.Context(), item.ID, catalog.PublishInput{PublishAt: &later})
		must(t, err)
		_, err = reader.GetVersion(t.Context(), item.Versions[0].ID, ck.GetVersionQuery{})
		requireError(t, err, 404, "not_found")
	})
	t.Run("CK-J05-S06_concurrent_recorders_catch_up_each_fact_once", func(t *testing.T) {
		actual := audienceAt.Add(time.Hour)
		w.clock.set(actual)
		_, err := reader.GetVersion(t.Context(), item.Versions[0].ID, ck.GetVersionQuery{})
		requireError(t, err, 404, "not_found")
		before, err := admin.ListEvents(t.Context(), catalog.EventQuery{})
		must(t, err)
		for _, event := range before.Events {
			if event.Type == "content.published" || event.Type == "version.published" || event.Type == "version.audience_released" {
				t.Fatalf("unstarted recorder already caught up: %+v", event)
			}
		}
		// Duplicate delivery of one accepted request races on the same durable
		// lifecycle. No broad scan or startup timer is needed to recover it.
		request := currentReleaseJob(t, a, item.Versions[0].ID)
		wakeDuePublicationJobs(t, a)
		ids := []int64{}
		for range 4 {
			job, err := fleet.Insert(t.Context(), request, &river.InsertOpts{Queue: queue})
			must(t, err)
			ids = append(ids, job.Job.ID)
		}
		must(t, fleet.Start(t.Context()))
		waitFor(t, func() bool {
			for _, id := range ids {
				job, err := fleet.JobGet(t.Context(), id)
				must(t, err)
				if job.State == rivertype.JobStateDiscarded {
					t.Fatalf("due recorder failed: %+v", job.Errors)
				}
				if job.State != rivertype.JobStateCompleted {
					return false
				}
			}
			return true
		})
		open, err := reader.GetVersion(t.Context(), item.Versions[0].ID, ck.GetVersionQuery{})
		must(t, err)
		if !open.Live || !open.Decision.Allowed || open.Body != "Embargoed edition" {
			t.Fatal("completed publication did not open the due audience")
		}
		check := func() {
			t.Helper()
			page, err := admin.ListEvents(t.Context(), catalog.EventQuery{After: before.NextCursor})
			must(t, err)
			counts := map[string]int{}
			for _, event := range page.Events {
				if event.ContentID != item.ID {
					continue
				}
				counts[event.Type]++
				if (event.Type == "content.published" || event.Type == "version.published") && !event.OccurredAt.Equal(actual) {
					t.Fatalf("publication did not record actual completed instant: %+v", event)
				}
			}
			if counts["content.published"] != 1 || counts["version.published"] != 1 || counts["version.audience_released"] != 1 || counts["version.publication_completed"] != 1 {
				t.Fatalf("caught-up facts = %+v; expected one of each", counts)
			}
		}
		check()
		job, err := fleet.Insert(t.Context(), request, &river.InsertOpts{Queue: queue})
		must(t, err)
		waitFor(t, func() bool {
			row, err := fleet.JobGet(t.Context(), job.Job.ID)
			must(t, err)
			return row.State == rivertype.JobStateCompleted
		})
		check()
	})
}
