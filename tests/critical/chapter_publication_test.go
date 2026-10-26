package critical_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/versions"
)

func chapterImage(t *testing.T, seed uint8) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 128, 96))
	for y := 0; y < 96; y++ {
		for x := 0; x < 128; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: uint8(x)*seed + uint8(y), G: uint8(y)*seed + uint8(x/3), B: uint8(x*y) + seed, A: 255})
		}
	}
	var out bytes.Buffer
	must(t, png.Encode(&out, img))
	return out.Bytes()
}

func TestCopiedPageChapterPlacement(t *testing.T) {
	w := newWorld(t)
	a := w.apps[0]
	attachMedia(t, w, a, func(c *ck.Config) {
		c.Media.Kinds[0].Publish = &media.Publish{Pages: "pages/{name}", Renditions: []string{"reader"}}
	})
	admin, viewer := a.engine.Client(), a.client("")
	source := galleryRef(t, admin, "Copy source")
	_, err := admin.IngestMedia(t.Context(), http.DefaultClient, source, uuid.NewString(), []ck.MediaFile{{Path: "pages/source.png", Type: "image/png", Body: bytes.NewReader(chapterImage(t, 41))}})
	must(t, err)
	awaitMediaReady(t, admin, source)
	ref := galleryRef(t, admin, "Independent copied pages")
	_, err = admin.PostMediaUploadCommit(t.Context(), media.CommitBody{Ref: ref, OperationID: uuid.NewString(), Ops: []media.Op{
		{Op: media.OpCopy, From: &media.CopyFrom{ID: source.ID, Version: source.Version, Path: "pages/source.png"}, To: "pages/01.png"},
		{Op: media.OpCopy, From: &media.CopyFrom{ID: source.ID, Version: source.Version, Path: "pages/source.png"}, To: "pages/20.png"},
		{Op: media.OpCopy, From: &media.CopyFrom{ID: ref.ID, Version: ref.Version, Path: "pages/01.png"}, To: "pages/30.png"},
	}})
	must(t, err)
	ready := awaitMediaReady(t, admin, ref)
	requireMediaPaths(t, ready, "pages/01.png", "pages/20.png", "pages/30.png", "reader/01.webp", "reader/20.webp", "reader/30.webp")
	// The existing page-quality policy excludes duplicate images by default.
	// Explicitly retain them through the editor API before assigning chapters.
	notices, err := admin.GetContentVersionPageNotices(t.Context(), ref.ID, ref.Version)
	must(t, err)
	for _, notice := range notices.Notices {
		if notice.Kind == "removed" {
			_, err = admin.PostContentVersionPageNoticeRestore(t.Context(), ref.ID, ref.Version, notice.ID)
			must(t, err)
		}
	}
	future := w.clock.now().Add(time.Hour)
	_, err = admin.PutContentVersionSegments(t.Context(), ref.ID, ref.Version, catalog.SegmentsInput{Segments: []catalog.SegmentInput{
		{Position: 0, StartPage: "pages/01.png"},
		{Position: 1, StartPage: "pages/20.png", ReleaseAt: &future},
	}})
	must(t, err)
	_, err = admin.PostContentPublish(t.Context(), ref.ID, catalog.PublishInput{})
	must(t, err)
	awaitPaths := func(want ...string) {
		t.Helper()
		waitForChapterPublication(t, func() bool {
			read, err := viewer.GetMediaVersion(t.Context(), ref.Kind, ref.ID, ref.Version, ck.GetMediaVersionQuery{})
			var paths []string
			for _, f := range read.Files {
				if f.URL != "" {
					paths = append(paths, f.Path)
				}
			}
			return err == nil && slices.Equal(paths, want)
		})
	}
	awaitPaths("reader/01.webp")
	// Identical bytes in three source entries must follow their own chapter.
	// Moving the same-item copy publishes it without exposing the other copy.
	index := 1
	_, err = admin.PostMediaUploadCommit(t.Context(), media.CommitBody{Ref: ref, OperationID: uuid.NewString(), Ops: []media.Op{{Op: media.OpMove, Path: "pages/30.png", Index: &index}}})
	must(t, err)
	awaitPaths("reader/01.webp", "reader/30.webp")
}

func TestChapterPlacementReconciliation(t *testing.T) {
	w := newWorld(t)
	a := w.apps[0]
	attachMedia(t, w, a, func(c *ck.Config) {
		k := &c.Media.Kinds[0]
		k.Uploads[0].Max = 20
		k.Publish = &media.Publish{Pages: "pages/{name}", Renditions: []string{"reader"}}
	})
	admin, viewer := a.engine.Client(), a.client("")
	ref := galleryRef(t, admin, "Ordered chapter publication")
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for _, query := range []string{
			`SELECT coalesce(jsonb_agg(jsonb_build_object('id',id,'state',state,'current',current,'due',scheduled_at,'error',error,'plan_fingerprint',plan->>'fingerprint','result_fingerprint',result->>'fingerprint')),'[]') FROM ` + pgx.Identifier{w.schema, "content_publication_requests"}.Sanitize(),
			`SELECT coalesce(jsonb_agg(jsonb_build_object('id',id,'kind',kind,'state',state,'args',args,'errors',errors,'due',scheduled_at)),'[]') FROM ` + pgx.Identifier{w.schema + "_river", "river_job"}.Sanitize() + ` WHERE kind IN ('contentkit_version_publish','contentkit_version_release') AND state NOT IN ('completed','cancelled')`,
		} {
			var state string
			err := w.pool.QueryRow(ctx, query).Scan(&state)
			t.Logf("publication diagnostic: %s error=%v", state, err)
		}
	})
	var files []ck.MediaFile
	for i, path := range []string{"pages/01.png", "pages/17.png", "pages/20.png", "pages/40.png"} {
		files = append(files, ck.MediaFile{Path: path, Type: "image/png", Body: bytes.NewReader(chapterImage(t, uint8(7+i*23)))})
	}
	_, err := admin.IngestMedia(t.Context(), http.DefaultClient, ref, uuid.NewString(), files)
	must(t, err)
	awaitMediaReady(t, admin, ref)
	future := w.clock.now().Add(time.Hour)
	_, err = admin.PutContentVersionSegments(t.Context(), ref.ID, ref.Version, catalog.SegmentsInput{Segments: []catalog.SegmentInput{
		{Position: 0, Title: "One", StartPage: "pages/01.png"},
		{Position: 1, Title: "Two", StartPage: "pages/20.png", ReleaseAt: &future},
	}})
	must(t, err)
	_, err = admin.PostContentPublish(t.Context(), ref.ID, catalog.PublishInput{})
	must(t, err)
	readPaths := func() (media.ReadResult, []string, error) {
		// Exercise the real read limiter instead of overwhelming it with the
		// generic 50ms async-condition poll interval.
		time.Sleep(550 * time.Millisecond)
		read, err := viewer.GetMediaVersion(t.Context(), ref.Kind, ref.ID, ref.Version, ck.GetMediaVersionQuery{})
		var paths []string
		for _, f := range read.Files {
			if f.URL != "" {
				paths = append(paths, f.Path)
			}
		}
		return read, paths, err
	}
	awaitPaths := func(want ...string) media.ReadResult {
		t.Helper()
		var got media.ReadResult
		waitForWithin(t, 45*time.Second, func() bool {
			read, paths, err := readPaths()
			got = read
			return err == nil && slices.Equal(paths, want)
		})
		return got
	}
	first := awaitPaths("reader/01.webp", "reader/17.webp")
	for _, file := range first.Files {
		if file.URL != "" {
			response, err := http.Get(file.URL)
			must(t, err)
			response.Body.Close()
			if response.StatusCode != http.StatusOK {
				t.Fatalf("completed chapter URL returned %d", response.StatusCode)
			}
		}
	}
	// A virtual filename inserted before the next start marker joins chapter
	// one automatically. No membership list or per-file identifier is authored.
	op := stageFile(t, admin, ref, "pages/19b.png", chapterImage(t, 101))
	index := 2
	op.Index = &index
	_, err = admin.PostMediaUploadCommit(t.Context(), media.CommitBody{Ref: ref, OperationID: uuid.NewString(), Ops: []media.Op{op}})
	must(t, err)
	inserted := awaitPaths("reader/01.webp", "reader/17.webp", "reader/19b.webp")
	var oldURL string
	for _, file := range inserted.Files {
		if file.Path == "reader/19b.webp" {
			oldURL = file.URL
		}
	}
	if oldURL == "" {
		t.Fatal("inserted source did not publish into its released chapter")
	}
	// Moving the same source past chapter two's marker withdraws it through
	// the publication worker, without waiting for a periodic purge.
	index = 4
	_, err = admin.PostMediaUploadCommit(t.Context(), media.CommitBody{Ref: ref, OperationID: uuid.NewString(), Ops: []media.Op{{Op: media.OpMove, Path: "pages/19b.png", Index: &index}}})
	must(t, err)
	awaitPaths("reader/01.webp", "reader/17.webp")
	waitForWithin(t, 15*time.Second, func() bool {
		response, err := http.Get(oldURL)
		must(t, err)
		response.Body.Close()
		return response.StatusCode == http.StatusNotFound
	})
	// Advancing the application clock alone cannot publish the chapter.
	// First drain the real follow-ups from the preceding move, so an already
	// executing worker cannot legitimately observe the new clock mid-attempt.
	waitForWithin(t, 45*time.Second, func() bool {
		var pending int
		must(t, w.pool.QueryRow(t.Context(), `SELECT count(*) FROM `+pgx.Identifier{w.schema + "_river", "river_job"}.Sanitize()+`
 WHERE kind IN ('contentkit_version_publish','contentkit_version_release')
 AND state IN ('available','pending','running','retryable','scheduled') AND scheduled_at<=clock_timestamp()`).Scan(&pending))
		return pending == 0
	})
	w.clock.set(future)
	_, paths, err := readPaths()
	must(t, err)
	if !slices.Equal(paths, []string{"reader/01.webp", "reader/17.webp"}) {
		t.Fatalf("clock alone changed released files: %v", paths)
	}
	// Advance the owned River fixture's wakeup, keeping the real worker and
	// authoritative request due-time check. This avoids waiting one wall hour.
	waitForWithin(t, 45*time.Second, func() bool {
		wakeDuePublicationJobs(t, a)
		_, paths, err := readPaths()
		return err == nil && slices.Equal(paths, []string{"reader/01.webp", "reader/17.webp", "reader/20.webp", "reader/40.webp", "reader/19b.webp"})
	})
}

// hiddenStartGallery publishes pages with chapters and returns helpers that
// wait for the exact readable page set and release due chapters.
type hiddenStartGallery struct {
	w             *world
	a             *application
	admin, viewer *ck.Client
	ref           media.RefBody
	images        map[string][]byte
	start         time.Time
}

func newHiddenStartGallery(t *testing.T, pages []string, chapters func(start time.Time) []catalog.SegmentInput) *hiddenStartGallery {
	w := newWorld(t)
	a := w.apps[0]
	attachMedia(t, w, a, func(c *ck.Config) {
		k := &c.Media.Kinds[0]
		k.Uploads[0].Max = 20
		k.Publish = &media.Publish{Pages: "pages/{name}", Renditions: []string{"reader"}}
	})
	g := &hiddenStartGallery{w: w, a: a, admin: a.engine.Client(), viewer: a.client(""), images: map[string][]byte{}, start: w.clock.now()}
	g.ref = galleryRef(t, g.admin, "Hidden chapter start")
	var files []ck.MediaFile
	for i, path := range pages {
		g.images[path] = chapterImage(t, uint8(9+i*29))
		files = append(files, ck.MediaFile{Path: path, Type: "image/png", Body: bytes.NewReader(g.images[path])})
	}
	_, err := g.admin.IngestMedia(t.Context(), http.DefaultClient, g.ref, uuid.NewString(), files)
	must(t, err)
	awaitMediaReady(t, g.admin, g.ref)
	_, err = g.admin.PutContentVersionSegments(t.Context(), g.ref.ID, g.ref.Version, catalog.SegmentsInput{Segments: chapters(g.start)})
	must(t, err)
	_, err = g.admin.PostContentPublish(t.Context(), g.ref.ID, catalog.PublishInput{})
	must(t, err)
	return g
}

func (g *hiddenStartGallery) insert(t *testing.T, at int, pages map[string][]byte, order ...string) {
	t.Helper()
	var ops []media.Op
	for i, path := range order {
		op := stageFile(t, g.admin, g.ref, path, pages[path])
		index := at + i
		op.Index = &index
		ops = append(ops, op)
	}
	_, err := g.admin.PostMediaUploadCommit(t.Context(), media.CommitBody{Ref: g.ref, OperationID: uuid.NewString(), Ops: ops})
	must(t, err)
}

// readable waits until signed-out readers get exactly want, then checks the
// reader plan answers and every listed page file is served.
func (g *hiddenStartGallery) readable(t *testing.T, want ...string) {
	t.Helper()
	var got media.ReadResult
	waitForWithin(t, 45*time.Second, func() bool {
		wakeDuePublicationJobs(t, g.a)
		time.Sleep(550 * time.Millisecond)
		read, err := g.viewer.GetMediaVersion(t.Context(), g.ref.Kind, g.ref.ID, g.ref.Version, ck.GetMediaVersionQuery{})
		var paths []string
		for _, f := range read.Files {
			if f.URL != "" {
				paths = append(paths, f.Path)
			}
		}
		got = read
		return err == nil && slices.Equal(paths, want)
	})
	reply, err := g.viewer.GetContentVersionReader(t.Context(), g.ref.ID, g.ref.Version)
	must(t, err)
	if reply.Status != http.StatusOK {
		t.Fatalf("reader plan returned %d: %s", reply.Status, reply.Body)
	}
	for _, file := range got.Files {
		if file.URL != "" {
			response, err := http.Get(file.URL)
			must(t, err)
			response.Body.Close()
			if response.StatusCode != http.StatusOK {
				t.Fatalf("%s returned %d", file.Path, response.StatusCode)
			}
		}
	}
}

// advance moves the clock after the publication jobs already due have run.
func (g *hiddenStartGallery) advance(t *testing.T, to time.Time) {
	t.Helper()
	waitForWithin(t, 45*time.Second, func() bool {
		var pending int
		must(t, g.w.pool.QueryRow(t.Context(), `SELECT count(*) FROM `+pgx.Identifier{g.w.schema + "_river", "river_job"}.Sanitize()+`
 WHERE kind IN ('contentkit_version_publish','contentkit_version_release')
 AND state IN ('available','pending','running','retryable','scheduled') AND scheduled_at<=clock_timestamp()`).Scan(&pending))
		return pending == 0
	})
	g.w.clock.set(to)
}

// A chapter whose start page is hidden as a duplicate starts at its next
// readable page. The kept copy is that chapter's first page, so it waits for
// the chapter's date although it sits in an earlier, released chapter.
func TestHiddenChapterStartKeepsPublishing(t *testing.T) {
	g := newHiddenStartGallery(t, []string{"pages/01.png", "pages/02.png", "pages/10.png", "pages/11.png", "pages/12.png"}, func(start time.Time) []catalog.SegmentInput {
		two, three := start.Add(time.Hour), start.Add(2*time.Hour)
		return []catalog.SegmentInput{
			{Position: 0, Title: "One", StartPage: "pages/01.png"},
			{Position: 1, Title: "Two", StartPage: "pages/10.png", ReleaseAt: &two},
			{Position: 2, Title: "Three", StartPage: "pages/11.png", ReleaseAt: &three},
		}
	})
	g.readable(t, "reader/01.webp", "reader/02.webp")

	_, err := g.admin.PostMediaUploadCommit(t.Context(), media.CommitBody{Ref: g.ref, OperationID: uuid.NewString(), Ops: []media.Op{{Op: media.OpRemove, Path: "pages/10.png"}}})
	var refused *ck.Error
	if !errors.As(err, &refused) || refused.Status != http.StatusBadRequest || !strings.Contains(refused.Message, "choose a new chapter start") {
		t.Errorf("removing a chapter start: %v", err)
	}

	// Copies named earlier hide chapters two and three's starts. Chapter two
	// has no readable page left and is dropped; three starts at 12.
	g.insert(t, 2, map[string][]byte{"pages/03.png": g.images["pages/10.png"], "pages/04.png": g.images["pages/11.png"], "pages/05.png": chapterImage(t, 201)},
		"pages/03.png", "pages/04.png", "pages/05.png")
	flagged := map[string]int{}
	waitForWithin(t, 45*time.Second, func() bool {
		notices, err := g.admin.GetContentVersionPageNotices(t.Context(), g.ref.ID, g.ref.Version)
		must(t, err)
		clear(flagged)
		for _, n := range notices.Notices {
			for _, p := range n.Pages {
				if n.Kind == "removed" && p.Reason == "duplicate" {
					flagged[p.SourceName] = p.Chapter
				}
			}
		}
		return len(flagged) == 2
	})
	if flagged["10.png"] != 2 || flagged["11.png"] != 3 {
		t.Errorf("hidden chapter starts are not flagged to editors: %v", flagged)
	}

	auditor, token := g.w.person(t, 0)
	authtest.GrantRole(t, g.w.tenant[0], g.a.group, iam.UserSubject(auditor.ID), g.w.viewer)
	var reply *ck.Reply
	waitForWithin(t, 45*time.Second, func() bool {
		reply, err = g.a.client(token).GetContentVersionReader(t.Context(), g.ref.ID, g.ref.Version)
		must(t, err)
		return reply.Status != http.StatusAccepted
	})
	var inspection versions.ReaderPlan
	if reply.Status != http.StatusOK || json.Unmarshal(reply.Body, &inspection) != nil || len(inspection.Segments) == 0 || !inspection.Segments[0].Inspection {
		t.Errorf("auditor inspection plan returned %d: %s", reply.Status, reply.Body)
	}

	g.readable(t, "reader/01.webp", "reader/02.webp", "reader/05.webp")
	g.advance(t, g.start.Add(time.Hour))
	g.readable(t, "reader/01.webp", "reader/02.webp", "reader/03.webp", "reader/05.webp")
	g.advance(t, g.start.Add(2*time.Hour))
	g.readable(t, "reader/01.webp", "reader/02.webp", "reader/03.webp", "reader/04.webp", "reader/05.webp", "reader/12.webp")
}

// A joined spread placed before a chapter start covers it. The spread holds
// that chapter's first page, so it waits for the chapter's date.
func TestHiddenChapterStartSpreadWaits(t *testing.T) {
	g := newHiddenStartGallery(t, []string{"pages/01.png", "pages/02.png", "pages/09.png", "pages/10.png", "pages/11.png"}, func(start time.Time) []catalog.SegmentInput {
		two := start.Add(time.Hour)
		return []catalog.SegmentInput{
			{Position: 0, Title: "One", StartPage: "pages/01.png"},
			{Position: 1, Title: "Two", StartPage: "pages/10.png", ReleaseAt: &two},
		}
	})
	g.readable(t, "reader/01.webp", "reader/02.webp", "reader/09.webp")
	g.insert(t, 2, map[string][]byte{"pages/03.png": chapterImage(t, 202), "pages/09-10.png": chapterImage(t, 203)}, "pages/03.png", "pages/09-10.png")
	g.readable(t, "reader/01.webp", "reader/02.webp", "reader/03.webp")
	g.advance(t, g.start.Add(time.Hour))
	g.readable(t, "reader/01.webp", "reader/02.webp", "reader/03.webp", "reader/09-10.webp", "reader/11.webp")
}
