package critical_test

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/access"
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
	requireMediaPaths(t, ready, "cover.png", "pages/01.png", "pages/20.png", "pages/30.png", "reader/01.webp", "reader/20.webp", "reader/30.webp")
	if len(ready.Public) != 1 || len(ready.Public[0].Renditions) != 2 {
		t.Fatalf("automatic cover renditions incomplete: %+v", ready.Public)
	}
	for _, rendition := range ready.Public[0].Renditions {
		assertImage(t, rendition.URL, rendition.W, rendition.H)
	}
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
			`SELECT coalesce(jsonb_agg(jsonb_build_object('id',id,'kind',kind,'state',state,'args',args,'errors',errors,'due',scheduled_at)),'[]') FROM ` + pgx.Identifier{w.schema + "_river", "river_job"}.Sanitize() + ` WHERE kind='contentkit_publication' AND state NOT IN ('completed','cancelled')`,
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
 WHERE kind='contentkit_publication'
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
	premium       bool
}

func newHiddenStartGallery(t *testing.T, premium bool, pages []string, chapters func(start time.Time) []catalog.SegmentInput) *hiddenStartGallery {
	w := newWorld(t)
	a := w.apps[0]
	attachMedia(t, w, a, func(c *ck.Config) {
		k := &c.Media.Kinds[0]
		k.Uploads[0].Max = 20
		k.Publish = &media.Publish{Pages: "pages/{name}", Renditions: []string{"reader"}, Download: "released-archive"}
		k.Private = append(k.Private, media.Private{Name: "archive-pages", From: "pages/{name}", To: "download-pages/{name}{ext}", Image: &media.Image{Format: media.ImageFormatSource}},
			media.Private{Name: "released-archive", To: "released.zip", Zip: "download-pages/", HostOnly: true})
	})
	g := &hiddenStartGallery{w: w, a: a, admin: a.engine.Client(), viewer: a.client(""), images: map[string][]byte{}, start: w.clock.now(), premium: premium}
	g.ref = galleryRef(t, g.admin, "Hidden chapter start")
	if premium {
		_, err := g.admin.PatchContentVersion(t.Context(), g.ref.ID, g.ref.Version, catalog.VersionInput{Access: &access.Rule{Level: access.PPV}})
		must(t, err)
	}
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
		g.images[path] = pages[path]
		op := stageFile(t, g.admin, g.ref, path, pages[path])
		index := at + i
		op.Index = &index
		ops = append(ops, op)
	}
	_, err := g.admin.PostMediaUploadCommit(t.Context(), media.CommitBody{Ref: g.ref, OperationID: uuid.NewString(), Ops: ops})
	must(t, err)
}

func (g *hiddenStartGallery) paths(t *testing.T, client *ck.Client) ([]string, media.ReadResult) {
	read, err := client.GetMediaVersion(t.Context(), g.ref.Kind, g.ref.ID, g.ref.Version, ck.GetMediaVersionQuery{})
	if err != nil {
		return []string{"error: " + err.Error()}, read
	}
	paths := []string{}
	for _, f := range read.Files {
		if f.URL != "" {
			paths = append(paths, f.Path)
		}
	}
	sort.Strings(paths)
	return paths, read
}

// readable waits until readers get exactly want and keep it, then checks the
// reader plan, every served page file, the released ZIP, and that a premium
// version shows signed-out readers nothing.
func (g *hiddenStartGallery) readable(t *testing.T, want ...string) {
	t.Helper()
	sort.Strings(want)
	reader := g.viewer
	if g.premium {
		reader = g.admin
	}
	var got []string
	var read media.ReadResult
	waitForWithin(t, 45*time.Second, func() bool {
		wakeDuePublicationJobs(t, g.a)
		time.Sleep(550 * time.Millisecond)
		before := got
		got, read = g.paths(t, reader)
		if !slices.Equal(before, got) {
			t.Logf("readable %v, want %v", got, want)
		}
		return slices.Equal(got, want)
	})
	for range 4 {
		wakeDuePublicationJobs(t, g.a)
		time.Sleep(500 * time.Millisecond)
		if again, _ := g.paths(t, reader); !slices.Equal(again, want) {
			t.Fatalf("readable changed to %v, want %v", again, want)
		}
	}
	reply, err := g.viewer.GetContentVersionReader(t.Context(), g.ref.ID, g.ref.Version)
	must(t, err)
	if reply.Status != http.StatusOK {
		t.Fatalf("reader plan returned %d: %s", reply.Status, reply.Body)
	}
	for _, file := range read.Files {
		if file.URL != "" {
			response, err := http.Get(file.URL)
			must(t, err)
			response.Body.Close()
			if response.StatusCode != http.StatusOK {
				t.Fatalf("%s returned %d", file.Path, response.StatusCode)
			}
		}
	}
	if g.premium {
		if signedOut, _ := g.paths(t, g.viewer); len(signedOut) != 0 {
			t.Fatalf("signed-out reader of a premium version reads %v", signedOut)
		}
		return
	}
	var sources []string
	for _, path := range want {
		sources = append(sources, archiveDigest(g.images["pages/"+strings.TrimSuffix(strings.TrimPrefix(path, "reader/"), ".webp")+".png"]))
	}
	sort.Strings(sources)
	if zipped := g.archive(t); !slices.Equal(zipped, sources) {
		t.Fatalf("released ZIP holds %v, want %v", zipped, sources)
	}
}

func archiveDigest(b []byte) string { sum := sha256.Sum256(b); return hex.EncodeToString(sum[:8]) }

// archive downloads the released ZIP as a signed-out reader and returns its
// entries' digests.
func (g *hiddenStartGallery) archive(t *testing.T) []string {
	t.Helper()
	var grant versions.FileDownload
	waitForWithin(t, 45*time.Second, func() bool {
		reply, err := g.viewer.GetContentVersionDownload(t.Context(), g.ref.ID, g.ref.Version)
		must(t, err)
		return reply.Status != http.StatusAccepted && reply.Decode(&grant) == nil && grant.URL != ""
	})
	base, err := url.Parse(g.a.server.URL + "/content/v1/contents/" + g.ref.ID + "/versions/" + g.ref.Version + "/download")
	must(t, err)
	relative, err := url.Parse(grant.URL)
	must(t, err)
	response, err := http.Get(base.ResolveReference(relative).String())
	must(t, err)
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	must(t, err)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("released ZIP returned %d: %s", response.StatusCode, body)
	}
	archive, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	must(t, err)
	out := []string{}
	for _, f := range archive.File {
		r, err := f.Open()
		must(t, err)
		b, err := io.ReadAll(r)
		r.Close()
		must(t, err)
		out = append(out, archiveDigest(b))
	}
	sort.Strings(out)
	return out
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

func laterChapters(dates ...time.Duration) func(time.Time) []catalog.SegmentInput {
	return func(start time.Time) []catalog.SegmentInput {
		out := []catalog.SegmentInput{{Position: 0, Title: "One", StartPage: "pages/01.png"}}
		for i, d := range dates {
			at := start.Add(d)
			out = append(out, catalog.SegmentInput{Position: i + 1, StartPage: []string{"pages/10.png", "pages/20.png"}[i], ReleaseAt: &at})
		}
		return out
	}
}

// A chapter whose start page is hidden as a duplicate starts at its next
// readable page. The kept copy is that chapter's first page, so it waits for
// the chapter's date although it sits in an earlier, released chapter.
func TestHiddenChapterStartKeepsPublishing(t *testing.T) {
	g := newHiddenStartGallery(t, false, []string{"pages/01.png", "pages/02.png", "pages/10.png", "pages/11.png", "pages/12.png"}, func(start time.Time) []catalog.SegmentInput {
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
	g.readable(t, "reader/01.webp", "reader/02.webp", "reader/05.webp")

	auditor, token := g.w.person(t, 0)
	authtest.GrantRole(t, g.w.tenant[0], g.a.group, iam.UserSubject(auditor.ID), g.w.viewer)
	var reply *ck.Reply
	waitForWithin(t, 45*time.Second, func() bool {
		reply, err = g.a.client(token).GetContentVersionReader(t.Context(), g.ref.ID, g.ref.Version)
		return err != nil || reply.Status != http.StatusAccepted
	})
	var inspection versions.ReaderPlan
	if err != nil || reply.Status != http.StatusOK || json.Unmarshal(reply.Body, &inspection) != nil || len(inspection.Segments) == 0 || !inspection.Segments[0].Inspection {
		t.Errorf("auditor inspection plan: %v %+v", err, reply)
	}

	g.advance(t, g.start.Add(time.Hour))
	g.readable(t, "reader/01.webp", "reader/02.webp", "reader/03.webp", "reader/05.webp")
	g.advance(t, g.start.Add(2*time.Hour))
	g.readable(t, "reader/01.webp", "reader/02.webp", "reader/03.webp", "reader/04.webp", "reader/05.webp", "reader/12.webp")
}

// Two overlapping spreads placed before a premium chapter both contain its
// hidden start page. Both wait for the chapter's date.
func TestHiddenChapterStartSpreadWaits(t *testing.T) {
	g := newHiddenStartGallery(t, true, []string{"pages/01.png", "pages/02.png", "pages/09.png", "pages/10.png", "pages/11.png", "pages/12.png"}, laterChapters(time.Hour))
	g.readable(t, "reader/01.webp", "reader/02.webp", "reader/09.webp")
	g.insert(t, 2, map[string][]byte{"pages/03.png": chapterImage(t, 202), "pages/09-10.png": chapterImage(t, 203), "pages/10-11.png": chapterImage(t, 205)},
		"pages/03.png", "pages/09-10.png", "pages/10-11.png")
	g.readable(t, "reader/01.webp", "reader/02.webp", "reader/03.webp")
	g.advance(t, g.start.Add(time.Hour))
	g.readable(t, "reader/01.webp", "reader/02.webp", "reader/03.webp", "reader/09-10.webp", "reader/10-11.webp", "reader/12.webp")
}

// Copies of later chapters' middle pages, one byte-identical and one with the
// same pixels in different bytes, wait for their chapters even when chapter
// three opens before chapter two.
func TestHiddenPageCopiesWaitForTheirChapter(t *testing.T) {
	g := newHiddenStartGallery(t, false, []string{"pages/01.png", "pages/02.png", "pages/10.png", "pages/11.png", "pages/20.png", "pages/21.png"}, laterChapters(2*time.Hour, time.Hour))
	g.readable(t, "reader/01.webp", "reader/02.webp")
	decoded, err := png.Decode(bytes.NewReader(g.images["pages/21.png"]))
	must(t, err)
	var pixels bytes.Buffer
	must(t, (&png.Encoder{CompressionLevel: png.NoCompression}).Encode(&pixels, decoded))
	if bytes.Equal(pixels.Bytes(), g.images["pages/21.png"]) {
		t.Fatal("re-encoding kept the same bytes")
	}
	g.insert(t, 2, map[string][]byte{"pages/03.png": g.images["pages/11.png"], "pages/04.png": pixels.Bytes(), "pages/05.png": chapterImage(t, 201)},
		"pages/03.png", "pages/04.png", "pages/05.png")
	g.readable(t, "reader/01.webp", "reader/02.webp", "reader/05.webp")
	g.advance(t, g.start.Add(time.Hour))
	g.readable(t, "reader/01.webp", "reader/02.webp", "reader/04.webp", "reader/05.webp", "reader/20.webp")
	g.advance(t, g.start.Add(2*time.Hour))
	g.readable(t, "reader/01.webp", "reader/02.webp", "reader/03.webp", "reader/04.webp", "reader/05.webp", "reader/10.webp", "reader/20.webp")
}

func blankPage(t *testing.T) []byte {
	img := image.NewNRGBA(image.Rect(0, 0, 128, 96))
	for y := 0; y < 96; y++ {
		for x := 0; x < 128; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: 255, G: 255, B: 255, A: 255})
		}
	}
	var out bytes.Buffer
	must(t, png.Encode(&out, img))
	return out.Bytes()
}

// A blank start page has no copy to hold: its chapter starts at the next
// page. A start that is the gallery's last page leaves its chapter empty,
// and the copy of it in chapter one still waits for that chapter's date.
func TestBlankAndLastChapterStarts(t *testing.T) {
	g := newHiddenStartGallery(t, false, []string{"pages/01.png", "pages/02.png", "pages/10.png", "pages/11.png", "pages/20.png"}, laterChapters(time.Hour, 2*time.Hour))
	g.readable(t, "reader/01.webp", "reader/02.webp")
	blank := blankPage(t)
	op := stageFile(t, g.admin, g.ref, "pages/10.png", blank)
	op.Replace = true
	_, err := g.admin.PostMediaUploadCommit(t.Context(), media.CommitBody{Ref: g.ref, OperationID: uuid.NewString(), Ops: []media.Op{op}})
	must(t, err)
	g.images["pages/10.png"] = blank
	g.insert(t, 2, map[string][]byte{"pages/03.png": g.images["pages/20.png"], "pages/05.png": chapterImage(t, 201)}, "pages/03.png", "pages/05.png")
	g.readable(t, "reader/01.webp", "reader/02.webp", "reader/05.webp")
	g.advance(t, g.start.Add(time.Hour))
	g.readable(t, "reader/01.webp", "reader/02.webp", "reader/05.webp", "reader/11.webp")
	g.advance(t, g.start.Add(2*time.Hour))
	g.readable(t, "reader/01.webp", "reader/02.webp", "reader/03.webp", "reader/05.webp", "reader/11.webp")
}
