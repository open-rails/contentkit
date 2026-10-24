package versions_test

import (
	"testing"

	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/versions"
)

func TestOrdinaryArchiveCoverRejectionKeepsFallbackBehavior(t *testing.T) {
	s := newSite(t, siteOptions{mediaGallery: func(k *media.Kind) {
		for i := range k.Uploads {
			if k.Uploads[i].Path == "cover" {
				k.Uploads[i].MaxBytes = 1
			}
		}
	}})
	v := s.version(s.content("gallery", `{"level":"public"}`))
	ref := s.ref("gallery", v)
	s.mustUpload(s.creator, ref, "originals/page", "image/png", pngOf(t, 128, 192, 8))
	s.plan(s.creator, v, func(p *versions.ReaderPlan) bool { return p.Pages != nil && *p.Pages == 1 })
	if err := versions.Publish(s.svc, s.ctx, v); err != nil {
		t.Fatal(err)
	}
}
