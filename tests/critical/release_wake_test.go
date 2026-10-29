package critical_test

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/kinds"
	"github.com/open-rails/contentkit/media"
)

// Partial processing can never be released, so the worker wakes a release only
// once an item's processing settles. While a multi-page gallery and a video
// encoded in several chunk checkpoints process, no release plan parks as
// waiting; both release promptly once processing settles, and a released
// edition still re-plans at once when processing changes its pages.
func TestReleaseWakesWhenProcessingSettles(t *testing.T) {
	m := newResilientMedia(t, func(c *ck.Config) {
		c.Media.Kinds[0].Publish = &media.Publish{Pages: "pages/{name}", Renditions: []string{"reader"}}
		c.Content.Kinds["clip"] = kinds.Kind{Route: "clips", Presentation: "player", Versions: kinds.VersionRules{Languages: "one", Hidden: true}}
		c.Media.Config.Kinds = append(c.Media.Config.Kinds, media.Kind{Name: "clip", KeepOriginals: true,
			Uploads: []media.Upload{{Path: "source", Types: []string{"video/mp4"}, MaxBytes: 10 << 20}},
			Private: []media.Private{{Name: "stream", From: "source", To: "streams/", HLS: &media.HLS{Ladder: []int{360, 240, 144}}}},
		})
	})
	admin, reader := m.app.engine.Client(), m.app.client("")
	tenant := publicationTenant(m.app)
	requests := pgx.Identifier{m.schema, "content_publication_requests"}.Sanitize()
	runs, record := pgx.Identifier{m.schema, "critical_release_plan_runs"}.Sanitize(), pgx.Identifier{m.schema, "critical_record_release_plan_run"}.Sanitize()
	// A wake-up moves the current request to pending; a planning run moves it
	// from pending to waiting, running or completed (nothing changed).
	_, err := m.pool.Exec(t.Context(), `CREATE TABLE `+runs+` (id bigserial, version_id text NOT NULL, before text NOT NULL, after text NOT NULL);
CREATE FUNCTION `+record+`() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  INSERT INTO `+runs+` (version_id, before, after) VALUES (NEW.version_id, OLD.state, NEW.state);
  RETURN NEW;
END $$;
CREATE TRIGGER critical_record_release_plan_run AFTER UPDATE OF state ON `+requests+`
 FOR EACH ROW EXECUTE FUNCTION `+record+`()`)
	must(t, err)
	mark := func() int64 {
		var id int64
		must(t, m.pool.QueryRow(t.Context(), `SELECT coalesce(max(id),0) FROM `+runs).Scan(&id))
		return id
	}
	since := func(from int64, version string) (wakes, plans, waiting int) {
		must(t, m.pool.QueryRow(t.Context(), `SELECT count(*) FILTER (WHERE after='pending'),
 count(*) FILTER (WHERE before IN ('pending','failed') AND after IN ('waiting','running','completed')),count(*) FILTER (WHERE before IN ('pending','failed') AND after='waiting')
 FROM `+runs+` WHERE id>$1 AND version_id=$2`, from, version).Scan(&wakes, &plans, &waiting))
		return wakes, plans, waiting
	}
	parked := func(version string) bool {
		var state string
		must(t, m.pool.QueryRow(t.Context(), `SELECT state FROM `+requests+` WHERE tenant_id=$1 AND version_id=$2 AND current`, tenant, version).Scan(&state))
		return state == "waiting"
	}
	livePages := func(version string) (string, map[string]string) {
		var id string
		var pages []struct{ Path, Source string }
		must(t, m.pool.QueryRow(t.Context(), `SELECT coalesce(v.release_id::text,''),coalesce(r.result->'pages'->'pages','[]') FROM `+pgx.Identifier{m.schema, "content_versions"}.Sanitize()+` v
 LEFT JOIN `+requests+` r ON r.tenant_id=v.tenant_id AND r.id=v.release_id WHERE v.tenant_id=$1 AND v.id=$2`, tenant, version).Scan(&id, &pages))
		sources := map[string]string{}
		for _, p := range pages {
			sources[p.Path] = p.Source
		}
		return id, sources
	}
	served := func(ref media.RefBody) []string {
		view, err := reader.GetMediaVersion(t.Context(), ref.Kind, ref.ID, ref.Version, ck.GetMediaVersionQuery{})
		if err != nil {
			return nil
		}
		var paths []string
		for _, file := range view.Files {
			if file.URL != "" {
				paths = append(paths, file.Path)
			}
		}
		slices.Sort(paths)
		return paths
	}

	// Both editions are published before any processing runs, so each first
	// plans once and parks until its media is ready.
	gallery := galleryRef(t, admin, "Wakes on settle")
	_, err = admin.IngestMedia(t.Context(), http.DefaultClient, gallery, uuid.NewString(), []ck.MediaFile{
		{Path: "pages/01.png", Type: "image/png", Body: bytes.NewReader(chapterImage(t, 31))},
		{Path: "pages/02.png", Type: "image/png", Body: bytes.NewReader(chapterImage(t, 97))},
		{Path: "pages/03.png", Type: "image/png", Body: bytes.NewReader(chapterImage(t, 151))},
		{Path: "pages/04.png", Type: "image/png", Body: bytes.NewReader(chapterImage(t, 211))},
	})
	must(t, err)
	source := filepath.Join(t.TempDir(), "source.mp4")
	command := exec.CommandContext(t.Context(), "ffmpeg", "-nostdin", "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=640x360:rate=12", "-t", "2", "-an", "-c:v", "libx264", "-threads", "1", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-movflags", "+faststart", source)
	if out, err := command.CombinedOutput(); err != nil {
		t.Fatalf("create real video source: %v %s", err, out)
	}
	video, err := os.ReadFile(source)
	must(t, err)
	item, err := admin.PostContents(t.Context(), catalog.ContentInput{Kind: "clip", Versions: []catalog.VersionInput{{Languages: []string{"en"}, Text: map[string]catalog.VersionText{"en": {Title: "Wakes on settle"}}}}})
	must(t, err)
	clip := media.RefBody{Kind: "clip", ID: item.ID, Version: item.Versions[0].ID}
	_, err = admin.IngestMedia(t.Context(), http.DefaultClient, clip, uuid.NewString(), []ck.MediaFile{{Path: "source.mp4", Type: "video/mp4", Body: bytes.NewReader(video)}})
	must(t, err)
	for _, ref := range []media.RefBody{gallery, clip} {
		_, err = admin.PostContentPublish(t.Context(), ref.ID, catalog.PublishInput{})
		must(t, err)
		waitForWithin(t, 45*time.Second, func() bool { return parked(ref.Version) })
	}
	start := mark()
	// Real renders and chunk encodes take longer than the few seconds a woken
	// release waits for River to schedule it. Hold each processing output
	// until every due release has run, so a wake-up during processing shows
	// up as the planning run it costs. Release copies are never held.
	stop, paced := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() { close(stop); <-paced })
	go func() {
		defer close(paced)
		for {
			hold := m.wire.pause(t, func(r *http.Request) bool {
				return r.Method == http.MethodPut && r.Header.Get("X-Amz-Copy-Source") == "" && !strings.HasSuffix(r.URL.Path, "manifest.json") &&
					(strings.Contains(r.URL.Path, "/internal/") || strings.Contains(r.URL.Path, "/tmp/"))
			})
			select {
			case <-stop:
				hold.unblock()
				return
			case <-hold.arrived:
			}
			for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
				var due int
				if err := m.pool.QueryRow(context.Background(), `SELECT count(*) FROM `+pgx.Identifier{m.schema + "_river", "river_job"}.Sanitize()+`
 WHERE kind='contentkit_publication' AND state IN ('available','running','retryable','scheduled') AND scheduled_at<=clock_timestamp()`).Scan(&due); err != nil || due == 0 {
					break
				}
			}
			hold.unblock()
		}
	}()
	startVideoFailureWorker(t, m)

	awaitMediaReady(t, admin, gallery)
	pages := []string{"reader/01.webp", "reader/02.webp", "reader/03.webp", "reader/04.webp"}
	waitForWithin(t, 20*time.Second, func() bool { return slices.Equal(served(gallery), pages) })
	awaitMediaReady(t, admin, clip)
	waitForWithin(t, 20*time.Second, func() bool {
		view, err := admin.GetContentVersion(t.Context(), clip.ID, clip.Version, ck.GetContentVersionQuery{})
		return err == nil && view.Live
	})
	var checkpoints int
	must(t, m.pool.QueryRow(t.Context(), `SELECT count(*) FROM `+pgx.Identifier{m.config.Schema, "encode_chunk"}.Sanitize()+` WHERE state='done'`).Scan(&checkpoints))
	if checkpoints < 2 {
		t.Fatalf("the video finished in %d encoding checkpoints; the fixture needs several", checkpoints)
	}
	for _, ref := range []media.RefBody{gallery, clip} {
		wakes, plans, waiting := since(start, ref.Version)
		t.Logf("%s: processing woke the release %d times; %d planning runs, %d of them parked as waiting", ref.Kind, wakes, plans, waiting)
		if waiting != 0 {
			t.Errorf("%s: processing woke %d release plans that could only wait", ref.Kind, waiting)
		}
	}

	// A later chapter parks the next request at its date. Replacing a released
	// page re-plans at once when processing settles, and the date stays.
	future := m.clock.now().Add(time.Hour)
	_, err = admin.PutContentVersionSegments(t.Context(), gallery.ID, gallery.Version, catalog.SegmentsInput{Segments: []catalog.SegmentInput{
		{Title: "One", StartPage: "pages/01.png"}, {Title: "Two", StartPage: "pages/03.png", ReleaseAt: &future},
	}})
	must(t, err)
	waitForWithin(t, 45*time.Second, func() bool { return slices.Equal(served(gallery), pages[:2]) })
	waitFor(t, func() bool { return currentPublicationJob(t, m.app, gallery.Version).Due.Equal(future) })
	release, before := livePages(gallery.Version)
	replan := mark()
	_, err = admin.PostMediaUploadCommit(t.Context(), media.CommitBody{Ref: gallery, OperationID: uuid.NewString(), Ops: []media.Op{{Op: media.OpEdit, Path: "pages/02.png", Edit: &media.Edit{Rotate: 90}}}})
	must(t, err)
	awaitMediaReady(t, admin, gallery)
	waitForWithin(t, 20*time.Second, func() bool {
		id, after := livePages(gallery.Version)
		return id != release && after["pages/02.png"] != "" && after["pages/02.png"] != before["pages/02.png"]
	})
	if paths := served(gallery); !slices.Equal(paths, pages[:2]) {
		t.Fatalf("re-planned edition serves %v, want only chapter one %v", paths, pages[:2])
	}
	waitFor(t, func() bool { return currentPublicationJob(t, m.app, gallery.Version).Due.Equal(future) })
	wakes, plans, waiting := since(replan, gallery.Version)
	t.Logf("released gallery: %d wake-ups and %d planning runs for the replaced page, %d of them parked as waiting", wakes, plans, waiting)
	// Only the editor's commit may plan before its processing settles.
	if waiting > 1 {
		t.Errorf("processing woke %d release plans of a released edition that could only wait", waiting)
	}
}
