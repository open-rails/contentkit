package versionstore

import (
	"context"
	"math"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	mediastore "github.com/open-rails/contentkit/internal/mediastore"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/layout"
	versions "github.com/open-rails/contentkit/media/versions"
)

// facts are what a version's media says about it (content_versions):
// pages for a publishing kind; duration and size of its longest video, else
// its first page's size, and whether it has audio. Publication readiness is
// determined by the manifest, not a separate cached boolean.
type facts struct {
	Pages    *int
	Duration *int64 // ms
	Width    *int
	Height   *int
	HasAudio bool
}

func factsOf(k *mediastore.RegisteredKind, m *mediastore.Manifest) facts {
	var f facts
	var pages int
	// Once anything is released, readers count what they are served.
	served := k.Publish != nil && slices.ContainsFunc(m.Files, func(o media.File) bool {
		return !o.IsUpload() && slices.Contains(k.Publish.Renditions, o.Preset) && layout.Served(layout.BlobArea(o.Blob))
	})
	var first *media.File
	var video *media.File
	for i := range m.Files {
		file := &m.Files[i]
		if file.Track != nil && file.Track.Kind == media.TrackAudio {
			f.HasAudio = true
		}
		if !file.IsUpload() || file.Unattached || file.Fail() != nil {
			continue
		}
		switch {
		case k.Publish != nil && inPattern(k.Publish.Pages, file.Path) && media.PageIncluded(*file):
			if served && !pageServed(k, m, file.Path) {
				continue
			}
			pages++
			if first == nil {
				first = file
			}
		case strings.HasPrefix(file.Type, "video/"):
			if video == nil || file.Dur > video.Dur {
				video = file
			}
		case strings.HasPrefix(file.Type, "audio/"):
			f.HasAudio = true
		}
	}
	if k.Publish != nil {
		f.Pages = &pages
	}

	size := first
	if video != nil {
		size = video
		if video.Dur > 0 {
			d := int64(math.Round(video.Dur * 1000))
			f.Duration = &d
		}
	}
	if size != nil && size.W > 0 && size.H > 0 {
		f.Width, f.Height = &size.W, &size.H
	}
	return f
}

// inPattern reports a path of a "{dir}{name}" upload pattern.
func inPattern(pattern, path string) bool {
	dir, ok := strings.CutSuffix(pattern, "{name}")
	return ok && strings.HasPrefix(path, dir) && len(path) > len(dir) && !strings.Contains(path[len(dir):], "/")
}

// lockTarget locks v's content, then v, the order catalog writes take them.
// Holding v makes callbacks that read its manifest afterwards record their
// snapshots in commit order. Its callers start tx for it, so it may wait.
func (s *Service) lockTarget(ctx context.Context, tx pgx.Tx, v versions.Target) error {
	if _, err := tx.Exec(ctx, `SELECT FROM `+s.t.items+` WHERE tenant_id = $1 AND id = $2 FOR UPDATE`, s.o.Tenant, v.Content); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `SELECT FROM `+s.t.versions+` WHERE tenant_id = $1 AND id = $2 FOR UPDATE`, s.o.Tenant, v.Version)
	return err
}

// pageServed reports a page whose reader renditions are all in a served
// area.
func pageServed(k *mediastore.RegisteredKind, m *mediastore.Manifest, path string) bool {
	for _, name := range k.Publish.Renditions {
		outs := m.Outputs(path, name)
		if len(outs) != 1 || !layout.Served(layout.BlobArea(outs[0].Blob)) {
			return false
		}
	}
	return true
}

// recordFacts writes v's changed media facts from its manifest.
func (s *Service) recordFacts(ctx context.Context, tx pgx.Tx, k *mediastore.RegisteredKind, v versions.Target, m *mediastore.Manifest) error {
	f := factsOf(k, m)
	_, err := tx.Exec(ctx, `UPDATE `+s.t.versions+` SET pages = $4, duration_ms = $5, width = $6, height = $7, has_audio = $8, updated_at = now()
WHERE tenant_id = $1 AND id = $2 AND content_id = $3
AND (pages, duration_ms, width, height, has_audio) IS DISTINCT FROM ($4, $5, $6, $7, $8)`,
		s.o.Tenant, v.Version, v.Content, f.Pages, f.Duration, f.Width, f.Height, f.HasAudio)
	return err
}
