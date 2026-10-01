package image_test

import (
	"context"
	"errors"
	"image/color"
	"sync"
	"testing"

	"github.com/open-rails/contentkit/media"
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

// Two uploads of the same bytes share their output blob. One is taken down
// while a pass renders the other, after the pass saw the blob in place: the
// pass must not record a blob that is gone.
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
	p := e.processor(t, headHook{Store: e.Store, after: func(key string) {
		if key == shared {
			once.Do(func() {
				_, takedown = e.up.Commit(ctx, e.editor, ref, []media.Op{{Op: media.OpRemove, Path: "files/a.png", Takedown: true}})
			})
		}
	}})
	if err := p.Process(ctx, media.ProcessJob{Ref: ref}); err == nil && e.exists(t, shared) {
		t.Fatal("the takedown did not run mid-pass")
	}
	if takedown != nil {
		t.Fatal(takedown)
	}
	e.process(t, media.ProcessJob{Ref: ref}) // the retry
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
// writes its output again after the takedown's deletes (the same bytes, the
// same name), and its closing edit deletes it, so old URLs do not come back.
func TestTakedownMidPassLeavesNoOutput(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	ref := e.ref(t, "post", 1)
	item, _ := e.reg.Item(ref)
	e.put(t, ref, "files/a.png", "image/png", solid(t, 80, 80, color.RGBA{30, 200, 30, 255}))
	e.process(t, media.ProcessJob{Ref: ref})
	source, _ := item.Blob(e.file(t, ref, "files/a.png").Blob)
	output, _ := item.Blob(e.file(t, ref, "web/a.webp").Blob)
	var once sync.Once
	var takedown error
	p := e.processor(t, headHook{Store: e.Store, before: func(key string) {
		if key == output {
			once.Do(func() {
				_, takedown = e.up.Commit(ctx, e.editor, ref, []media.Op{{Op: media.OpRemove, Path: "files/a.png", Takedown: true}})
			})
		}
	}})
	if err := p.Process(ctx, media.ProcessJob{Ref: ref, Force: true}); err != nil {
		t.Fatal(err)
	}
	if takedown != nil {
		t.Fatal(takedown)
	}
	if e.exists(t, source) || e.exists(t, output) {
		t.Fatalf("after the takedown: source kept %v, re-rendered output kept %v", e.exists(t, source), e.exists(t, output))
	}
}
