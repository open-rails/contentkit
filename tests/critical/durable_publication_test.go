package critical_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/open-rails/contentkit/internal/jobhook"
	mediastore "github.com/open-rails/contentkit/internal/mediastore"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/internal/media/workqueue"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/layout"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertype"
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
	// One release job per version: accepting publication wakes it, and its
	// run plans the due time from the catalog.
	unfinished := func(version string) (jobs int) {
		t.Helper()
		must(t, w.pool.QueryRow(t.Context(), `SELECT count(*) FROM `+w.schema+`_river.river_job
 WHERE kind='contentkit_publication' AND args->>'version'=$1 AND state NOT IN ('completed','cancelled','discarded')`, version).Scan(&jobs))
		return jobs
	}
	request := currentPublicationJob(t, a, item.Versions[0].ID)
	if request.ID == 0 || unfinished(item.Versions[0].ID) != 1 {
		t.Fatalf("accepted publication has %d release jobs, want one", unfinished(item.Versions[0].ID))
	}
	for _, at := range []time.Time{due.Add(time.Hour), due.Add(-30 * time.Minute)} {
		_, err = admin.PostContentPublish(t.Context(), item.ID, catalog.PublishInput{PublishAt: &at})
		must(t, err)
		if next := currentPublicationJob(t, a, item.Versions[0].ID); next.ID != request.ID || unfinished(item.Versions[0].ID) != 1 {
			t.Fatalf("rescheduling made job %+v, want the version's one job %d", next, request.ID)
		}
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
	must(t, fleet.Stop(t.Context()))
	must(t, admin.DeleteContentVersion(t.Context(), item.ID, edition.ID))
	if currentPublicationJob(t, a, edition.ID).ID == 0 || unfinished(edition.ID) != 1 {
		t.Fatalf("deletion woke %d release jobs, want one", unfinished(edition.ID))
	}
	must(t, fleet.Start(t.Context()))
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

// A manifest that fails validation fails every attempt alike. Its release
// job is cancelled after the first failure instead of retrying, keeping the
// error.
func TestInvalidManifestPublicationIsNotRetried(t *testing.T) {
	w := newWorld(t)
	a := w.apps[0]
	storage := attachMedia(t, w, a, func(c *ck.Config) {
		c.Media.Config.Kinds[0].Publish = &media.Publish{Pages: "pages/{name}", Renditions: []string{"reader"}}
	})
	admin := a.engine.Client()
	ref := galleryRef(t, admin, "Invalid manifest")
	_, err := admin.IngestMedia(t.Context(), http.DefaultClient, ref, uuid.NewString(), []ck.MediaFile{{Path: "pages/one.png", Type: "image/png", Body: bytes.NewReader(imageBytes(t))}})
	must(t, err)
	awaitMediaReady(t, admin, ref)

	config, err := a.cfg.MediaKinds()
	must(t, err)
	registry, err := mediastore.NewRegistry(config, mediastore.Hooks{})
	must(t, err)
	target, err := registry.VersionRef(ref.Kind, ref.ID, ref.Version)
	must(t, err)
	item, err := registry.Item(target)
	must(t, err)
	queue, err := river.NewClient(riverpgxv5.New(w.pool), &river.Config{Schema: a.cfg.Media.WorkerSchema})
	must(t, err)
	var manifest *mediastore.Manifest
	var i int
	// The editor view is written after the page is ready, and other derivative
	// work may still be editing the manifest: read it only once no media job is
	// in flight, so the raw rewrite below is the last write.
	waitFor(t, func() bool {
		jobs, err := queue.JobList(t.Context(), river.NewJobListParams().Kinds((workqueue.IngestArgs{}).Kind(), (workqueue.DerivativeArgs{}).Kind()).States(
			rivertype.JobStateAvailable, rivertype.JobStateRunning, rivertype.JobStateRetryable, rivertype.JobStateScheduled, rivertype.JobStatePending).First(1))
		must(t, err)
		if len(jobs.Jobs) > 0 {
			return false
		}
		body, _, err := storage.store.Get(t.Context(), item.ManifestKey(), media.GetOptions{})
		must(t, err)
		stored, err := io.ReadAll(body)
		body.Close()
		must(t, err)
		manifest, err = mediastore.DecodeManifest(stored)
		must(t, err)
		i = manifest.Find("pages/one.png")
		return i >= 0 && manifest.Files[i].Editor != nil
	})
	// An editor view in an area the layout does not accept, as written before
	// the future/ area was renamed.
	_, name, _ := layout.SplitBlobRef(manifest.Files[i].Editor.Blob)
	manifest.Files[i].Editor.Blob = "future/" + name
	rewritten, err := json.Marshal(manifest)
	must(t, err)
	_, err = storage.store.Put(t.Context(), item.ManifestKey(), bytes.NewReader(rewritten), int64(len(rewritten)), media.PutOptions{ContentType: "application/json"})
	must(t, err)
	// Reads trust the item cache's revision: drop its row so the release
	// reads the rewritten manifest.
	_, err = w.pool.Exec(t.Context(), `DELETE FROM `+pgx.Identifier{w.schema, "content_media_items"}.Sanitize()+` WHERE content_version_id=$1`, ref.Version)
	must(t, err)

	_, err = admin.PostContentPublish(t.Context(), ref.ID, catalog.PublishInput{})
	must(t, err)
	var job, failure string
	var attempts int
	waitForWithin(t, 45*time.Second, func() bool {
		wakeDuePublicationJobs(t, a)
		must(t, w.pool.QueryRow(t.Context(), `SELECT state,attempt,coalesce(errors[array_length(errors,1)]->>'error','') FROM `+pgx.Identifier{publicationRiverSchema(a), "river_job"}.Sanitize()+`
 WHERE kind='contentkit_publication' AND args->>'version'=$1 ORDER BY id DESC LIMIT 1`, ref.Version).Scan(&job, &attempts, &failure))
		return job != "available" && job != "running"
	})
	if job != "cancelled" || attempts != 1 || !strings.Contains(failure, "editor") {
		t.Fatalf("invalid manifest publication: job %s after %d attempts, error %q; want a cancelled job after one attempt keeping its error", job, attempts, failure)
	}
}
