package media_test

import (
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/internal/s3test"
)

// A Stable preset keeps its current image at the logical names too, the
// URL built from the item's id: the first image lands there, a
// replacement takes its place once rendered , and a removal or the
// item's erasure leaves nothing, where the gateway serves the default.
func TestStablePresetKeepsTheCurrentImageAtItsLogicalNames(t *testing.T) {
	f := newFixtureOn(t, s3test.Open(t), func(c *media.Config) {
		for i := range c.Kinds {
			if c.Kinds[i].Name == "user" {
				c.Kinds[i].Public[0].Stable = true
			}
		}
	})
	ref := f.ref("user", 1)
	f.visible(1)
	stable := f.reg.PublicURL(ref, "avatar-64.webp")
	shows := func(want string) {
		t.Helper()
		code, body, _ := f.fetch(stable)
		switch {
		case want == "" && code != http.StatusNotFound:
			t.Fatalf("stable name: %d, want none", code)
		case want != "" && (code != http.StatusOK || body != want):
			t.Fatalf("stable name: %d %q, want %q", code, body, want)
		}
	}
	shows("")

	f.put(ref, "avatar", "image/png", png(1))
	shows("")
	f.produce(ref)
	shows(string(png(1)))
	if _, gen, _ := f.fetch(f.reg.PublicURL(ref, f.publicName(ref, "avatar-64.webp"))); gen != string(png(1)) {
		t.Fatalf("the current generation %q", gen)
	}

	f.put(ref, "avatar", "image/png", png(2))
	f.produce(ref)
	shows(string(png(2)))

	cur, _, err := f.ms.Get(t.Context(), ref)
	if err != nil || len(cur.Files) != 1 {
		t.Fatalf("one upload: %v", err)
	}
	if m := f.commit(ref, media.Op{Op: media.OpRemove, Path: cur.Files[0].Path}); len(m.Files) != 0 {
		t.Fatalf("removal left %+v", m.Files)
	}
	shows("")

	f.put(ref, "avatar", "image/png", png(3))
	f.produce(ref)
	shows(string(png(3)))
	if err := f.jobs.Purge(t.Context(), media.Deletion{Ref: ref, OperationID: uuid.NewString()}); err != nil {
		t.Fatal(err)
	}
	shows("")
}
