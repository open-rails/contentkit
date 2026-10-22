package media_test

import (
	"testing"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/internal/resolve"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/internal/s3test"
)

func TestReaderHidesLegacyCopiedPreviewsButKeepsCover(t *testing.T) {
	f := newFixtureOn(t, s3test.Open(t), func(c *media.Config) { k := bookKind(); k.Versions = true; c.Kinds = append(c.Kinds, k) })
	ref, err := f.reg.VersionRef("book", cid(1), cid(2))
	if err != nil {
		t.Fatal(err)
	}
	f.visible(1)
	f.put(ref, "cover.png", "image/png", png(1))
	f.put(ref, "previews/001.webp", "image/webp", png(2))
	f.produce(ref)
	f.res.set(cid(1), resolve.Resolution{Visible: true, Accessible: true, MediaFiles: map[string]bool{}})
	read, err := f.rd.Read(t.Context(), ref, access.Actor{ID: "viewer", Kind: "user"}, media.ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(read.Previews) != 0 {
		t.Fatalf("legacy preview URLs escaped grant: %v", read.Previews)
	}
	cover := false
	for _, image := range read.Public {
		if image.Preset == "preview" {
			t.Fatal("legacy preview metadata escaped grant")
		}
		cover = cover || image.Preset == "cover"
	}
	if !cover {
		t.Fatal("public cover teaser disappeared")
	}
}
