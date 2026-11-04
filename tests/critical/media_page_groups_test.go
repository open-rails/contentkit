package critical_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	mediaimage "github.com/open-rails/contentkit/internal/media/image"
	mediastore "github.com/open-rails/contentkit/internal/mediastore"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertest"
)

// The image processor records a gallery's rendered pages in groups. The host
// runs without its media worker: the test places the staged pages and runs
// the real processor against real PostgreSQL and object storage, so it alone
// renders and records them.
func TestMediaPageGroups(t *testing.T) {
	m := newResilientMedia(t, func(c *ck.Config) { c.Media.Kinds[0].Uploads[0].Max = 64 })

	t.Run("forty_pages_take_two_manifest_edits_and_report_each_group", func(t *testing.T) {
		g := newPageGroups(t, m, "Grouped page records", landscapePages(t, 40, 0))
		before := g.manifestEdits(t)
		must(t, g.processor(t, mediaimage.Config{Workers: 2, GroupWait: time.Minute}).Process(g.job(t.Context()), mediastore.ProcessJob{Ref: g.target}))
		if edits := g.manifestEdits(t) - before; edits != 2 {
			t.Fatalf("40 pages took %d manifest edits; groups of 32 take 2", edits)
		}
		if _, readers := g.readers(t); len(readers) != 40 {
			t.Fatalf("%d of 40 pages recorded their reader output", len(readers))
		}
		var progress []string
		for _, event := range g.processingEvents(t) {
			for _, preset := range event.fact.Presets {
				if preset.Name == "reader" {
					progress = append(progress, fmt.Sprintf("%s:%d/%d", event.typ, preset.Completed, preset.Total))
				}
			}
		}
		want := []string{media.EventProcessingProgressed + ":32/40", media.EventProcessingCompleted + ":40/40"}
		if len(progress) < 2 || !slices.Equal(progress[len(progress)-2:], want) {
			t.Fatalf("processing events %v do not report each group, want them to end %v", progress, want)
		}
		awaitMediaReady(t, g.client, g.ref)
	})

	t.Run("an_unrenderable_page_fails_alone_in_its_group", func(t *testing.T) {
		pages := landscapePages(t, 6, 10)
		pages[3].data = pageImage(t, 400, 400, 13)
		g := newPageGroups(t, m, "One page over the pixel limit", pages)
		before := g.manifestEdits(t)
		must(t, g.processor(t, mediaimage.Config{Workers: 2, GroupWait: time.Minute, MaxPixels: 100_000}).Process(g.job(t.Context()), mediastore.ProcessJob{Ref: g.target}))
		if edits := g.manifestEdits(t) - before; edits != 1 {
			t.Fatalf("one group took %d manifest edits", edits)
		}
		snapshot, readers := g.readers(t)
		bad, _ := snapshot.Get(pages[3].path)
		if failure := bad.Fail(); failure == nil || failure.Code != media.CodeImageTooLarge {
			t.Fatalf("oversized page failure = %+v", failure)
		}
		if _, ok := readers[pages[3].path]; ok || len(readers) != 5 {
			t.Fatalf("the rest of the group was not kept: %d readers", len(readers))
		}
	})

	t.Run("a_page_replaced_while_its_group_renders_keeps_no_stale_output", func(t *testing.T) {
		pages := landscapePages(t, 5, 20)
		g := newPageGroups(t, m, "Replaced during rendering", pages)
		replaced := pages[2].path
		original := g.blobKey(t, replaced)
		g.store.holdRead(original)
		done := make(chan error, 1)
		go func() {
			done <- g.processor(t, mediaimage.Config{Workers: 2, GroupWait: time.Minute}).Process(g.job(t.Context()), mediastore.ProcessJob{Ref: g.target})
		}()
		g.store.awaitHeld(t)
		_, err := g.client.PostMediaUploadCommit(t.Context(), media.CommitBody{Ref: g.ref, OperationID: uuid.NewString(),
			Ops: []media.Op{stageFile(t, g.client, g.ref, replaced, pageImage(t, 64, 96, 29))}})
		must(t, err)
		g.place(t, 1)
		current := g.blobKey(t, replaced)
		if current == original {
			t.Fatal("replacement was not placed")
		}
		// The group records nothing before the held page finishes. Its edit
		// is observed while the next pass reads the new source.
		before := g.manifestEdits(t)
		first := g.store.release
		g.store.holdRead(current)
		close(first)
		g.store.awaitHeld(t)
		if edits := g.manifestEdits(t) - before; edits != 1 {
			t.Fatalf("the group took %d manifest edits, want 1", edits)
		}
		if _, readers := g.readers(t); len(readers) != 4 {
			t.Fatalf("the group recorded %d pages, want the 4 unchanged", len(readers))
		}
		close(g.store.release)
		must(t, <-done)
		if edits := g.manifestEdits(t) - before; edits != 2 {
			t.Fatalf("replacement took %d manifest edits, want 2", edits)
		}
		_, readers := g.readers(t)
		if len(readers) != 5 {
			t.Fatalf("%d of 5 pages recorded their reader output", len(readers))
		}
		if out := readers[replaced]; out.W != 32 || out.H != 48 {
			t.Fatalf("replaced page carries a %dx%d reader output, want the new portrait 32x48", out.W, out.H)
		}
	})

	t.Run("cancellation_records_nothing_of_its_group_and_the_next_run_converges", func(t *testing.T) {
		pages := landscapePages(t, 6, 30)
		g := newPageGroups(t, m, "Cancelled while rendering", pages)
		g.store.holdRead(g.blobKey(t, pages[5].path))
		before := g.manifestEdits(t)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan error, 1)
		go func() {
			done <- g.processor(t, mediaimage.Config{Workers: 2, GroupWait: time.Minute}).Process(g.job(ctx), mediastore.ProcessJob{Ref: g.target})
		}()
		g.store.awaitHeld(t)
		waitForWithin(t, 30*time.Second, func() bool { return len(g.store.written()) == 5 })
		cancel()
		if err := <-done; err == nil {
			t.Fatal("cancelled processing reported success")
		}
		if edits := g.manifestEdits(t) - before; edits != 0 {
			t.Fatalf("cancelled group made %d manifest edits", edits)
		}
		if _, readers := g.readers(t); len(readers) != 0 {
			t.Fatalf("cancelled group recorded %d of its pages", len(readers))
		}
		// Written but unrecorded outputs stay owned by their allocations, which
		// the existing sweep retires once they expire unreferenced.
		unrecorded := g.store.written()
		for _, key := range unrecorded {
			if _, err := m.storage.store.Head(t.Context(), key); err != nil {
				t.Fatalf("unrecorded output %s: %v", key, err)
			}
		}
		var owned int
		must(t, m.pool.QueryRow(t.Context(), `SELECT count(*) FROM `+pgx.Identifier{m.schema, "content_media_allocations"}.Sanitize()+`
WHERE tenant_id=$1 AND folder_prefix=$2 AND object_key=ANY($3) AND retired_at IS NULL`, g.target.TenantID, g.item.Prefix(), unrecorded).Scan(&owned))
		if owned != len(unrecorded) {
			t.Fatalf("%d of %d unrecorded outputs keep their allocation", owned, len(unrecorded))
		}
		must(t, g.processor(t, mediaimage.Config{Workers: 2, GroupWait: time.Minute}).Process(g.job(t.Context()), mediastore.ProcessJob{Ref: g.target}))
		if edits := g.manifestEdits(t) - before; edits != 1 {
			t.Fatalf("retry took %d manifest edits, want 1", edits)
		}
		_, readers := g.readers(t)
		if len(readers) != 6 {
			t.Fatalf("retry recorded %d of 6 pages", len(readers))
		}
		for _, out := range readers {
			key, err := g.item.Blob(out.Blob)
			must(t, err)
			if slices.Contains(unrecorded, key) {
				t.Fatalf("retry recorded the cancelled run's output %s", key)
			}
		}
	})

	t.Run("a_group_over_the_manifest_bound_keeps_what_fits_and_marks_it_full", func(t *testing.T) {
		// Measure what recording one page adds, on a gallery of the same shape.
		sample := newPageGroups(t, m, "Manifest growth sample", landscapePages(t, 6, 40))
		empty := sample.size(t)
		must(t, sample.processor(t, mediaimage.Config{Workers: 2, GroupWait: time.Minute}).Process(sample.job(t.Context()), mediastore.ProcessJob{Ref: sample.target}))
		perPage := (sample.size(t) - empty) / 6
		// Leave room for three and a half pages under the edit bound
		// (MaxManifestBytes less mediastore's 4 KiB editHeadroom).
		g := newPageGroups(t, m, "Nearly full gallery", landscapePages(t, 6, 50))
		g.pad(t, media.MaxManifestBytes-4<<10-perPage*7/2)
		must(t, g.processor(t, mediaimage.Config{Workers: 2, GroupWait: time.Minute}).Process(g.job(t.Context()), mediastore.ProcessJob{Ref: g.target}))
		snapshot, readers := g.readers(t)
		if !snapshot.Full || len(readers) != 3 {
			t.Fatalf("full=%v with %d of 6 pages recorded; alone, three fit before the bound", snapshot.Full, len(readers))
		}
		for _, f := range snapshot.Files {
			if f.IsUpload() && f.Fail() != nil {
				t.Fatalf("%s failed instead of waiting for room: %+v", f.Path, f.Fail())
			}
		}
	})
}

type pageSource struct {
	path string
	data []byte
}

func landscapePages(t *testing.T, n, seed int) []pageSource {
	pages := make([]pageSource, n)
	for i := range pages {
		pages[i] = pageSource{fmt.Sprintf("pages/p%02d.png", i), pageImage(t, 96, 64, seed+i)}
	}
	return pages
}

func pageImage(t testing.TB, width, height, seed int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := range height {
		for x := range width {
			img.SetNRGBA(x, y, color.NRGBA{R: uint8(x*2 + seed), G: uint8(y*3 + seed*7), B: uint8(40 + seed), A: 255})
		}
	}
	var encoded bytes.Buffer
	must(t, png.Encode(&encoded, img))
	return encoded.Bytes()
}

type pageGroups struct {
	m         *resilientMedia
	client    *ck.Client
	ref       media.RefBody
	target    contentref.ContentRef
	item      mediastore.Item
	store     *pageStore
	manifests *mediastore.Manifests
	jobs      *river.Client[pgx.Tx]
}

func newPageGroups(t *testing.T, m *resilientMedia, title string, pages []pageSource) *pageGroups {
	t.Helper()
	g := &pageGroups{m: m, client: m.app.engine.Client(), store: &pageStore{Store: m.storage.store}, jobs: m.queue(t)}
	g.ref = galleryRef(t, g.client, title)
	var err error
	g.target, err = m.config.Kinds.VersionRef(g.ref.Kind, g.ref.ID, g.ref.Version)
	must(t, err)
	g.item, err = m.config.Kinds.Item(g.target)
	must(t, err)
	journal, err := mediastore.NewPGJournal(m.pool, m.schema, nil)
	must(t, err)
	g.manifests, err = mediastore.NewManifests(g.store, m.config.Kinds, mediastore.ManifestOptions{Locker: mediastore.PGLocker(m.pool), Journal: journal})
	must(t, err)
	ops := make([]media.Op, len(pages))
	for i, page := range pages {
		ops[i] = stageFile(t, g.client, g.ref, page.path, page.data)
	}
	_, err = g.client.PostMediaUploadCommit(t.Context(), media.CommitBody{Ref: g.ref, OperationID: uuid.NewString(), Ops: ops})
	must(t, err)
	g.place(t, len(pages))
	return g
}

func (g *pageGroups) processor(t *testing.T, c mediaimage.Config) *mediaimage.Processor {
	t.Helper()
	c.Store, c.Manifests = g.store, g.manifests
	p, err := mediaimage.New(c)
	must(t, err)
	return p
}

// job is ctx as the media worker's jobs see it: their edits are owned by
// the running job, not admitted as new ingest work.
func (g *pageGroups) job(ctx context.Context) context.Context {
	return rivertest.WorkContext(ctx, g.jobs)
}

// place does the media worker's ingest step: staged uploads become blobs.
func (g *pageGroups) place(t *testing.T, want int) {
	t.Helper()
	placed, err := g.manifests.Place(g.job(t.Context()), g.target)
	must(t, err)
	if placed != want {
		t.Fatalf("placed %d staged pages, want %d", placed, want)
	}
}

func (g *pageGroups) blobKey(t *testing.T, path string) string {
	t.Helper()
	snapshot, _, err := g.manifests.Get(t.Context(), g.target)
	must(t, err)
	f, ok := snapshot.Get(path)
	if !ok || f.Blob == "" {
		t.Fatalf("%s is not placed", path)
	}
	key, err := g.item.Blob(f.Blob)
	must(t, err)
	return key
}

// manifestEdits counts the folder's manifest edits in the journal: every
// commit but the allocation and retirement records.
func (g *pageGroups) manifestEdits(t *testing.T) int {
	t.Helper()
	var n int
	must(t, g.m.pool.QueryRow(t.Context(), `SELECT count(*) FROM `+pgx.Identifier{g.m.schema, "content_media_commits"}.Sanitize()+`
WHERE tenant_id=$1 AND folder_prefix=$2 AND NOT coalesce((effects->>'cleanup')::boolean, false)`, g.target.TenantID, g.item.Prefix()).Scan(&n))
	return n
}

// readers returns the pages' reader outputs, failing on one that is not the
// rendition of its page's current source.
func (g *pageGroups) readers(t *testing.T) (*mediastore.Manifest, map[string]media.File) {
	t.Helper()
	snapshot, _, err := g.manifests.Get(t.Context(), g.target)
	must(t, err)
	out := map[string]media.File{}
	for _, f := range snapshot.Files {
		outputs := snapshot.Outputs(f.Path, "reader")
		if !f.IsUpload() || len(outputs) == 0 {
			continue
		}
		reader := g.item.Kind().PrivateFor(f.Path)[0]
		if len(outputs) != 1 || outputs[0].FP != mediastore.SpecFP(f, reader.ImageFor(media.Dims{W: f.W, H: f.H}), mediaimage.Recipe) {
			t.Fatalf("%s carries an output of another source: %+v", f.Path, outputs)
		}
		out[f.Path] = outputs[0]
	}
	return snapshot, out
}

type processingEvent struct {
	typ  string
	fact media.ProcessingEvent
}

func (g *pageGroups) processingEvents(t *testing.T) []processingEvent {
	t.Helper()
	var out []processingEvent
	query := catalog.EventQuery{Types: []string{media.EventProcessingProgressed, media.EventProcessingCompleted}}
	for {
		page, err := g.client.ListEvents(t.Context(), query)
		must(t, err)
		for _, record := range page.Events {
			if record.ContentID != g.ref.ID {
				continue
			}
			event := processingEvent{typ: record.Type}
			must(t, json.Unmarshal(record.Data, &event.fact))
			out = append(out, event)
		}
		if !page.HasMore {
			return out
		}
		query.After = page.NextCursor
	}
}

// size is the manifest's JSON length, as its edit bound counts it.
func (g *pageGroups) size(t *testing.T) int64 {
	t.Helper()
	snapshot, _, err := g.manifests.Get(t.Context(), g.target)
	must(t, err)
	var encoded bytes.Buffer
	enc := json.NewEncoder(&encoded)
	enc.SetEscapeHTML(false)
	must(t, enc.Encode(snapshot))
	return int64(encoded.Len())
}

// pad fills the item's metadata until the manifest is size bytes.
func (g *pageGroups) pad(t *testing.T, size int64) {
	t.Helper()
	filler := 0
	for range 3 {
		filler += int(size - g.size(t))
		_, err := g.manifests.EditExisting(g.job(t.Context()), g.target, func(cur *mediastore.Manifest) error {
			cur.Meta = maps.Clone(cur.Meta)
			if cur.Meta == nil {
				cur.Meta = map[string]any{}
			}
			cur.Meta["filler"] = strings.Repeat("x", filler)
			return nil
		})
		must(t, err)
		if g.size(t) == size {
			return
		}
	}
	t.Fatalf("manifest is %d bytes, want %d", g.size(t), size)
}

// pageStore is the real bucket. Its test can hold one source after its bytes
// were read, as a slow decode would, and sees which rendered outputs were
// written.
type pageStore struct {
	media.Store
	mu      sync.Mutex
	hold    string
	held    chan struct{}
	release chan struct{}
	outputs []string
}

func (s *pageStore) holdRead(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hold, s.held, s.release = key, make(chan struct{}), make(chan struct{})
}

func (s *pageStore) awaitHeld(t *testing.T) {
	t.Helper()
	select {
	case <-s.held:
	case <-time.After(30 * time.Second):
		t.Fatal("processor never read the held source")
	}
}

func (s *pageStore) written() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.outputs)
}

func (s *pageStore) Get(ctx context.Context, key string, options media.GetOptions) (io.ReadCloser, media.Object, error) {
	body, object, err := s.Store.Get(ctx, key, options)
	s.mu.Lock()
	hold, held, release := err == nil && key == s.hold, s.held, s.release
	if hold {
		s.hold = ""
	}
	s.mu.Unlock()
	if !hold {
		return body, object, err
	}
	data, err := io.ReadAll(body)
	body.Close()
	if err != nil {
		return nil, object, err
	}
	close(held)
	select {
	case <-release:
		return io.NopCloser(bytes.NewReader(data)), object, nil
	case <-ctx.Done():
		return nil, object, ctx.Err()
	}
}

func (s *pageStore) Put(ctx context.Context, key string, body io.Reader, size int64, options media.PutOptions) (media.Object, error) {
	object, err := s.Store.Put(ctx, key, body, size, options)
	if err == nil && options.ContentType == "image/webp" {
		s.mu.Lock()
		s.outputs = append(s.outputs, key)
		s.mu.Unlock()
	}
	return object, err
}
