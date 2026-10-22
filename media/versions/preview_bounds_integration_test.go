package versions_test

import (
	"fmt"
	"testing"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/versions"
)

func TestDirectReaderMatchesPreviewBounds(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		t.Run(fmt.Sprint(enabled), func(t *testing.T) {
			s := newSite(t, siteOptions{mediaGallery: func(k *media.Kind) {
				if !enabled {
					k.Publish.Previews = ""
					return
				}
				for i := range k.Public {
					if k.Public[i].Name == k.Publish.Previews {
						k.Public[i].First = 1
					}
				}
			}})
			v := s.version(s.content("gallery", `{"level":"tier","tiers":["premium"],"preview":{"pages":3}}`))
			ref := s.ref("gallery", v)
			for i := 1; i <= 3; i++ {
				s.mustUpload(s.creator, ref, fmt.Sprintf("originals/%03d", i), "image/png", pngOf(t, 30, 40, uint8(i)))
			}
			anon := access.Actor{Anonymous: true, IP: "203.0.113.10"}
			plan := s.plan(anon, v, func(p *versions.ReaderPlan) bool { return p.Pages != nil && *p.Pages == 3 })
			want := 0
			if enabled {
				want = 1
			}
			if len(plan.Previews) != want {
				t.Fatalf("plan previews=%d want%d", len(plan.Previews), want)
			}
			read, err := s.reader.Read(s.ctx, ref, anon, media.ReadOptions{Paths: []string{"low-res/003.webp"}})
			if err != nil {
				t.Fatal(err)
			}
			if read.Total != want {
				t.Fatalf("direct reader broadened preview cap: total=%d want%d", read.Total, want)
			}
			for _, f := range read.Files {
				if f.URL != "" {
					t.Fatalf("out-of-bounds preview was signed: %+v", f)
				}
			}
		})
	}
}
