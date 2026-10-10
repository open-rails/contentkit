package image_test

import (
	"context"
	"fmt"
	"image/color"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/contentkit/media"
)

// A preview is the public image of each of an Upload's first files, named
// by position: reordering or removing pages renders the positions again,
// names past the last page go at once, and a hidden item has none.
func TestPreviewPreset(t *testing.T) {
	e := newEnv(t, func(c *media.Config) {
		c.Kinds[0].Public = append(c.Kinds[0].Public, media.Public{Name: "preview", From: "originals/{name}",
			To: "preview-{n}.webp", First: 2, Image: media.Image{Width: 40}})
	})
	ctx := context.Background()
	ref := e.ref(t, "gallery", 1)
	for name, c := range map[string]color.RGBA{"a": red, "b": green, "c": blue} {
		e.put(t, ref, "originals/"+name+".png", "image/png", solid(t, 80, 80, c))
	}
	order := func(paths ...string) {
		t.Helper()
		for i, p := range paths {
			e.commit(t, ref, media.Op{Op: media.OpMove, Path: p, Index: &i})
		}
	}
	// previews checks the positions' colours; a nil colour is no file.
	previews := func(want ...*color.RGBA) {
		t.Helper()
		for i, c := range want {
			name := fmt.Sprintf("preview-%d.webp", i+1)
			b, obj, ok := e.public(t, ref, name)
			switch {
			case c == nil && ok:
				t.Fatalf("%s exists", name)
			case c != nil && !ok:
				t.Fatalf("%s is missing", name)
			case c != nil:
				pixels(t, b, 40, 40, map[[2]int]color.RGBA{{20, 20}: *c})
				if obj.ContentType != "image/webp" {
					t.Fatalf("%s is %s", name, obj.ContentType)
				}
			}
		}
	}
	order("originals/a.png", "originals/b.png", "originals/c.png")
	e.process(t, media.ProcessJob{Ref: ref})
	previews(&red, &green, nil)
	if m := e.manifest(t, ref); len(m.Files[0].Pending)+len(m.Files[1].Pending)+len(m.Files[2].Pending) != 0 {
		t.Fatalf("pending after the pass: %+v", m.Files[:3])
	}
	// The third page moves first: both positions change, and their old
	// images go with the commit, not when the worker gets to it.
	order("originals/c.png")
	if f := e.file(t, ref, "originals/c.png"); len(f.Pending) != 1 || f.Pending[0] != "preview" {
		t.Fatalf("the page that took a position is not pending: %+v", f)
	}
	previews(nil, nil, nil)
	e.process(t, media.ProcessJob{Ref: ref})
	previews(&blue, &red, nil)
	// A page is removed: the others move up; with one page left, the second
	// name goes with the commit.
	e.commit(t, ref, media.Op{Op: media.OpRemove, Path: "originals/c.png"})
	e.process(t, media.ProcessJob{Ref: ref})
	previews(&red, &green, nil)
	e.commit(t, ref, media.Op{Op: media.OpRemove, Path: "originals/a.png"})
	previews(nil, nil) // position 1 is green now: the old image is not served meanwhile
	e.process(t, media.ProcessJob{Ref: ref})
	previews(&green, nil)
	// Hidden: none.
	if _, err := e.ms.EditExisting(ctx, ref, func(m *media.Manifest) error {
		m.Hidden = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	e.process(t, media.ProcessJob{Ref: ref})
	previews(nil, nil)
}

// A reorder landing between a pass publishing a preview and its closing
// edit: the upload keeps the preview pending, so it is rendered at its new
// position before the manifest vouches for that name.
func TestPreviewMovedMidPass(t *testing.T) {
	e := newEnv(t, func(c *media.Config) {
		c.Kinds[0].Public = append(c.Kinds[0].Public, media.Public{Name: "preview", From: "originals/{name}",
			To: "preview-{n}.webp", First: 2, Image: media.Image{Width: 40}})
	})
	ctx := context.Background()
	ref := e.ref(t, "gallery", 1)
	item, _ := e.reg.Item(ref)
	e.put(t, ref, "originals/x.png", "image/png", solid(t, 80, 80, red))
	e.put(t, ref, "originals/y.png", "image/png", solid(t, 80, 80, green))
	e.process(t, media.ProcessJob{Ref: ref})
	// A new page takes position 1; the pass renders it there.
	first := 0
	e.put(t, ref, "originals/n.png", "image/png", solid(t, 80, 80, blue), media.Op{Op: media.OpMove, Path: "originals/n.png", Index: &first})
	second := item.PublicPrefix() + "preview-2-"
	var mu sync.Mutex
	var events []string // what the processor does to preview-2 once the page has moved there
	var once sync.Once
	var moved error
	fired := false
	record := func(what, key string) {
		mu.Lock()
		defer mu.Unlock()
		if fired && strings.HasPrefix(key, second) {
			events = append(events, what)
		}
	}
	p := e.processor(t, headHook{
		Store: &hooked{Store: e.Store, onPut: func(key string, put func() error) error {
			record("put", key)
			if strings.HasPrefix(key, item.PrivatePrefix()) {
				once.Do(func() { // the new page's preview is published; its private outputs come next
					_, moved = e.up.Commit(ctx, e.editor, ref, uuid.NewString(), []media.Op{{Op: media.OpMove, Path: "originals/y.png", Index: &first}})
					mu.Lock()
					fired = true
					mu.Unlock()
				})
			}
			return put()
		}},
		before: func(key string) { record("head", key) },
	})
	if err := p.Process(ctx, media.ProcessJob{Ref: ref}); err != nil {
		t.Fatal(err)
	}
	if moved != nil || !fired {
		t.Fatalf("the move did not run mid-pass: fired %v, %v", fired, moved)
	}
	if len(events) == 0 || events[0] != "put" {
		t.Fatalf("the moved page was vouched for at its new position before being rendered there: %v", events)
	}
	for name, c := range map[string]color.RGBA{"preview-1.webp": green, "preview-2.webp": blue} {
		b, _, ok := e.public(t, ref, name)
		if !ok {
			t.Fatalf("%s is missing", name)
		}
		pixels(t, b, 40, 40, map[[2]int]color.RGBA{{20, 20}: c})
	}
}
