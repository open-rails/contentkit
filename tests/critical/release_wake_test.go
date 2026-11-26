package critical_test

import (
	"bytes"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/kinds"
	"github.com/open-rails/contentkit/media"
)

// A release is all or nothing: a published gallery and a video whose media
// still processes serve nothing, and every manifest write wakes the release,
// so both release once processing settles. A released edition re-plans when
// processing changes a page, serving the new rendition and keeping a later
// chapter's date.
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
	served := func(ref media.RefBody) map[string]string {
		view, err := reader.GetMediaVersion(t.Context(), ref.Kind, ref.ID, ref.Version, ck.GetMediaVersionQuery{})
		if err != nil {
			return nil
		}
		out := map[string]string{}
		for _, file := range view.Files {
			if u, err := url.Parse(file.URL); err == nil && file.URL != "" {
				out[file.Path] = u.Path // its hash: a new rendition has a new path
			}
		}
		return out
	}
	paths := func(files map[string]string) []string {
		out := make([]string, 0, len(files))
		for p := range files {
			out = append(out, p)
		}
		slices.Sort(out)
		return out
	}

	// Both editions are published before any processing runs.
	gallery := galleryRef(t, admin, "Wakes on settle")
	_, err := admin.IngestMedia(t.Context(), http.DefaultClient, gallery, uuid.NewString(), []ck.MediaFile{
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
	}
	// No worker processes yet: each release runs, finds processing, holds.
	for _, ref := range []media.RefBody{gallery, clip} {
		waitForWithin(t, 45*time.Second, func() bool { return currentPublicationJob(t, m.app, ref.Version).ID == 0 })
	}
	if files := served(gallery); len(files) != 0 {
		t.Fatalf("a gallery still processing serves %v", paths(files))
	}
	if view, err := admin.GetContentVersion(t.Context(), clip.ID, clip.Version, ck.GetContentVersionQuery{}); err != nil || view.Live {
		t.Fatalf("a video still processing went live: %+v %v", view, err)
	}
	startVideoFailureWorker(t, m)

	awaitMediaReady(t, admin, gallery)
	pages := []string{"reader/01.webp", "reader/02.webp", "reader/03.webp", "reader/04.webp"}
	waitForWithin(t, 20*time.Second, func() bool { return slices.Equal(paths(served(gallery)), pages) })
	awaitMediaReady(t, admin, clip)
	waitForWithin(t, 20*time.Second, func() bool {
		view, err := admin.GetContentVersion(t.Context(), clip.ID, clip.Version, ck.GetContentVersionQuery{})
		return err == nil && view.Live
	})
	var checkpoints int
	must(t, m.pool.QueryRow(t.Context(), `SELECT count(*) FROM `+m.config.Schema+`.encode_chunk WHERE state='done'`).Scan(&checkpoints))
	if checkpoints < 2 {
		t.Fatalf("the video finished in %d encoding checkpoints; the fixture needs several", checkpoints)
	}

	// A later chapter waits for its date. Replacing a released page re-plans
	// once its processing settles; the date stays.
	future := m.clock.now().Add(time.Hour)
	_, err = admin.PutContentVersionSegments(t.Context(), gallery.ID, gallery.Version, catalog.SegmentsInput{Segments: []catalog.SegmentInput{
		{Title: "One", StartPage: "pages/01.png"}, {Title: "Two", StartPage: "pages/03.png", ReleaseAt: &future},
	}})
	must(t, err)
	waitForWithin(t, 45*time.Second, func() bool { return slices.Equal(paths(served(gallery)), pages[:2]) })
	waiting := func() {
		t.Helper()
		waitFor(t, func() bool {
			job := currentPublicationJob(t, m.app, gallery.Version)
			return job.State == "scheduled" && job.Due.After(time.Now().Add(30*time.Minute))
		})
	}
	waiting()
	before := served(gallery)["reader/02.webp"]
	_, err = admin.PostMediaUploadCommit(t.Context(), media.CommitBody{Ref: gallery, OperationID: uuid.NewString(), Ops: []media.Op{{Op: media.OpEdit, Path: "pages/02.png", Edit: &media.Edit{Rotate: 90}}}})
	must(t, err)
	awaitMediaReady(t, admin, gallery)
	waitForWithin(t, 20*time.Second, func() bool {
		after := served(gallery)["reader/02.webp"]
		return after != "" && after != before
	})
	if got := paths(served(gallery)); !slices.Equal(got, pages[:2]) {
		t.Fatalf("re-planned edition serves %v, want only chapter one %v", got, pages[:2])
	}
	waiting()
}
