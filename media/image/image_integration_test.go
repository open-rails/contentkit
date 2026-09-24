package image_test

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	stdimage "image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"golang.org/x/image/webp"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/image"
	"github.com/open-rails/contentkit/media/internal/s3test"
)

var (
	thumb = media.Spec{Width: 100, Height: 150, Fit: media.FitCover, Quality: 80}
	low   = media.Spec{Width: 300, Height: 300, Fit: media.FitInside, Quality: 90}
	high  = media.Spec{Quality: 90}
	cover = media.Slot{Aspect: 3, Widths: []int{150, 300, 600}}
)

func galleryKind() media.Kind {
	return media.Kind{Name: "gallery", Versioned: true, Types: []string{"image/png", "image/jpeg"}, MaxBytes: 10 << 20,
		Specs: map[string]media.Spec{"thumb": thumb, "low": low, "high": high}, Zip: "high",
		Slots: map[string]media.Slot{"cover": cover}}
}

// countingStore is the real store, counting and optionally holding reads of originals.
type countingStore struct {
	media.Store
	reads atomic.Int64
	gate  func()
}

func (s *countingStore) Get(ctx context.Context, key string, o media.GetOptions) (io.ReadCloser, media.Object, error) {
	if strings.Contains(key, "/originals/") && !strings.HasSuffix(key, ".json") { // slot records are not originals
		s.reads.Add(1)
		if s.gate != nil {
			s.gate()
		}
	}
	return s.Store.Get(ctx, key, o)
}

type allow struct{}

func (allow) CanUpload(context.Context, access.Actor, contentref.ContentRef) (media.UploadGrant, error) {
	return media.UploadGrant{Allowed: true}, nil
}

type queue struct {
	mu   sync.Mutex
	jobs []media.ProcessJob
}

func (q *queue) Enqueue(_ context.Context, j media.ProcessJob) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.jobs = append(q.jobs, j)
	return nil
}

func (q *queue) take() []media.ProcessJob {
	q.mu.Lock()
	defer q.mu.Unlock()
	j := q.jobs
	q.jobs = nil
	return j
}

type env struct {
	*s3test.Env
	store     *countingStore
	manifests *media.Manifests
	uploads   *media.Uploads
	queue     *queue
	proc      *image.Processor
	kinds     *media.Registry
	mu        sync.Mutex
	failed    []string
	stamps    map[string]media.SlotStamp // Hooks.SlotEncoded, by ref#slot
	reported  map[media.SlotStamp]bool
	late      map[media.SlotStamp]bool // first reported after the record showed it
}

func newEnv(t *testing.T, kind media.Kind) *env {
	t.Helper()
	s := s3test.Open(t)
	if !s.Store.Capabilities().ConditionalPut {
		t.Skip("backend lacks conditional PUT")
	}
	e := &env{Env: s, store: &countingStore{Store: s.Store}, queue: &queue{}}
	e.useKind(t, kind)
	return e
}

// useKind rebuilds the registry, manifests, uploads and processor: a deploy
// with new specs.
func (e *env) useKind(t *testing.T, kind media.Kind) {
	t.Helper()
	kinds, err := media.NewRegistry(kind)
	if err != nil {
		t.Fatal(err)
	}
	e.kinds = kinds
	e.manifests = s3test.Manifests(t, e.store, kinds, media.ManifestOptions{})
	if e.uploads, err = media.NewUploads(media.UploadOptions{Store: e.Env.Store, Kinds: kinds, Manifests: e.manifests,
		Authorizer: allow{}, Queue: e.queue}); err != nil {
		t.Fatal(err)
	}
	e.proc, err = image.New(image.Config{Store: e.store, Kinds: kinds, Manifests: e.manifests,
		Hooks: media.Hooks{Failed: func(_ context.Context, _ contentref.ContentRef, file string, _ error) {
			e.mu.Lock()
			e.failed = append(e.failed, file)
			e.mu.Unlock()
		}, SlotEncoded: func(ctx context.Context, ref contentref.ContentRef, slot string, stamp media.SlotStamp) {
			version, _, _ := stamp.Parse()
			rec, err := e.manifests.Slot(ctx, ref, slot)
			shown := err == nil && rec.Result != nil && rec.Result.Version == version
			e.mu.Lock()
			if e.stamps == nil {
				e.stamps, e.reported, e.late = map[string]media.SlotStamp{}, map[media.SlotStamp]bool{}, map[media.SlotStamp]bool{}
			}
			e.stamps[ref.String()+"#"+slot] = stamp
			if shown && !e.reported[stamp] {
				e.late[stamp] = true
			}
			e.reported[stamp] = true
			e.mu.Unlock()
		}}})
	if err != nil {
		t.Fatal(err)
	}
}

// upload presigns and PUTs a PNG like a browser, returning its original name.
func (e *env) upload(t *testing.T, ref contentref.ContentRef, slot string, body []byte) string {
	t.Helper()
	return e.uploadAs(t, ref, slot, "image/png", body)
}

func (e *env) uploadAs(t *testing.T, ref contentref.ContentRef, slot, typ string, body []byte) string {
	t.Helper()
	sum := sha256.Sum256(body)
	p, err := e.uploads.Presign(context.Background(), access.Actor{ID: "u"}, media.PresignRequest{Ref: ref, Type: typ,
		Size: int64(len(body)), SHA256: sum[:], Slot: slot})
	if err != nil {
		t.Fatal(err)
	}
	if p.Put != nil {
		req, _ := http.NewRequest(p.Put.Method, p.Put.URL, bytes.NewReader(body))
		for k := range p.Put.Header {
			req.Header.Set(k, p.Put.Header.Get(k))
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("put: %d", resp.StatusCode)
		}
	}
	if slot != "" {
		if err := e.uploads.CommitSlot(context.Background(), access.Actor{ID: "u"}, ref, slot, sum[:], nil); err != nil {
			t.Fatal(err)
		}
	}
	return p.Name
}

func (e *env) commit(t *testing.T, ref contentref.ContentRef, ops ...media.Op) {
	t.Helper()
	if _, err := e.uploads.Commit(context.Background(), access.Actor{ID: "u"}, ref, ops); err != nil {
		t.Fatal(err)
	}
}

// drain runs every queued job, as the River worker would.
func (e *env) drain(t *testing.T) {
	t.Helper()
	for _, j := range e.queue.take() {
		if err := e.proc.Process(context.Background(), j); err != nil {
			t.Fatal(err)
		}
	}
}

func (e *env) manifest(t *testing.T, ref contentref.ContentRef) (*media.Manifest, string) {
	t.Helper()
	m, etag, err := e.manifests.Get(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	return m, etag
}

func (e *env) object(t *testing.T, key string) ([]byte, media.Object) {
	t.Helper()
	rc, obj, err := e.Env.Store.Get(context.Background(), key, media.GetOptions{})
	if err != nil {
		t.Fatalf("get %s: %v", key, err)
	}
	defer rc.Close()
	b, _ := io.ReadAll(rc)
	return b, obj
}

func (e *env) blob(t *testing.T, ref contentref.ContentRef, name string) ([]byte, media.Object) {
	t.Helper()
	return e.object(t, e.Tenant+"/"+ref.ContentKind+"/"+ref.ContentID+"/blobs/"+name)
}

func pngImage(t *testing.T, w, h int, seed uint8) []byte {
	t.Helper()
	img := stdimage.NewRGBA(stdimage.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.RGBA{uint8(x) + seed, uint8(y) * seed, seed, 255})
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func webpSize(t *testing.T, b []byte) (int, int) {
	t.Helper()
	c, err := webp.DecodeConfig(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("not a webp: %v", err)
	}
	return c.Width, c.Height
}

func ins(name, original string) media.Op {
	return media.Op{Op: media.OpInsert, Name: name, Original: original}
}

func TestVariantsZipAndSkip(t *testing.T) {
	e := newEnv(t, galleryKind())
	ref := contentref.NewVersion(e.Tenant, "gallery", "1", "en")
	p1, p2 := pngImage(t, 800, 1200, 1), pngImage(t, 600, 900, 2)
	e.commit(t, ref, ins("001.png", e.upload(t, ref, "", p1)), ins("002.png", e.upload(t, ref, "", p2)))
	e.drain(t)

	m, etag := e.manifest(t, ref)
	want := map[string][3][2]int{"001.png": {{100, 150}, {200, 300}, {800, 1200}}, "002.png": {{100, 150}, {200, 300}, {600, 900}}}
	for _, f := range m.Files {
		for i, name := range []string{"thumb", "low", "high"} {
			v, ok := f.Variants[name]
			if !ok || v.Spec != galleryKind().Specs[name].Hash() || v.Type != "image/webp" {
				t.Fatalf("%s %s: %+v", f.Name, name, v)
			}
			b, obj := e.blob(t, ref, v.Blob)
			if obj.ContentType != "image/webp" || obj.CacheControl != "max-age=31536000, immutable" || int64(len(b)) != v.Size {
				t.Fatalf("%s %s blob: %+v", f.Name, name, obj)
			}
			if w, h := webpSize(t, b); [2]int{w, h} != want[f.Name][i] {
				t.Fatalf("%s %s: %dx%d, want %v", f.Name, name, w, h, want[f.Name][i])
			}
		}
		if f.Meta["w"] != float64(want[f.Name][2][0]) || f.Meta["h"] != float64(want[f.Name][2][1]) {
			t.Fatalf("%s meta: %v", f.Name, f.Meta)
		}
	}

	d, ok := m.Downloads["zip"]
	if !ok || d.Type != "application/zip" || d.Inputs == "" {
		t.Fatalf("zip: %+v", m.Downloads)
	}
	zb, _ := e.blob(t, ref, d.Blob)
	zr, err := zip.NewReader(bytes.NewReader(zb), int64(len(zb)))
	if err != nil || len(zr.File) != 2 {
		t.Fatalf("zip: %v %d", err, len(zr.File))
	}
	for i, zf := range zr.File {
		rc, _ := zf.Open()
		got, _ := io.ReadAll(rc)
		rc.Close()
		hb, _ := e.blob(t, ref, m.Files[i].Variants["high"].Blob)
		if zf.Name != []string{"001.webp", "002.webp"}[i] || !bytes.Equal(got, hb) || bytes.Equal(got, p1) || bytes.Equal(got, p2) {
			t.Fatalf("zip entry %d %q is not the high variant", i, zf.Name)
		}
	}

	// Unchanged sources and specs: nothing is read, encoded or written.
	e.store.reads.Store(0)
	for range 2 {
		if err := e.proc.Process(context.Background(), media.ProcessJob{Ref: ref}); err != nil {
			t.Fatal(err)
		}
	}
	if n := e.store.reads.Load(); n != 0 {
		t.Fatalf("unchanged sources read %d times", n)
	}
	if _, again := e.manifest(t, ref); again != etag {
		t.Fatal("unchanged run rewrote the manifest")
	}
}

func TestSpecChangeAndZipRebuild(t *testing.T) {
	e := newEnv(t, galleryKind())
	ref := contentref.NewVersion(e.Tenant, "gallery", "2", "en")
	e.commit(t, ref, ins("001.png", e.upload(t, ref, "", pngImage(t, 400, 600, 3))),
		ins("002.png", e.upload(t, ref, "", pngImage(t, 400, 600, 4))))
	e.drain(t)
	before, _ := e.manifest(t, ref)

	// A new thumb spec regenerates thumbs only; the zip's inputs are unchanged.
	k := galleryKind()
	k.Specs["thumb"] = media.Spec{Width: 120, Height: 120, Fit: media.FitCover, Quality: 80}
	e.useKind(t, k)
	e.store.reads.Store(0)
	if err := e.proc.Process(context.Background(), media.ProcessJob{Ref: ref}); err != nil {
		t.Fatal(err)
	}
	if n := e.store.reads.Load(); n != 2 {
		t.Fatalf("read %d originals, want 2", n)
	}
	after, _ := e.manifest(t, ref)
	for i, f := range after.Files {
		old := before.Files[i]
		if v := f.Variants["thumb"]; v.Spec != k.Specs["thumb"].Hash() || v.Blob == old.Variants["thumb"].Blob {
			t.Fatalf("%s thumb not regenerated: %+v", f.Name, v)
		}
		b, _ := e.blob(t, ref, f.Variants["thumb"].Blob)
		if w, h := webpSize(t, b); w != 120 || h != 120 {
			t.Fatalf("%s thumb is %dx%d", f.Name, w, h)
		}
		if f.Variants["low"] != old.Variants["low"] || f.Variants["high"] != old.Variants["high"] {
			t.Fatalf("%s: unchanged specs were regenerated", f.Name)
		}
	}
	if after.Downloads["zip"] != before.Downloads["zip"] {
		t.Fatal("zip rebuilt though its inputs did not change")
	}

	// A new high spec changes the zip's inputs: the zip is rebuilt.
	k.Specs["high"] = media.Spec{Quality: 60}
	e.useKind(t, k)
	if err := e.proc.Process(context.Background(), media.ProcessJob{Ref: ref}); err != nil {
		t.Fatal(err)
	}
	rebuilt, _ := e.manifest(t, ref)
	if z := rebuilt.Downloads["zip"]; z.Inputs == after.Downloads["zip"].Inputs || z.Blob == after.Downloads["zip"].Blob {
		t.Fatalf("zip not rebuilt: %+v", z)
	}

	// Replacing a page rebuilds the zip; dropping a spec prunes its variants.
	e.commit(t, ref, media.Op{Op: media.OpReplace, Name: "002.png", Original: e.upload(t, ref, "", pngImage(t, 300, 300, 5))})
	delete(k.Specs, "low")
	e.useKind(t, k)
	e.drain(t)
	final, _ := e.manifest(t, ref)
	if z := final.Downloads["zip"]; z.Inputs == rebuilt.Downloads["zip"].Inputs {
		t.Fatal("zip not rebuilt after a replace")
	}
	for _, f := range final.Files {
		if _, ok := f.Variants["low"]; ok || len(f.Variants) != 2 {
			t.Fatalf("%s variants: %v", f.Name, f.Variants)
		}
	}
	if final.Files[1].Meta["w"] != float64(300) {
		t.Fatalf("replaced page meta: %v", final.Files[1].Meta)
	}
}

func TestConcurrentEdits(t *testing.T) {
	e := newEnv(t, galleryKind())
	ref := contentref.NewVersion(e.Tenant, "gallery", "3", "en")
	e.commit(t, ref, ins("001.png", e.upload(t, ref, "", pngImage(t, 300, 400, 6))),
		ins("002.png", e.upload(t, ref, "", pngImage(t, 300, 400, 7))))
	e.queue.take()

	// The job holds the old sources while a commit replaces 001 and adds 003.
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	e.store.gate = func() { once.Do(func() { close(entered) }); <-release }
	done := make(chan error)
	go func() { done <- e.proc.Process(context.Background(), media.ProcessJob{Ref: ref}) }()
	<-entered
	replacement := e.upload(t, ref, "", pngImage(t, 500, 500, 8))
	e.commit(t, ref, media.Op{Op: media.OpReplace, Name: "001.png", Original: replacement},
		ins("003.png", e.upload(t, ref, "", pngImage(t, 300, 400, 9))))
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	e.store.gate = nil

	// The running job absorbs the commit: it derives the new sources too, and
	// never records the old 001's results under the replacement.
	m, _ := e.manifest(t, ref)
	if len(m.Files) != 3 || m.Files[0].Original != replacement {
		t.Fatalf("the concurrent commit was lost: %+v", m.Files)
	}
	for _, f := range m.Files {
		if len(f.Variants) != 3 {
			t.Fatalf("%s: %v", f.Name, f.Variants)
		}
	}
	hb, _ := e.blob(t, ref, m.Files[0].Variants["high"].Blob)
	if w, h := webpSize(t, hb); w != 500 || h != 500 || m.Files[0].Meta["w"] != float64(500) {
		t.Fatalf("001 carries the replaced source's results: %dx%d %v", w, h, m.Files[0].Meta)
	}
	if _, ok := m.Downloads["zip"]; !ok {
		t.Fatal("zip missing")
	}
	zipOf(t, e, ref, m)

	// The commit's queued job, run twice at once, finds nothing left to do.
	e.store.reads.Store(0)
	jobs := e.queue.take()
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			if err := e.proc.Process(context.Background(), jobs[0]); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if n := e.store.reads.Load(); n != 0 {
		t.Fatalf("completed item re-read %d originals", n)
	}
}

// zipOf checks the recorded zip holds the high variants in page order.
func zipOf(t *testing.T, e *env, ref contentref.ContentRef, m *media.Manifest) {
	t.Helper()
	d := m.Downloads["zip"]
	zb, _ := e.blob(t, ref, d.Blob)
	zr, err := zip.NewReader(bytes.NewReader(zb), int64(len(zb)))
	if err != nil || len(zr.File) != len(m.Files) {
		t.Fatalf("zip: %v, %d entries", err, len(zr.File))
	}
	for i, zf := range zr.File {
		rc, _ := zf.Open()
		got, _ := io.ReadAll(rc)
		rc.Close()
		hb, _ := e.blob(t, ref, m.Files[i].Variants["high"].Blob)
		if !bytes.Equal(got, hb) {
			t.Fatalf("zip entry %d is not page %d's high variant", i, i)
		}
	}
}

func TestUndecodableReportsFailed(t *testing.T) {
	e := newEnv(t, galleryKind())
	ref := contentref.NewVersion(e.Tenant, "gallery", "4", "en")
	e.commit(t, ref, ins("001.png", e.upload(t, ref, "", pngImage(t, 200, 200, 1))),
		ins("002.png", e.upload(t, ref, "", []byte("not an image at all"))))
	e.drain(t)
	m, _ := e.manifest(t, ref)
	if len(e.failed) != 1 || e.failed[0] != "002.png" {
		t.Fatalf("failed: %v", e.failed)
	}
	if len(m.Files[0].Variants) != 3 || len(m.Files[1].Variants) != 0 || m.Downloads["zip"].Blob != "" {
		t.Fatalf("manifest: %+v", m)
	}
}

// A kind's Types bind the decoder: bytes of another format under a declared
// image/png (a JPEG, an SVG) are refused, not handed to whichever libvips
// loader sniffs them (PDF, SVG, ImageMagick...).
func TestDeclaredTypeBindsTheDecoder(t *testing.T) {
	e := newEnv(t, galleryKind())
	ref := contentref.NewVersion(e.Tenant, "gallery", "7", "en")
	var jpg bytes.Buffer
	if err := jpeg.Encode(&jpg, stdimage.NewRGBA(stdimage.Rect(0, 0, 64, 64)), nil); err != nil {
		t.Fatal(err)
	}
	svg := []byte(`<svg xmlns="http://www.w3.org/2000/svg" width="64" height="64"><rect width="64" height="64" fill="red"/></svg>`)
	e.commit(t, ref, ins("001.png", e.upload(t, ref, "", pngImage(t, 64, 64, 1))),
		ins("002.png", e.upload(t, ref, "", jpg.Bytes())), ins("003.png", e.upload(t, ref, "", svg)))
	e.upload(t, ref.Content(), "cover", svg)
	e.drain(t)
	m, _ := e.manifest(t, ref)
	slices.Sort(e.failed)
	if failed := slices.Compact(e.failed); !slices.Equal(failed, []string{"002.png", "003.png", "cover"}) {
		t.Fatalf("failed: %v", failed)
	}
	if len(m.Files[0].Variants) != 3 || len(m.Files[1].Variants) != 0 || len(m.Files[2].Variants) != 0 {
		t.Fatalf("manifest: %+v", m)
	}
	if _, err := e.Env.Store.Head(context.Background(), e.Tenant+"/gallery/7/public/cover_150.webp"); !errors.Is(err, media.ErrNotFound) {
		t.Fatalf("cover derived from an SVG declared image/png: %v", err)
	}
}

// TestProcessDoesNotResurrectADeletedItem deletes the manifest while a pass
// derives: the pass records nothing and removes what it stored.
func TestProcessDoesNotResurrectADeletedItem(t *testing.T) {
	e := newEnv(t, galleryKind())
	ctx := context.Background()
	ref := contentref.NewVersion(e.Tenant, "gallery", "gone", "v1")
	e.commit(t, ref, media.Op{Op: media.OpInsert, Name: "1.png", Original: e.uploadAs(t, ref, "", "image/png", pngImage(t, 64, 64, 1))})
	item, _ := e.kinds.Item(ref)
	key, _ := item.ManifestKey()
	var once sync.Once
	e.store.gate = func() { once.Do(func() { _ = e.Env.Store.Delete(ctx, key) }) }
	e.drain(t)
	e.store.gate = nil
	for o, err := range e.Env.Store.List(ctx, item.Prefix()) {
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(o.Key, "/originals/") {
			t.Errorf("a pass over a deleted item left %s", o.Key)
		}
	}
}
