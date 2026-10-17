package versions_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/versions"
)

// A version is ready to go live once its files are produced and its current
// publication shows them: a page still producing or failed keeps it pending,
// and a version without uploads has nothing to wait for.
func TestPendingUntilPublished(t *testing.T) {
	s := newSite(t, siteOptions{})
	v := s.version(s.content("gallery", `{"level":"public"}`))
	ref := s.ref("gallery", v)
	pending := func() []versions.MediaPending {
		t.Helper()
		tx, err := s.pool.Begin(s.ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(s.ctx)
		out, err := s.svc.PendingTx(s.ctx, tx, []versions.Target{v})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	edit := func(fn func(f *media.File)) {
		t.Helper()
		if _, err := s.ms.EditExisting(s.ctx, ref, func(m *media.Manifest) error {
			for i := range m.Files {
				if m.Files[i].IsUpload() && strings.HasPrefix(m.Files[i].Path, "originals/002") {
					fn(&m.Files[i])
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}

	if p := pending(); len(p) != 0 {
		t.Fatalf("a version without uploads is pending: %+v", p)
	}
	for i := range 2 {
		s.mustUpload(s.creator, ref, fmt.Sprintf("originals/%03d", i+1), "image/png", pngOf(t, 30, 40, uint8(i+1)))
	}
	s.plan(s.fan, v, func(p *versions.ReaderPlan) bool { return p.Pages != nil && *p.Pages == 2 })
	if p := pending(); len(p) != 0 {
		t.Fatalf("a published version is pending: %+v", p)
	}

	edit(func(f *media.File) { f.Pending = append(f.Pending, "low") })
	if p := pending(); len(p) != 1 || p[0].Target != v || p[0].Reason != "pages are still processing" {
		t.Fatalf("a producing page: %+v", p)
	}
	edit(func(f *media.File) { f.Pending = nil })
	if p := pending(); len(p) != 0 {
		t.Fatalf("produced again, its publication current: %+v", p)
	}

	edit(func(f *media.File) { f.Failed = &media.Failure{Of: f.Key(), Message: "unreadable"} })
	if p := pending(); len(p) != 1 || !strings.HasPrefix(p[0].Reason, "a file failed to process: originals/002") {
		t.Fatalf("a failed page: %+v", p)
	}
	edit(func(f *media.File) { f.Failed = nil })

	// Audio is waited for once the version has some; image-only versions never are.
	s.mustUpload(s.creator, ref, "audio/theme", "audio/wav", wav(t))
	s.plan(s.fan, v, func(p *versions.ReaderPlan) bool { return p.Playback != nil && len(p.Playback.Tracks) == 1 })
	if p := pending(); len(p) != 0 {
		t.Fatalf("published audio is pending: %+v", p)
	}
	if _, err := s.ms.EditExisting(s.ctx, ref, func(m *media.Manifest) error {
		for i := range m.Files {
			if m.Files[i].IsUpload() && strings.HasPrefix(m.Files[i].Path, "audio/theme") {
				m.Files[i].Pending = append(m.Files[i].Pending, "listen")
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if p := pending(); len(p) != 1 || p[0].Reason != "playback media is still processing" {
		t.Fatalf("producing audio: %+v", p)
	}
}
