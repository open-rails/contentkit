package critical_test

import (
	"context"
	"testing"
	"time"

	"github.com/open-rails/contentkit/internal/jobhook"

	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/catalog"
	"github.com/riverqueue/river"
)

func TestDurableTextPublication(t *testing.T) {
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
	item, err := admin.PostContents(t.Context(), catalog.ContentInput{Kind: "gallery", Versions: []catalog.VersionInput{{Languages: []string{"en"}, Text: map[string]catalog.VersionText{
		"en": {Title: "Durable release", Body: "Publication follows completed work"},
	}}}})
	must(t, err)
	due := w.clock.now().Add(time.Hour)
	queued, err := admin.PostContentPublish(t.Context(), item.ID, catalog.PublishInput{PublishAt: &due})
	must(t, err)
	if queued.Live || queued.PublishedAt != nil {
		t.Fatal("accepted publication was reported as completed before its worker ran")
	}
	var jobs int
	must(t, w.pool.QueryRow(t.Context(), `SELECT count(*) FROM `+w.schema+`_river.river_job j
 JOIN `+w.schema+`.content_publication_requests r ON r.id::text=j.args->>'id'
 WHERE j.kind='contentkit_version_release' AND r.content_id=$1 AND j.scheduled_at=$2`, item.ID, due).Scan(&jobs))
	if jobs != 1 {
		t.Fatalf("accepted publication has %d durable scheduled jobs, want one", jobs)
	}
	request := currentReleaseJob(t, a, item.Versions[0].ID)
	if !request.Due.Equal(due) {
		t.Fatalf("accepted request due=%v, want %v", request.Due, due)
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
	// Only wake the accepted request; no periodic scan can rescue missing work.
	wakeDuePublicationJobs(t, a)
	must(t, fleet.Start(t.Context()))
	awaitDuePublications(t, a)
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
	must(t, admin.DeleteContentVersion(t.Context(), item.ID, edition.ID))
	awaitDuePublications(t, a)
	// Deletion hides the edition from ordinary readers while retaining its
	// completed publication for exact-key holders until physical purge.
	_, err = reader.GetVersion(t.Context(), edition.ID, ck.GetVersionQuery{})
	requireError(t, err, 404, "not_found")
	_, err = admin.PatchContentVersion(t.Context(), item.ID, edition.ID, catalog.VersionInput{State: catalog.StatePublished})
	must(t, err)
	awaitDuePublications(t, a)
	waitFor(t, func() bool {
		got, err := reader.GetContentVersion(t.Context(), item.ID, edition.ID, ck.GetContentVersionQuery{})
		return err == nil && got.Live
	})
	publicAt := actual.Add(time.Hour)
	protected, err := admin.PostContents(t.Context(), catalog.ContentInput{Kind: "gallery",
		Access: &access.Rule{Level: access.TierLevel, Tiers: []string{"gold"}, Releases: []access.Release{{At: publicAt, Level: access.Public}}}, Versions: []catalog.VersionInput{{Languages: []string{"en"}, Text: map[string]catalog.VersionText{"en": {Title: "Audience promotion", Body: "Still protected until publication work completes"}}}},
	})
	must(t, err)
	_, err = admin.PostContentPublish(t.Context(), protected.ID, catalog.PublishInput{})
	must(t, err)
	awaitDuePublications(t, a)
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
