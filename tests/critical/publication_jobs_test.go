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

type scheduledContentJob struct{ kind string }

func (j scheduledContentJob) Kind() string { return j.kind }

func TestPublicationRecorderAndParentBoundary(t *testing.T) {
	w := newWorld(t)
	a := w.apps[0]
	admin, reader := a.engine.Client(), a.client("")
	base := w.clock.now()
	versionAt, parentAt, audienceAt := base.Add(30*time.Minute), base.Add(time.Hour), base.Add(2*time.Hour)
	item, err := admin.PostContents(t.Context(), catalog.ContentInput{Kind: "gallery",
		Access:   &access.Rule{Level: access.TierLevel, Tiers: []string{"gold"}, Releases: []access.Release{{At: audienceAt, Level: access.Public}}},
		Text:     map[string]catalog.Text{"en": {Title: "Parent embargo", Body: "Embargoed edition"}},
		Versions: []catalog.VersionInput{{State: "published", PublishAt: &versionAt, Languages: []string{"en"}, Text: map[string]catalog.VersionText{"en": {Title: "Embargoed version"}}}}})
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
		locked, err := reader.GetVersion(t.Context(), item.Versions[0].ID, ck.GetVersionQuery{})
		must(t, err)
		if !locked.Live || locked.Body != "" || locked.Decision.Allowed {
			t.Fatalf("parent boundary exposed wrong state: %+v", locked)
		}
	})
	t.Run("CK-J05-S06_concurrent_recorders_catch_up_each_fact_once", func(t *testing.T) {
		w.clock.set(audienceAt)
		open, err := reader.GetVersion(t.Context(), item.Versions[0].ID, ck.GetVersionQuery{})
		must(t, err)
		if !open.Decision.Allowed || open.Body != "Embargoed edition" {
			t.Fatal("reader required a recorder to expose the matured release")
		}
		before, err := admin.ListEvents(t.Context(), catalog.EventQuery{})
		must(t, err)
		for _, event := range before.Events {
			if event.Type == "content.published" || event.Type == "version.published" || event.Type == "version.audience_released" {
				t.Fatalf("unstarted recorder already caught up: %+v", event)
			}
		}
		fleet, err := riverhelpers.New(t.Context(), w.pool, &river.Config{Schema: w.schema + "_river", Queues: map[string]river.QueueConfig{}}, a.engine.RiverJobs())
		must(t, err)
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			must(t, fleet.Stop(ctx))
		})
		must(t, a.engine.Start(t.Context(), ck.WithRiverClient(fleet)))
		// Insert several real jobs before starting their two-worker queue. The
		// schedule's own startup job may race them; all share the same recorder.
		ids := []int64{}
		for range 4 {
			job, err := fleet.Insert(t.Context(), scheduledContentJob{kind: "contentkit_content_publish_due"}, &river.InsertOpts{Queue: catalog.Queue})
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
				if (event.Type == "content.published" || event.Type == "version.published") && !event.OccurredAt.Equal(parentAt) {
					t.Fatalf("recorder changed effective publication instant: %+v", event)
				}
			}
			if counts["content.published"] != 1 || counts["version.published"] != 1 || counts["version.audience_released"] != 1 {
				t.Fatalf("caught-up facts = %+v; expected one of each", counts)
			}
		}
		check()
		job, err := fleet.Insert(t.Context(), scheduledContentJob{kind: "contentkit_content_publish_due"}, &river.InsertOpts{Queue: catalog.Queue})
		must(t, err)
		waitFor(t, func() bool {
			row, err := fleet.JobGet(t.Context(), job.Job.ID)
			must(t, err)
			return row.State == rivertype.JobStateCompleted
		})
		check()
	})
}
