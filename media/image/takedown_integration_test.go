package image_test

import (
	"bytes"
	"context"
	"errors"
	"image/color"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/layout"
)

// headHook runs before and after around each Head.
type headHook struct {
	media.Store
	before, after func(key string)
}

func (s headHook) Head(ctx context.Context, key string) (media.Object, error) {
	if s.before != nil {
		s.before(key)
	}
	obj, err := s.Store.Head(ctx, key)
	if s.after != nil {
		s.after(key)
	}
	return obj, err
}

func (e *env) exists(t *testing.T, key string) bool {
	t.Helper()
	_, err := e.Store.Head(context.Background(), key)
	if err != nil && !errors.Is(err, media.ErrNotFound) {
		t.Fatal(err)
	}
	return err == nil
}

// Identical uploads have independent output allocations. Taking down one
// while the other renders cannot erase the other's newly written output.
func TestSharedOutputTakenDownMidPass(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	ref := e.ref(t, "post", 1)
	item, _ := e.reg.Item(ref)
	body := solid(t, 80, 80, color.RGBA{200, 30, 30, 255})
	e.put(t, ref, "files/a.png", "image/png", body)
	e.process(t, media.ProcessJob{Ref: ref})
	e.put(t, ref, "files/b.png", "image/png", body)
	shared, _ := item.Blob(e.file(t, ref, "web/a.webp").Blob)
	var once sync.Once
	var takedown error
	var written string
	p := e.processor(t, &hooked{Store: e.Store, onPut: func(key string, put func() error) error {
		if strings.HasPrefix(key, item.PrivatePrefix()) {
			once.Do(func() {
				written = key
				_, takedown = e.up.Commit(ctx, e.editor, ref, uuid.NewString(), []media.Op{{Op: media.OpRemove, Path: "files/a.png", Takedown: true}})
			})
		}
		return put()
	}})
	err := p.Process(ctx, media.ProcessJob{Ref: ref})
	if takedown != nil {
		t.Fatal(takedown)
	}
	if written == "" || written == shared || e.exists(t, shared) {
		t.Fatalf("the takedown did not isolate output allocations: old=%s new=%s (%v)", shared, written, err)
	}
	if err != nil {
		e.process(t, media.ProcessJob{Ref: ref}) // the job's retry; usually the next pass already rendered it again
	}
	m := e.manifest(t, ref)
	if out, ok := m.Get("web/b.webp"); !ok || out.Blob == "" {
		t.Fatalf("the other upload was not rendered: %+v", m.Files)
	}
	for _, b := range m.Blobs() {
		if key, _ := item.Blob(b); !e.exists(t, key) {
			t.Fatalf("the manifest references missing %s", key)
		}
	}
}

// An upload taken down while a pass re-renders it leaves nothing: the pass
// writes fresh outputs after the takedown's deletes, and its closing edit
// retires those too, so old URLs do not come back.
func TestTakedownMidPassLeavesNoOutput(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	ref := e.ref(t, "gallery", 1)
	item, _ := e.reg.Item(ref)
	e.put(t, ref, "originals/a.png", "image/png", solid(t, 80, 80, color.RGBA{30, 200, 30, 255}))
	e.process(t, media.ProcessJob{Ref: ref})
	highSum, _ := layout.BlobDigest(e.file(t, ref, "high/a.webp").Blob)
	highPrefix := item.PrivatePrefix() + layout.SHA256Name(highSum) + "-"
	var once sync.Once
	var takedown error
	fired := false
	p := e.processor(t, &hooked{Store: e.Store, onPut: func(key string, put func() error) error {
		if strings.HasPrefix(key, highPrefix) {
			once.Do(func() { // all source reads are done; the high rendition is about to land
				fired = true
				_, takedown = e.up.Commit(ctx, e.editor, ref, uuid.NewString(), []media.Op{{Op: media.OpRemove, Path: "originals/a.png", Takedown: true}})
			})
		}
		return put()
	}})
	if err := p.Process(ctx, media.ProcessJob{Ref: ref, Force: true}); err != nil {
		t.Fatal(err)
	}
	if takedown != nil || !fired {
		t.Fatalf("the takedown did not run mid-pass: fired %v, %v", fired, takedown)
	}
	for o, err := range e.Store.List(ctx, item.PrivatePrefix()) {
		if err != nil {
			t.Fatal(err)
		}
		t.Fatalf("%s kept after the takedown", o.Key)
	}
}

// Taking down an identical upload during rendering does not throw away the
// pass or force unrelated sources to be read again.
func TestDecoyTakedownCostsOneUpload(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	ref := e.ref(t, "post", 1)
	item, _ := e.reg.Item(ref)
	decoy := solid(t, 80, 80, color.RGBA{200, 30, 30, 255})
	e.put(t, ref, "files/p.png", "image/png", decoy)
	e.process(t, media.ProcessJob{Ref: ref})
	e.put(t, ref, "files/q.png", "image/png", decoy)
	others := []string{"files/o0.png", "files/o1.png", "files/o2.png", "files/o3.png"}
	for i, path := range others {
		e.put(t, ref, path, "image/png", solid(t, 80, 80, color.RGBA{uint8(40 * i), 90, 200, 255}))
	}
	source := func(path string) string {
		key, _ := item.Blob(e.file(t, ref, path).Blob)
		return key
	}
	twin := source("files/q.png")
	var mu sync.Mutex
	reads := map[string]int{}
	var once sync.Once
	var takedown error
	fired := false
	p := e.processor(t, &hooked{Store: e.Store, onGet: func(key string) {
		mu.Lock()
		reads[key]++
		mu.Unlock()
		if key == twin {
			once.Do(func() {
				fired = true
				_, takedown = e.up.Commit(ctx, e.editor, ref, uuid.NewString(), []media.Op{{Op: media.OpRemove, Path: "files/p.png", Takedown: true}})
			})
		}
	}})
	sources := map[string]string{}
	for _, path := range others {
		sources[path] = source(path)
	}
	if err := p.Process(ctx, media.ProcessJob{Ref: ref}); err != nil {
		t.Fatal(err)
	}
	if takedown != nil || !fired {
		t.Fatalf("the takedown did not run mid-pass: fired %v, %v", fired, takedown)
	}
	m := e.manifest(t, ref)
	for _, path := range append([]string{"files/q.png"}, others...) {
		if outs := m.Outputs(path, "web"); len(outs) != 1 {
			t.Fatalf("%s has %d outputs", path, len(outs))
		}
	}
	for _, b := range m.Blobs() {
		if key, _ := item.Blob(b); !e.exists(t, key) {
			t.Fatalf("the manifest references missing %s", key)
		}
	}
	for path, key := range sources {
		if reads[key] != 1 {
			t.Fatalf("%s was rendered %d times: the pass was thrown away", path, reads[key])
		}
	}
	if reads[twin] != 1 {
		t.Fatalf("the twin was rendered %d times, want once", reads[twin])
	}
}

// An upload taken down and put again at the same path while a pass renders
// it: what the pass writes for the old source is deleted in its closing
// edit, although the path still exists.
func TestTakedownAndPutMidPassLeavesNoOutput(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	ref := e.ref(t, "gallery", 1)
	item, _ := e.reg.Item(ref)
	e.put(t, ref, "originals/a.png", "image/png", solid(t, 80, 80, color.RGBA{30, 200, 30, 255}))
	e.put(t, ref, "originals/z.png", "image/png", solid(t, 80, 80, color.RGBA{30, 30, 200, 255}))
	e.process(t, media.ProcessJob{Ref: ref})
	other := e.file(t, ref, "originals/z.png").Blob
	var old []string
	for _, path := range []string{"originals/a.png", "thumb/a.webp", "high/a.webp"} {
		key, _ := item.Blob(e.file(t, ref, path).Blob)
		old = append(old, key)
	}
	highSum, _ := layout.BlobDigest(e.file(t, ref, "high/a.webp").Blob)
	highPrefix := item.PrivatePrefix() + layout.SHA256Name(highSum) + "-"
	var once sync.Once
	var replaced error
	fired := false
	p := e.processor(t, &hooked{Store: e.Store, onPut: func(key string, put func() error) error {
		if strings.HasPrefix(key, highPrefix) {
			once.Do(func() { // the old source's high rendition is encoded but not yet written
				fired = true
				_, replaced = e.up.Commit(ctx, e.editor, ref, uuid.NewString(), []media.Op{
					{Op: media.OpRemove, Path: "originals/a.png", Takedown: true},
					{Op: media.OpPut, Path: "originals/a.png", Blob: other},
				})
			})
		}
		return put()
	}})
	if err := p.Process(ctx, media.ProcessJob{Ref: ref, Force: true}); err != nil {
		t.Fatal(err)
	}
	if replaced != nil || !fired {
		t.Fatalf("the replacement did not run mid-pass: fired %v, %v", fired, replaced)
	}
	for _, key := range old {
		if e.exists(t, key) {
			t.Fatalf("%s kept after the takedown", key)
		}
	}
	m := e.manifest(t, ref)
	if a, z := m.Outputs("originals/a.png", "high"), m.Outputs("originals/z.png", "high"); len(a) != 1 || len(z) != 1 || a[0].Blob == z[0].Blob || !bytes.Equal(e.blob(t, ref, a[0]), e.blob(t, ref, z[0])) {
		t.Fatalf("the new source was not rendered: %+v %+v", a, z)
	}
	for _, b := range m.Blobs() {
		if key, _ := item.Blob(b); !e.exists(t, key) {
			t.Fatalf("the manifest references missing %s", key)
		}
	}
}
