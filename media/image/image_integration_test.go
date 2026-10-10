package image_test

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image/color"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/image"
	"github.com/open-rails/contentkit/media/internal/s3test"
)

// Image presets render each page through its edit, record provenance and
// clear pending; the zip packs the high files in manifest order and is
// rebuilt when they move; a second pass redoes nothing.
func TestPrivatePresetsAndZip(t *testing.T) {
	e := newEnv(t, nil)
	g := e.ref(t, "gallery", 1)
	for i, c := range []color.RGBA{red, green, blue} {
		e.put(t, g, fmt.Sprintf("originals/%d.png", i+1), "image/png", solid(t, 300, 400, c))
	}
	e.commit(t, g, media.Op{Op: media.OpMeta, Meta: map[string]any{"title": "Book"}})
	e.process(t, media.ProcessJob{Ref: g})
	m := e.manifest(t, g)
	k, _ := e.reg.Kind("gallery")
	if r := k.Readiness(m); !r.Ready() {
		t.Fatalf("readiness %+v", r)
	}
	th := e.file(t, g, "thumb/2.webp")
	if th.From != "originals/2.png" || th.Preset != "thumb" || th.FP == "" || th.W != 100 || th.H != 150 {
		t.Fatalf("thumb %+v", th)
	}
	pixels(t, e.blob(t, g, th), 100, 150, map[[2]int]color.RGBA{{50, 75}: green})
	if up := e.file(t, g, "originals/1.png"); up.W != 300 || up.H != 400 || up.Pending != nil {
		t.Fatalf("measured upload %+v", up)
	}
	z := e.file(t, g, "download/pages.zip")
	if z.Download != "Book.zip" || z.From != "high/" || z.FP != media.ZipFP(k.ZipInputs(m, &k.Private[2])) {
		t.Fatalf("zip %+v", z)
	}
	names := func(b []byte) []string {
		zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, f := range zr.File {
			out = append(out, f.Name)
		}
		return out
	}
	if got := names(e.blob(t, g, z)); !slices.Equal(got, []string{"1.webp", "2.webp", "3.webp"}) {
		t.Fatalf("zip entries %v", got)
	}
	before := e.manifest(t, g)
	e.process(t, media.ProcessJob{Ref: g})
	if after := e.manifest(t, g); !slices.EqualFunc(before.Files, after.Files, func(a, b media.File) bool { return a.Blob == b.Blob && a.Path == b.Path }) {
		t.Fatal("a second pass changed outputs")
	}
	e.commit(t, g, media.Op{Op: media.OpMove, Path: "originals/3.png", Index: new(int)})
	e.process(t, media.ProcessJob{Ref: g})
	if got := names(e.blob(t, g, e.file(t, g, "download/pages.zip"))); !slices.Equal(got, []string{"3.webp", "1.webp", "2.webp"}) {
		t.Fatalf("zip after a move %v", got)
	}
}

// A deploy that changes a preset's spec makes exactly that preset's outputs
// stale; Force redoes current ones; Preset limits a job.
func TestSpecChangeAndForce(t *testing.T) {
	e := newEnv(t, nil)
	g := e.ref(t, "gallery", 1)
	e.put(t, g, "originals/1.png", "image/png", solid(t, 300, 400, red))
	e.process(t, media.ProcessJob{Ref: g})
	high := e.file(t, g, "high/1.webp")
	thumb := e.file(t, g, "thumb/1.webp")
	e.deploy(t, func(c *media.Config) {
		c.Kinds[0].Private[0].Image = &media.Image{Width: 50, Height: 50, Fit: media.FitCover}
	})
	e.process(t, media.ProcessJob{Ref: g})
	if th := e.file(t, g, "thumb/1.webp"); th.FP == thumb.FP || th.W != 50 {
		t.Fatalf("thumb not regenerated %+v", th)
	}
	if h := e.file(t, g, "high/1.webp"); h.FP != high.FP || h.Blob != high.Blob {
		t.Fatalf("high regenerated %+v", h)
	}
	if _, err := e.ms.EditExisting(context.Background(), g, func(m *media.Manifest) error {
		m.Files[m.Find("high/1.webp")].Blob = thumb.Blob // a wrong blob a producer fix must redo
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	e.process(t, media.ProcessJob{Ref: g, Preset: "thumb", Force: true})
	if h := e.file(t, g, "high/1.webp"); h.Blob != thumb.Blob {
		t.Fatal("Preset thumb touched high")
	}
	e.process(t, media.ProcessJob{Ref: g, Preset: "high", Force: true})
	if h := e.file(t, g, "high/1.webp"); h.Blob == thumb.Blob || h.Blob == high.Blob || h.FP != high.FP || h.W != high.W || h.H != high.H {
		t.Fatalf("Force did not redo high: %+v", h)
	}
}

// Public presets render fixed names through the crop at the preset's
// aspect, never upscaled, with their provenance as object metadata, and are
// purged on every write; a removed upload's names are deleted and purged; a
// hidden item renders none.
func TestPublicPreset(t *testing.T) {
	var removed []string
	e := newEnv(t, func(c *media.Config) {
		c.Hooks.PurgePublic = func(_ context.Context, urls []string) { removed = append(removed, urls...) }
	})
	g := e.ref(t, "gallery", 1)
	e.put(t, g, "cover.png", "image/png", quadrants(t), media.Op{Op: media.OpEdit, Path: "cover.png", Edit: &media.Edit{Crop: &media.Crop{X: 0, Y: 0, W: 400, H: 1}}})
	e.process(t, media.ProcessJob{Ref: g})
	c := e.file(t, g, "cover.png")
	if c.Pending != nil || c.Edit.Crop.H != 133 || c.W != 400 || c.H != 200 {
		t.Fatalf("cover %+v", c)
	}
	for _, w := range []int{150, 300} {
		b, obj, ok := e.public(t, g, fmt.Sprintf("cover-%d.webp", w))
		if !ok || obj.Metadata["fp"] == "" || obj.Metadata["from"] != "cover.png" {
			t.Fatalf("cover-%d: %v %+v", w, ok, obj.Metadata)
		}
		pixels(t, b, w, w/3, map[[2]int]color.RGBA{{w / 4, w / 12}: red, {3 * w / 4, w / 12}: blue})
	}
	// 600 is wider than the 400px edit: rendered at the edited width.
	if b, _, ok := e.public(t, g, "cover-600.webp"); !ok {
		t.Fatal("cover-600 missing")
	} else if w, h := webpSize(t, b); w != 400 || h != 133 {
		t.Fatalf("cover-600 is %dx%d", w, h)
	}
	if p := e.takePurged(); len(p) != 3 {
		t.Fatalf("purged %v", p)
	}
	e.process(t, media.ProcessJob{Ref: g})
	if p := e.takePurged(); len(p) != 0 {
		t.Fatalf("an unchanged cover was rewritten: %v", p)
	}
	e.commit(t, g, media.Op{Op: media.OpEdit, Path: "cover.png", Edit: &media.Edit{Crop: &media.Crop{X: 200, Y: 100, W: 200, H: 1}}})
	e.process(t, media.ProcessJob{Ref: g})
	b, _, _ := e.public(t, g, "cover-150.webp")
	pixels(t, b, 150, 50, map[[2]int]color.RGBA{{75, 25}: white})
	if p := e.takePurged(); len(p) != 3 {
		t.Fatalf("re-crop purged %v", p)
	}
	if len(removed) != 3 {
		t.Fatalf("re-crop did not retire its old public generation: %v", removed)
	}
	removed = nil
	if _, err := e.up.Commit(context.Background(), e.editor, g, uuid.NewString(), []media.Op{{Op: media.OpEdit, Path: "cover.png", Edit: &media.Edit{Crop: &media.Crop{X: 0, Y: 0, W: 50, H: 1}}}}); err == nil {
		t.Fatal("an edit under MinWidth accepted")
	}
	e.commit(t, g, media.Op{Op: media.OpRemove, Path: "cover.png"})
	if _, _, ok := e.public(t, g, "cover-150.webp"); ok {
		t.Fatal("remove returned before public cleanup")
	}
	if len(removed) != 3 {
		t.Fatalf("removal purged %v", removed)
	}
	e.process(t, media.ProcessJob{Ref: g})
	// A hidden item renders nothing public.
	h := e.ref(t, "gallery", 2)
	e.put(t, h, "cover.png", "image/png", quadrants(t))
	if _, err := e.ms.EditExisting(context.Background(), h, func(m *media.Manifest) error {
		m.Hidden = true
		m.Files[m.Find("cover.png")].Pending = nil
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	e.process(t, media.ProcessJob{Ref: h})
	if _, _, ok := e.public(t, h, "cover-150.webp"); ok {
		t.Fatal("a hidden item rendered its cover")
	}
}

func TestRemoveFencesInFlightPublicPublication(t *testing.T) {
	e := newEnv(t, nil)
	ref := e.ref(t, "gallery", 1)
	e.put(t, ref, "cover.png", "image/png", quadrants(t))
	e.process(t, media.ProcessJob{Ref: ref})
	item, _ := e.reg.Item(ref)
	source, _ := item.Blob(e.file(t, ref, "cover.png").Blob)
	reading, resume := make(chan struct{}), make(chan struct{})
	var once, release sync.Once
	defer release.Do(func() { close(resume) })
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var publicWrites atomic.Int64
	s := &hooked{Store: e.Store,
		onGet: func(key string) {
			if key == source {
				once.Do(func() {
					close(reading)
					select {
					case <-resume:
					case <-ctx.Done():
					}
				})
			}
		},
		onPut: func(key string, put func() error) error {
			if strings.HasPrefix(key, item.PublicPrefix()) {
				publicWrites.Add(1)
			}
			return put()
		},
	}
	processor := e.processor(t, s)
	done := make(chan error, 1)
	go func() { done <- processor.Process(ctx, media.ProcessJob{Ref: ref, Force: true}) }()
	select {
	case <-reading:
	case err := <-done:
		t.Fatalf("worker exited before the publication pause: %v", err)
	}
	op := media.Op{Op: media.OpRemove, Path: "cover.png"}
	e.commit(t, ref, op)
	if _, _, ok := e.public(t, ref, "cover-150.webp"); ok {
		t.Fatal("remove returned before deleting the old public image")
	}
	e.commit(t, ref, op) // a retry must still confirm cleanup
	if _, err := e.Store.Head(ctx, source); err != nil {
		t.Fatalf("ordinary removal lost the retained private blob: %v", err)
	}
	release.Do(func() { close(resume) })
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if n := publicWrites.Load(); n != 0 {
		t.Fatalf("in-flight worker published %d public files after removal returned", n)
	}
}

// A PUT can outlive the manifest revision that authorized it. It must write
// only its retired generation, never the current cover's filename or pixels.
func TestPublicGenerationsFenceLateWrites(t *testing.T) {
	e := newEnv(t, nil)
	ref := e.ref(t, "gallery", 1)
	e.put(t, ref, "cover.png", "image/png", quadrants(t))
	e.process(t, media.ProcessJob{Ref: ref})
	item, _ := e.reg.Item(ref)
	started, resume := make(chan struct{}), make(chan struct{})
	var paused atomic.Bool
	var release sync.Once
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	defer release.Do(func() { close(resume) })
	s := &hooked{Store: e.Store, onPut: func(key string, put func() error) error {
		if strings.HasPrefix(key, item.PublicPrefix()) && paused.CompareAndSwap(false, true) {
			close(started)
			select {
			case <-resume:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return put()
	}}
	worker := e.processor(t, s)
	done := make(chan error, 1)
	go func() { done <- worker.Process(ctx, media.ProcessJob{Ref: ref, Force: true, Preset: "cover"}) }()
	select {
	case <-started:
	case err := <-done:
		t.Fatalf("worker exited before the delayed PUT: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	old, _ := e.file(t, ref, "cover.png").Publication("cover")
	if old.Ready() {
		t.Fatal("unwritten reservation was published")
	}
	// Cleanup during the PUT must protect this active reservation.
	if _, err := e.ms.SyncPublic(ctx, ref); err != nil {
		t.Fatal(err)
	}
	e.commit(t, ref, media.Op{Op: media.OpEdit, Path: "cover.png",
		Edit: &media.Edit{Crop: &media.Crop{X: 200, Y: 100, W: 200, H: 1}}})
	e.process(t, media.ProcessJob{Ref: ref})
	current, _ := e.file(t, ref, "cover.png").Publication("cover")
	if !current.Ready() || old.Generation == current.Generation {
		t.Fatalf("replacement reused the delayed generation: old=%+v, new=%+v", old, current)
	}
	release.Do(func() { close(resume) })
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	after, _ := e.file(t, ref, "cover.png").Publication("cover")
	if after.Generation != current.Generation {
		t.Fatalf("old worker replaced the current publication: %+v", after)
	}
	b, _, ok := e.public(t, ref, "cover-150.webp")
	if !ok {
		t.Fatal("current cover disappeared")
	}
	pixels(t, b, 150, 50, map[[2]int]color.RGBA{{75, 25}: white})
	for _, name := range old.NamesOnDisk() {
		key, _ := item.Public(name)
		if _, err := e.Store.Head(ctx, key); !errors.Is(err, media.ErrNotFound) {
			t.Fatalf("retired delayed PUT was not reclaimed: %s: %v", key, err)
		}
	}
}

func TestPublicReservationRecoversInterruptedWrite(t *testing.T) {
	e := newEnv(t, nil)
	ref := e.ref(t, "gallery", 1)
	e.put(t, ref, "cover.png", "image/png", quadrants(t))
	item, _ := e.reg.Item(ref)
	var interrupted atomic.Bool
	var verified atomic.Bool
	failure := errors.New("response lost after public PUT")
	s := &hooked{Store: e.Store, omitChecksums: true, onGet: func(key string) {
		if strings.HasPrefix(key, item.PublicPrefix()) {
			verified.Store(true)
		}
	}, onPut: func(key string, put func() error) error {
		err := put()
		if err == nil && strings.HasPrefix(key, item.PublicPrefix()) && interrupted.CompareAndSwap(false, true) {
			return failure
		}
		return err
	}}
	worker := e.processor(t, s)
	if err := worker.Process(t.Context(), media.ProcessJob{Ref: ref}); !errors.Is(err, failure) {
		t.Fatalf("write interruption: %v", err)
	}
	reserved, _ := e.file(t, ref, "cover.png").Publication("cover")
	if reserved.Generation == "" || reserved.Ready() {
		t.Fatalf("lost the unpublished reservation: %+v", reserved)
	}
	keys, err := e.ms.SyncPublic(t.Context(), ref)
	if err != nil || len(keys) != 0 {
		t.Fatalf("cleanup retired active outputs: %v, %v", keys, err)
	}
	if err := worker.Process(t.Context(), media.ProcessJob{Ref: ref}); err != nil {
		t.Fatal(err)
	}
	if !verified.Load() {
		t.Fatal("retry accepted an existing allocation without a checksum or reading its bytes")
	}
	pub, _ := e.file(t, ref, "cover.png").Publication("cover")
	if !pub.Ready() || pub.Generation != reserved.Generation {
		t.Fatalf("retry failed to publish its reserved generation: before=%+v, after=%+v", reserved, pub)
	}
	for i, name := range pub.NamesOnDisk() {
		b, _, ok := e.public(t, ref, name)
		if !ok {
			t.Fatalf("published missing rendition %s", name)
		}
		w, h := webpSize(t, b)
		if pub.Dims[i] != (media.Dims{W: w, H: h}) {
			t.Fatalf("published dimensions %+v do not describe %dx%d", pub.Dims[i], w, h)
		}
	}
}

type failDelete struct {
	media.Store
	key    string
	err    error
	failed atomic.Bool
}

func (s *failDelete) Delete(ctx context.Context, key string) error {
	if key == s.key && s.failed.CompareAndSwap(false, true) {
		return s.err
	}
	return s.Store.Delete(ctx, key)
}

type failingQueue struct {
	media.TransactionalProcessQueue
	err error
}

func (q *failingQueue) EnqueueTx(ctx context.Context, tx pgx.Tx, job media.ProcessJob) error {
	if err := q.TransactionalProcessQueue.EnqueueTx(ctx, tx, job); err != nil {
		return err
	}
	return q.err // fail after insertion, so the regression exercises rollback
}

func TestRemoveCleansPublicAfterCommitFailure(t *testing.T) {
	for _, scenario := range []string{"enqueue failure", "client disconnect"} {
		t.Run(scenario, func(t *testing.T) {
			e := newEnv(t, nil)
			ref := e.ref(t, "gallery", 1)
			e.put(t, ref, "cover.png", "image/png", quadrants(t))
			e.process(t, media.ProcessJob{Ref: ref})
			item, _ := e.reg.Item(ref)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			store := &hooked{Store: e.Store, onPut: func(key string, put func() error) error {
				err := put()
				if err == nil && key == item.ManifestKey() && scenario == "client disconnect" {
					cancel()
				}
				return err
			}}
			failure := errors.New("queue unavailable")
			queue := &failingQueue{TransactionalProcessQueue: e.queue}
			if scenario == "enqueue failure" {
				queue.err = failure
			}
			journal, err := media.NewPGJournal(e.Pool(), e.ContentSchema(), queue)
			if err != nil {
				t.Fatal(err)
			}
			manifests := s3test.Manifests(t, store, e.reg, media.ManifestOptions{Journal: journal})
			uploads, err := media.NewUploads(media.UploadOptions{Store: store, Manifests: manifests})
			if err != nil {
				t.Fatal(err)
			}
			_, err = uploads.Commit(ctx, e.editor, ref, uuid.NewString(), []media.Op{{Op: media.OpRemove, Path: "cover.png"}})
			if scenario == "enqueue failure" && !errors.Is(err, failure) || scenario == "client disconnect" && err != nil {
				t.Fatalf("commit did not preserve the queue error: %v", err)
			}
			for obj, err := range e.Store.List(t.Context(), item.PublicPrefix()) {
				if err != nil {
					t.Fatal(err)
				}
				t.Fatalf("public file survived %s: %s", scenario, obj.Key)
			}
			queue.err = nil
			for range 2 {
				if err := manifests.Recover(t.Context(), ref); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

type parallelDelete struct {
	media.Store
	started atomic.Int64
	ready   chan struct{}
}

func (s *parallelDelete) Delete(ctx context.Context, key string) error {
	if s.started.Add(1) == 3 {
		close(s.ready)
	}
	select {
	case <-s.ready:
		return s.Store.Delete(ctx, key)
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestPublicCleanupDeletesConcurrently(t *testing.T) {
	e := newEnv(t, nil)
	ref := e.ref(t, "gallery", 1)
	e.put(t, ref, "cover.png", "image/png", quadrants(t))
	e.process(t, media.ProcessJob{Ref: ref})
	if _, err := e.ms.EditExisting(t.Context(), ref, func(m *media.Manifest) error {
		m.Files = nil
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	store := &parallelDelete{Store: e.Store, ready: make(chan struct{})}
	manifests := s3test.Manifests(t, store, e.reg, media.ManifestOptions{Journal: e.journal})
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	keys, err := manifests.SyncPublic(ctx, ref)
	if err != nil || len(keys) != 3 {
		t.Fatalf("public deletes did not overlap: keys=%v, err=%v", keys, err)
	}
	if _, err := e.ms.EditExisting(t.Context(), ref, func(*media.Manifest) error { return nil }); err != nil {
		t.Fatalf("cleanup did not release the folder lock: %v", err)
	}
}

func TestRemoveRetriesFailedPublicCleanup(t *testing.T) {
	e := newEnv(t, nil)
	ref := e.ref(t, "gallery", 1)
	e.put(t, ref, "cover.png", "image/png", quadrants(t))
	e.process(t, media.ProcessJob{Ref: ref})
	item, _ := e.reg.Item(ref)
	pub, _ := e.file(t, ref, "cover.png").Publication("cover")
	names := pub.NamesOnDisk()
	key, _ := item.Public(names[0])
	failure := errors.New("public delete unavailable")
	store := &failDelete{Store: e.Store, key: key, err: failure}
	manifests := s3test.Manifests(t, store, e.reg, media.ManifestOptions{Journal: e.journal})
	uploads, err := media.NewUploads(media.UploadOptions{Store: store, Manifests: manifests})
	if err != nil {
		t.Fatal(err)
	}
	ops := []media.Op{{Op: media.OpRemove, Path: "cover.png"}}
	if _, err := uploads.Commit(t.Context(), e.editor, ref, uuid.NewString(), ops); !errors.Is(err, failure) {
		t.Fatalf("remove did not report failed cleanup: %v", err)
	}
	if _, ok := e.manifest(t, ref).Get("cover.png"); ok {
		t.Fatal("cleanup failure unexpectedly restored the removed upload")
	}
	if _, err := uploads.Commit(t.Context(), e.editor, ref, uuid.NewString(), ops); err != nil {
		t.Fatalf("retry could not finish cleanup after the upload was removed: %v", err)
	}
	for _, name := range names {
		key, _ := item.Public(name)
		if _, err := e.Store.Head(t.Context(), key); !errors.Is(err, media.ErrNotFound) {
			t.Fatalf("retry kept %s: %v", name, err)
		}
	}
}

// EXIF orientation is applied before measuring and editing.
func TestOrientation(t *testing.T) {
	e := newEnv(t, nil)
	g := e.ref(t, "gallery", 1)
	img := paint(200, 100, func(x, _ int) color.RGBA {
		if x < 100 {
			return red
		}
		return blue
	})
	e.put(t, g, "originals/1.jpg", "image/jpeg", orientedJPEG(t, img, 6)) // 90° clockwise: displayed 100×200
	e.process(t, media.ProcessJob{Ref: g})
	if up := e.file(t, g, "originals/1.jpg"); up.W != 100 || up.H != 200 {
		t.Fatalf("oriented size %dx%d", up.W, up.H)
	}
	pixels(t, e.blob(t, g, e.file(t, g, "high/1.webp")), 100, 200, map[[2]int]color.RGBA{{50, 50}: red, {50, 150}: blue})
}

// Animations stay animated where allowed; a refusing preset, an
// undecodable file or bytes that do not match their hash fail the upload
// for its blob (Hooks.Failed), and the item reads as failed.
func TestAnimationAndFailures(t *testing.T) {
	e := newEnv(t, nil)
	a := e.ref(t, "anim", 1)
	e.put(t, a, "files/a.gif", "image/gif", animatedGIF(t))
	e.process(t, media.ProcessJob{Ref: a})
	w, h, n, delays := frames(t, e.blob(t, a, e.file(t, a, "large/a.webp")))
	if w != 40 || h != 40 || n != 4 || len(delays) != 4 || delays[3] != 400 {
		t.Fatalf("animated output %dx%d ×%d %v", w, h, n, delays)
	}
	u := e.ref(t, "user", 1)
	e.put(t, u, "avatar", "image/gif", animatedGIF(t))
	e.process(t, media.ProcessJob{Ref: u})
	if f := e.file(t, u, "avatar.gif").Fail(); f == nil || f.Code != media.CodeAnimationNotAllowed {
		t.Fatalf("animated avatar failure %+v", f)
	}
	k, _ := e.reg.Kind("user")
	if r := k.Readiness(e.manifest(t, u)); r.State != media.StateFailed {
		t.Fatalf("readiness %+v", r)
	}
	p := e.ref(t, "post", 1)
	e.put(t, p, "files/bad.png", "image/png", []byte("not a png at all"))
	e.process(t, media.ProcessJob{Ref: p})
	if f := e.file(t, p, "files/bad.png").Fail(); f == nil || f.Code != media.CodeImageUnreadable {
		t.Fatalf("undecodable %+v", f)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, want := range []string{"avatar.gif", "files/bad.png"} {
		if !slices.Contains(e.failed, want) {
			t.Fatalf("Hooks.Failed %v lacks %s", e.failed, want)
		}
	}
}

// Choose picks a per-file spec; KeepOriginals false drops an upload's blob
// once its outputs exist; editor views render on request.
func TestChooseGoneAndEditorViews(t *testing.T) {
	e := newEnv(t, func(c *media.Config) {
		post := &c.Kinds[3]
		post.Private[0].Choose = func(f media.File) *media.Image {
			if strings.Contains(f.Path, "tall") {
				return &media.Image{Width: 20, Height: 80}
			}
			return nil
		}
	})
	p := e.ref(t, "post", 1)
	e.put(t, p, "files/tall.png", "image/png", solid(t, 100, 400, red))
	e.put(t, p, "files/wide.png", "image/png", solid(t, 400, 200, red))
	e.process(t, media.ProcessJob{Ref: p, Editor: true})
	if w := e.file(t, p, "web/tall.webp"); w.W != 20 || w.H != 80 {
		t.Fatalf("chosen spec %dx%d", w.W, w.H)
	}
	if w := e.file(t, p, "web/wide.webp"); w.W != 50 || w.H != 25 {
		t.Fatalf("default spec %dx%d", w.W, w.H)
	}
	up := e.file(t, p, "files/wide.png")
	if !up.Gone {
		t.Fatalf("an upload of a kind without KeepOriginals kept: %+v", up)
	}
	if slices.Contains(e.manifest(t, p).Blobs(), up.Blob) {
		t.Fatal("a gone upload's blob is still referenced")
	}
	g := e.ref(t, "gallery", 1)
	e.put(t, g, "cover.png", "image/png", quadrants(t))
	e.process(t, media.ProcessJob{Ref: g, Editor: true})
	view := e.reg.EditorView(e.file(t, g, "cover.png"))
	item, _ := e.reg.Item(g)
	key, _ := item.Blob(view)
	b, _ := e.read(t, key)
	if w, h := webpSize(t, b); w != 400 || h != 200 {
		t.Fatalf("editor view %dx%d", w, h)
	}
}

// PublishDefaults renders each public default to its kind's _default item
// at every width, once.
func TestPublishDefaults(t *testing.T) {
	e := newEnv(t, nil)
	e.deploy(t, func(c *media.Config) {
		c.Kinds[0].Defaults = fstest.MapFS{"cover.png": {Data: quadrants(t)}}
	})
	ctx := context.Background()
	keys, err := image.PublishDefaults(ctx, e.Store, e.reg)
	if err != nil || len(keys) != 3 {
		t.Fatalf("published %v %v", keys, err)
	}
	k, _ := e.reg.Kind("gallery")
	b, obj := e.read(t, k.DefaultKey("cover-300.webp"))
	if w, h := webpSize(t, b); w != 300 || h != 100 || obj.Metadata["fp"] == "" {
		t.Fatalf("default %dx%d %+v", w, h, obj.Metadata)
	}
	if keys, err := image.PublishDefaults(ctx, e.Store, e.reg); err != nil || len(keys) != 0 {
		t.Fatalf("republished %v %v", keys, err)
	}
}

// A pass whose outputs the manifest cannot hold marks the item Full instead
// of recording them: later jobs render nothing, until a commit shrinks the
// manifest and the pass completes.
func TestFullManifestStopsProcessing(t *testing.T) {
	e := newEnv(t, nil)
	p := e.ref(t, "post", 1)
	ctx := context.Background()
	e.put(t, p, "files/a.png", "image/png", solid(t, 10, 10, red))
	// Fill the manifest to just under where edits stop (4 KiB short of the
	// bound): the output's record no longer fits.
	if _, err := e.ms.EditExisting(ctx, p, func(m *media.Manifest) error {
		c := *m
		c.Meta = map[string]any{"pad": ""}
		var b bytes.Buffer
		enc := json.NewEncoder(&b)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(&c); err != nil {
			return err
		}
		m.Meta = map[string]any{"pad": strings.Repeat("A", media.MaxManifestBytes-4<<10-100-b.Len())}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	e.process(t, media.ProcessJob{Ref: p})
	m := e.manifest(t, p)
	if f, _ := m.Get("files/a.png"); !m.Full || f.Fail() != nil || len(f.Pending) == 0 {
		t.Fatalf("full %v, upload %+v", m.Full, f)
	}
	purged := len(e.takePurged())
	e.process(t, media.ProcessJob{Ref: p})
	if m := e.manifest(t, p); len(m.Outputs("files/a.png", "web")) != 0 || len(e.takePurged()) != 0 {
		t.Fatalf("a full item was processed again (%d purged before)", purged)
	}
	e.commit(t, p, media.Op{Op: media.OpMeta})
	if m := e.manifest(t, p); m.Full {
		t.Fatal("a shrinking commit kept the item full")
	}
	e.process(t, media.ProcessJob{Ref: p})
	if m := e.manifest(t, p); len(m.Outputs("files/a.png", "web")) != 1 {
		t.Fatal("not processed once shrunk")
	}
}

// A Full item at the edit limit can replace an existing public reservation
// without growing it, while its private outputs stay stopped.
func TestFullStillRendersPublic(t *testing.T) {
	e := newEnv(t, nil)
	g := e.ref(t, "gallery", 1)
	ctx := context.Background()
	e.put(t, g, "cover.png", "image/png", quadrants(t))
	e.process(t, media.ProcessJob{Ref: g})
	if _, _, ok := e.public(t, g, "cover-150.webp"); !ok {
		t.Fatal("no cover")
	}
	e.put(t, g, "originals/1.png", "image/png", solid(t, 30, 40, red))
	if _, err := e.ms.EditExisting(ctx, g, func(m *media.Manifest) error {
		c := *m
		c.Meta = map[string]any{"pad": ""}
		var b bytes.Buffer
		enc := json.NewEncoder(&b)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(&c); err != nil {
			return err
		}
		m.Meta = map[string]any{"pad": strings.Repeat("A", media.MaxManifestBytes-4<<10-b.Len())}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	e.process(t, media.ProcessJob{Ref: g})
	if m := e.manifest(t, g); !m.Full || m.Deficit <= 0 {
		t.Fatalf("full %v, deficit %d", m.Full, m.Deficit)
	}
	// The objects disappeared but the manifest still owns their allocation.
	// With no pending marker there is no space to grow one: reservation and
	// completion must fit by replacing their fixed-width state.
	item, _ := e.reg.Item(g)
	pub, _ := e.file(t, g, "cover.png").Publication("cover")
	for _, name := range pub.NamesOnDisk() {
		key, _ := item.Public(name)
		if err := e.Store.Delete(ctx, key); err != nil {
			t.Fatal(err)
		}
	}
	e.process(t, media.ProcessJob{Ref: g})
	m := e.manifest(t, g)
	if _, _, ok := e.public(t, g, "cover-150.webp"); !ok || !m.Full {
		t.Fatalf("a full item's cover rendered %v, still full %v", ok, m.Full)
	}
	if c, _ := m.Get("cover.png"); len(c.Pending) != 0 || len(m.Outputs("originals/1.png", "thumb")) != 0 {
		t.Fatalf("cover pending %v, thumbs %v", c.Pending, m.Outputs("originals/1.png", "thumb"))
	}
}

// A public render that fails on a Full item records no failure (it would
// not fit) and drops the pending name: a job tries it once and ends, and no
// commit queues it again.
func TestFullPublicFailureDoesNotSpin(t *testing.T) {
	e := newEnv(t, nil)
	g := e.ref(t, "gallery", 1)
	ctx := context.Background()
	e.put(t, g, "cover.png", "image/png", []byte("not a png at all"))
	_, cause := e.ms.EditExisting(ctx, g, func(m *media.Manifest) error {
		m.Meta = map[string]any{"pad": strings.Repeat("A", media.MaxManifestBytes)}
		return nil
	})
	if !errors.Is(cause, media.ErrManifestTooLarge) {
		t.Fatalf("an oversized record: %v", cause)
	}
	if err := e.ms.SetFull(ctx, g, cause); err != nil {
		t.Fatal(err)
	}
	e.process(t, media.ProcessJob{Ref: g})
	m := e.manifest(t, g)
	if c, _ := m.Get("cover.png"); !m.Full || len(c.Pending) != 0 || c.Fail() != nil {
		t.Fatalf("full %v, cover %+v", m.Full, c)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.failed) != 1 {
		t.Fatalf("the failing render ran %d times in one job", len(e.failed))
	}
}
