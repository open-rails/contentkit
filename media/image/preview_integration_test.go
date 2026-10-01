package image_test

import (
	"context"
	"fmt"
	"image/color"
	"testing"

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
	// The third page moves first: both positions change.
	order("originals/c.png")
	if f := e.file(t, ref, "originals/c.png"); len(f.Pending) != 1 || f.Pending[0] != "preview" {
		t.Fatalf("the page that took a position is not pending: %+v", f)
	}
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
