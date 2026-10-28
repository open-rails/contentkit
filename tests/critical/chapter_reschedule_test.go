package critical_test

import (
	"bytes"
	"image/png"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	versionstore "github.com/open-rails/contentkit/internal/versionstore"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/media"
)

// The public media read API is rate limited. Observe publication at under two
// requests per second rather than turning a storage wait into a 429 burst.
func waitForChapterPublication(t *testing.T, ready func() bool) {
	t.Helper()
	deadline := time.NewTimer(45 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(550 * time.Millisecond)
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
	// future boundary. Once due, the normal reconciliation publishes it again.
	_, err = admin.PutContentVersionSegments(t.Context(), ref.ID, ref.Version, input)
	must(t, err)
	var jobID int64
	var requestID string
	waitFor(t, func() bool {
		return w.pool.QueryRow(t.Context(), `SELECT j.id,r.id::text
 FROM `+w.schema+`_river.river_job j JOIN `+w.schema+`.content_publication_requests r ON j.args->>'id'=r.id::text
 WHERE j.kind='contentkit_publication' AND r.tenant_id=$1 AND r.version_id=$2 AND r.current
 AND r.scheduled_at=$3 AND j.state='scheduled' ORDER BY j.id DESC LIMIT 1`, a.cfg.Scope.AppID.String(), ref.Version, future).Scan(&jobID, &requestID) == nil
	})
	wake := func() {
		_, err := w.pool.Exec(t.Context(), `UPDATE `+w.schema+`_river.river_job
 SET scheduled_at=clock_timestamp(),state='available' WHERE id=$1 AND state IN ('scheduled','retryable')`, jobID)
		must(t, err)
	}
	// Pass the queue's due check, then rewind the controlled clock while the
	// release waits for its source lock, exercising the second due check.
	unlock, err := versionstore.LockSource(t.Context(), w.pool, a.cfg.Scope.AppID.String(), ref.Version)
	must(t, err)
	t.Cleanup(unlock)
	beforeDue := w.clock.now()
	w.clock.set(future.Add(time.Second))
	wake()
	waitFor(t, func() bool {
		var waiting bool
		must(t, w.pool.QueryRow(t.Context(), `SELECT EXISTS (
 SELECT 1 FROM pg_locks WHERE locktype='advisory' AND NOT granted
 AND ((classid::bigint << 32) | objid::bigint)=hashtextextended($1,0))
 AND EXISTS (SELECT 1 FROM `+w.schema+`_river.river_job WHERE id=$2 AND state='running')`,
			"contentkit:media:contentkit:versions:"+a.cfg.Scope.AppID.String()+":"+ref.Version, jobID).Scan(&waiting))
		return waiting
	})
	w.clock.set(beforeDue)
	unlock()
	waitFor(t, func() bool {
		var state string
		must(t, w.pool.QueryRow(t.Context(), `SELECT state FROM `+w.schema+`_river.river_job WHERE id=$1`, jobID).Scan(&state))
		if state == "completed" || state == "discarded" || state == "cancelled" {
			t.Fatalf("early release job ended instead of snoozing: %s", state)
		}
		return state == "scheduled"
	})
	var state, failure string
	must(t, w.pool.QueryRow(t.Context(), `SELECT state,coalesce(error,'') FROM `+w.schema+`.content_publication_requests WHERE id=$1`, requestID).Scan(&state, &failure))
	if state == "failed" || failure != "" {
		t.Fatalf("early wake failed the publication request: %s %s", state, failure)
	}
	// River uses wall time; wake the same snoozed job at the catalog deadline.
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

// A release held at the source lock serves a pixel copy of chapter two before processing
// hides it. Processing completion must withdraw it at once; chapter two keeps its date.
func TestProcessingWithdrawsHiddenCopyBeforeChapterDate(t *testing.T) {
	m := newResilientMedia(t, func(c *ck.Config) {
		c.Media.Kinds[0].Publish = &media.Publish{Pages: "pages/{name}", Renditions: []string{"reader"}}
	})
	m.start(t)
	admin, reader := m.app.engine.Client(), m.app.client("")
	tenant := publicationTenant(m.app)
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
	waitFor(t, func() bool { return currentPublicationJob(t, m.app, ref.Version).Due.Equal(future) })

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
	waitForWithin(t, 45*time.Second, func() bool {
		var busy int
		must(t, m.pool.QueryRow(t.Context(), `SELECT count(*) FROM `+pgx.Identifier{m.schema + "_river", "river_job"}.Sanitize()+`
 WHERE kind='contentkit_publication' AND state IN ('available','pending','running','retryable','scheduled') AND scheduled_at<=clock_timestamp()`).Scan(&busy))
		return busy == 0
	})
	unlock, err := versionstore.LockSource(t.Context(), m.pool, tenant, ref.Version)
	must(t, err)
	t.Cleanup(unlock)
	queued := func(want int) {
		t.Helper()
		waitFor(t, func() bool {
			var n int
			must(t, m.pool.QueryRow(t.Context(), `SELECT count(*) FROM pg_locks WHERE locktype='advisory' AND NOT granted
 AND ((classid::bigint << 32) | objid::bigint)=hashtextextended($1,0)`, "contentkit:media:contentkit:versions:"+tenant+":"+ref.Version).Scan(&n))
			return n == want
		})
	}
	// Publishing again queues a release ahead of the derivative job's page selection.
	_, err = admin.PostContentPublish(t.Context(), ref.ID, catalog.PublishInput{})
	must(t, err)
	queued(1)
	hold.unblock()
	queued(2)
	unlock()
	waitForWithin(t, 45*time.Second, func() bool {
		var leaked bool
		must(t, m.pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM `+pgx.Identifier{m.schema, "content_publication_requests"}.Sanitize()+`
 WHERE tenant_id=$1 AND version_id=$2 AND state='completed' AND result->'pages'->'pages' @> '[{"path":"pages/30.png"}]')`, tenant, ref.Version).Scan(&leaked))
		return leaked
	})
	deadline := time.Now().Add(45 * time.Second)
	for paths := served(); !slices.Equal(paths, []string{"reader/01.webp"}); paths = served() {
		if time.Now().After(deadline) {
			t.Fatalf("copy of chapter two hidden by processing stayed public until the chapter date: %v", paths)
		}
		time.Sleep(550 * time.Millisecond)
	}
	// The corrected view is served while its release is still retiring, before
	// that release parks the next request.
	deadline = time.Now().Add(15 * time.Second)
	for due := currentPublicationJob(t, m.app, ref.Version).Due; !due.Equal(future); due = currentPublicationJob(t, m.app, ref.Version).Due {
		if time.Now().After(deadline) {
			t.Fatalf("chapter two moved from its date after the correction: %s, want %s", due, future)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
