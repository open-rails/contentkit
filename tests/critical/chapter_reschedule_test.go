package critical_test

import (
	"bytes"
	"image/png"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/media"
)

// Media fixtures disable the HTTP read limiter so publication can be polled
// promptly while retaining the full worker timeout.
func waitForChapterPublication(t *testing.T, ready func() bool) {
	t.Helper()
	deadline := time.NewTimer(45 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		if ready() {
			return
		}
		select {
		case <-t.Context().Done():
			t.Fatal(t.Context().Err())
		case <-deadline.C:
			t.Fatal("publication did not reach the expected state within 45 seconds")
		case <-tick.C:
		}
	}
}

func TestReleasedChapterFutureDateWithdrawsItsFiles(t *testing.T) {
	w := newWorld(t)
	a := w.apps[0]
	attachMedia(t, w, a, func(c *ck.Config) {
		c.Media.Kinds[0].Uploads[0].Max = 4
		c.Media.Kinds[0].Publish = &media.Publish{Pages: "pages/{name}", Renditions: []string{"reader"}}
	})
	admin, reader := a.engine.Client(), a.client("")
	ref := galleryRef(t, admin, "Chapter date changes")
	_, err := admin.IngestMedia(t.Context(), http.DefaultClient, ref, uuid.NewString(), []ck.MediaFile{
		{Path: "pages/01.png", Type: "image/png", Body: bytes.NewReader(chapterImage(t, 31))},
		{Path: "pages/20.png", Type: "image/png", Body: bytes.NewReader(chapterImage(t, 97))},
	})
	must(t, err)
	awaitMediaReady(t, admin, ref)
	chapters, err := admin.PutContentVersionSegments(t.Context(), ref.ID, ref.Version, catalog.SegmentsInput{Segments: []catalog.SegmentInput{
		{Title: "One", StartPage: "pages/01.png"}, {Title: "Two", StartPage: "pages/20.png"},
	}})
	must(t, err)
	_, err = admin.PostContentPublish(t.Context(), ref.ID, catalog.PublishInput{})
	must(t, err)
	var oldURL string
	waitForChapterPublication(t, func() bool {
		view, err := reader.GetMediaVersion(t.Context(), ref.Kind, ref.ID, ref.Version, ck.GetMediaVersionQuery{})
		if err != nil {
			return false
		}
		for _, file := range view.Files {
			if file.Path == "reader/20.webp" {
				oldURL = file.URL
			}
		}
		return oldURL != ""
	})
	future := w.clock.now().Add(time.Hour)
	input := catalog.SegmentsInput{Segments: []catalog.SegmentInput{
		{ID: chapters.Segments[0].ID, Title: "One", StartPage: "pages/01.png"},
		{ID: chapters.Segments[1].ID, Title: "Two", StartPage: "pages/20.png", ReleaseAt: &future},
	}}
	updated, err := admin.PutContentVersionSegments(t.Context(), ref.ID, ref.Version, input)
	must(t, err)
	if updated.Segments[1].ReleasedAt != nil || updated.Segments[1].ID != chapters.Segments[1].ID {
		t.Fatalf("future date did not withdraw the same chapter intent: %+v", updated.Segments[1])
	}
	waitForChapterPublication(t, func() bool {
		view, err := reader.GetMediaVersion(t.Context(), ref.Kind, ref.ID, ref.Version, ck.GetMediaVersionQuery{})
		if err != nil {
			return false
		}
		first := false
		for _, file := range view.Files {
			if file.Path == "reader/20.webp" && file.URL != "" {
				return false
			}
			first = first || file.Path == "reader/01.webp" && file.URL != ""
		}
		return first
	})
	waitForChapterPublication(t, func() bool {
		response, err := http.Get(oldURL)
		if err != nil {
			return false
		}
		response.Body.Close()
		return response.StatusCode == http.StatusNotFound
	})
	// Repeating the same intent does not create a new chapter or undo its
	// future boundary: the version's release waits for the date.
	_, err = admin.PutContentVersionSegments(t.Context(), ref.ID, ref.Version, input)
	must(t, err)
	var job publicationJob
	waitFor(t, func() bool {
		job = currentPublicationJob(t, a, ref.Version)
		return job.State == "scheduled" && job.Due.After(time.Now().Add(30*time.Minute))
	})
	wake := func() {
		_, err := w.pool.Exec(t.Context(), `UPDATE `+w.schema+`_river.river_job
 SET scheduled_at=clock_timestamp(),state='available' WHERE id=$1 AND state IN ('scheduled','retryable')`, job.ID)
		must(t, err)
	}
	// Woken early, the release plans again and waits again, without error.
	wake()
	waitFor(t, func() bool {
		var state string
		var errs int
		must(t, w.pool.QueryRow(t.Context(), `SELECT state,coalesce(array_length(errors,1),0) FROM `+w.schema+`_river.river_job WHERE id=$1`, job.ID).Scan(&state, &errs))
		if state == "completed" || state == "discarded" || state == "cancelled" || errs > 0 {
			t.Fatalf("an early release ended or failed instead of waiting: %s with %d errors", state, errs)
		}
		return state == "scheduled"
	})
	// River uses wall time; wake the same job at the catalog deadline.
	w.clock.set(future.Add(time.Second))
	wake()
	waitForChapterPublication(t, func() bool {
		view, err := reader.GetMediaVersion(t.Context(), ref.Kind, ref.ID, ref.Version, ck.GetMediaVersionQuery{})
		if err != nil {
			return false
		}
		for _, file := range view.Files {
			if file.Path == "reader/20.webp" && file.URL != "" {
				return true
			}
		}
		return false
	})
}

func TestIndependentChaptersSkipUnreleasedMiddle(t *testing.T) {
	w := newWorld(t)
	a := w.apps[0]
	attachMedia(t, w, a, func(c *ck.Config) {
		c.Media.Kinds[0].Uploads[0].Max = 4
		c.Media.Kinds[0].Publish = &media.Publish{Pages: "pages/{name}", Renditions: []string{"reader"}}
	})
	admin, reader := a.engine.Client(), a.client("")
	ref := galleryRef(t, admin, "Independent chapter deadlines")
	_, err := admin.IngestMedia(t.Context(), http.DefaultClient, ref, uuid.NewString(), []ck.MediaFile{
		{Path: "pages/01.png", Type: "image/png", Body: bytes.NewReader(chapterImage(t, 41))},
		{Path: "pages/20.png", Type: "image/png", Body: bytes.NewReader(chapterImage(t, 83))},
		{Path: "pages/40.png", Type: "image/png", Body: bytes.NewReader(chapterImage(t, 129))},
	})
	must(t, err)
	awaitMediaReady(t, admin, ref)
	future := w.clock.now().Add(time.Hour)
	_, err = admin.PutContentVersionSegments(t.Context(), ref.ID, ref.Version, catalog.SegmentsInput{Segments: []catalog.SegmentInput{
		{Title: "One", StartPage: "pages/01.png"},
		{Title: "Two", StartPage: "pages/20.png", ReleaseAt: &future},
		{Title: "Three", StartPage: "pages/40.png"},
	}})
	must(t, err)
	_, err = admin.PostContentPublish(t.Context(), ref.ID, catalog.PublishInput{})
	must(t, err)
	waitForChapterPublication(t, func() bool {
		view, err := reader.GetMediaVersion(t.Context(), ref.Kind, ref.ID, ref.Version, ck.GetMediaVersionQuery{})
		if err != nil {
			return false
		}
		first, third := false, false
		for _, file := range view.Files {
			if file.Path == "reader/20.webp" && file.URL != "" {
				t.Fatal("future middle chapter leaked into the completed reader view")
			}
			first = first || file.Path == "reader/01.webp" && file.URL != ""
			third = third || file.Path == "reader/40.webp" && file.URL != ""
		}
		return first && third
	})
	view, err := reader.GetContentVersion(t.Context(), ref.ID, ref.Version, ck.GetContentVersionQuery{})
	must(t, err)
	if len(view.Segments) != 2 || view.Segments[0].Title != "One" || view.Segments[1].Title != "Three" {
		t.Fatalf("catalog chapters did not use the completed snapshot: %+v", view.Segments)
	}
	for _, chapter := range view.Segments {
		if chapter.StartPage != "" || !chapter.Released || chapter.Start < 0 || chapter.End > 2 {
			t.Fatalf("catalog leaked authoring markers or uncompressed future ranges: %+v", chapter)
		}
	}
}

// A release woken while a pixel copy of chapter two is still processing
// changes nothing; once processing saved the page selection, the copy stays
// private and chapter two keeps its date.
func TestReleaseDuringPageSelectionKeepsCopyPrivate(t *testing.T) {
	m := newResilientMedia(t, func(c *ck.Config) {
		c.Media.Kinds[0].Publish = &media.Publish{Pages: "pages/{name}", Renditions: []string{"reader"}}
	})
	m.start(t)
	admin, reader := m.app.engine.Client(), m.app.client("")
	ref := galleryRef(t, admin, "Processing after release")
	_, err := admin.IngestMedia(t.Context(), http.DefaultClient, ref, uuid.NewString(), []ck.MediaFile{
		{Path: "pages/01.png", Type: "image/png", Body: bytes.NewReader(chapterImage(t, 31))},
		{Path: "pages/20.png", Type: "image/png", Body: bytes.NewReader(chapterImage(t, 97))},
	})
	must(t, err)
	awaitMediaReady(t, admin, ref)
	future := m.clock.now().Add(time.Hour)
	_, err = admin.PutContentVersionSegments(t.Context(), ref.ID, ref.Version, catalog.SegmentsInput{Segments: []catalog.SegmentInput{
		{Title: "One", StartPage: "pages/01.png"}, {Title: "Two", StartPage: "pages/20.png", ReleaseAt: &future},
	}})
	must(t, err)
	_, err = admin.PostContentPublish(t.Context(), ref.ID, catalog.PublishInput{})
	must(t, err)
	served := func() []string {
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
		return paths
	}
	waitForChapterPublication(t, func() bool { return slices.Equal(served(), []string{"reader/01.webp"}) })
	waiting := func() {
		t.Helper()
		waitFor(t, func() bool {
			job := currentPublicationJob(t, m.app, ref.Version)
			return job.State == "scheduled" && job.Due.After(time.Now().Add(30*time.Minute))
		})
	}
	waiting()

	// Different bytes with the same pixels: only page analysis finds the copy.
	decoded, err := png.Decode(bytes.NewReader(chapterImage(t, 97)))
	must(t, err)
	var copied bytes.Buffer
	must(t, (&png.Encoder{CompressionLevel: png.NoCompression}).Encode(&copied, decoded))
	hold := m.wire.pause(t, func(r *http.Request) bool {
		return r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/internal/") && strings.HasPrefix(r.Header.Get("Content-Type"), "image/webp")
	})
	op := stageFile(t, admin, ref, "pages/30.png", copied.Bytes())
	index := 1
	op.Index = &index
	_, err = admin.PostMediaUploadCommit(t.Context(), media.CommitBody{Ref: ref, OperationID: uuid.NewString(), Ops: []media.Op{op}})
	must(t, err)
	select {
	case <-hold.arrived:
	case <-time.After(60 * time.Second):
		t.Fatal("the copy's reader image never reached the held storage write")
	}
	// Publishing again runs the release while the copy processes.
	_, err = admin.PostContentPublish(t.Context(), ref.ID, catalog.PublishInput{})
	must(t, err)
	awaitDuePublications(t, m.app)
	if paths := served(); !slices.Equal(paths, []string{"reader/01.webp"}) {
		t.Fatalf("a release during processing changed what is served: %v", paths)
	}
	hold.unblock()
	awaitMediaReady(t, admin, ref)
	awaitDuePublications(t, m.app)
	for stable := time.Now().Add(2 * time.Second); time.Now().Before(stable); time.Sleep(50 * time.Millisecond) {
		if paths := served(); !slices.Equal(paths, []string{"reader/01.webp"}) {
			t.Fatalf("copy of chapter two became readable before its date: %v", paths)
		}
	}
	waiting()
}
