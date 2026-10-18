package versions

import (
	"context"
	"errors"
	"math"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/media"
)

// facts are what a version's media says about it (content_versions):
// pages for a publishing kind; duration and size of its longest video, else
// its first page's size; and whether it has audio.
type facts struct {
	Pages    *int
	Duration *int64 // ms
	Width    *int
	Height   *int
	HasAudio bool
}

func factsOf(k *media.Kind, m *media.Manifest) facts {
	var f facts
	var pages int
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
func (s *Service) lockTarget(ctx context.Context, tx pgx.Tx, v Target) error {
	if _, err := tx.Exec(ctx, `SELECT FROM `+s.t.items+` WHERE tenant_id = $1 AND id = $2 FOR UPDATE`, s.o.Tenant, v.Content); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `SELECT FROM `+s.t.versions+` WHERE tenant_id = $1 AND id = $2 FOR UPDATE`, s.o.Tenant, v.Version)
	return err
}

// recordFacts writes v's facts and refreshes its facets when they changed.
func (s *Service) recordFacts(ctx context.Context, tx pgx.Tx, k *media.Kind, v Target, m *media.Manifest) error {
	// Catalog edits lock the parent before the version and derive access in
	// a later statement. Hold that same parent lock through the facet callback
	// so media facts cannot restore an access snapshot read before an edit.
	var id string
	err := tx.QueryRow(ctx, `SELECT id FROM `+s.t.items+` WHERE tenant_id = $1 AND id = $2 FOR UPDATE`, s.o.Tenant, v.Content).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	} else if err != nil {
		return err
	}
	f := factsOf(k, m)
	err = tx.QueryRow(ctx, `UPDATE `+s.t.versions+` SET pages = $4, duration_ms = $5, width = $6, height = $7, has_audio = $8, updated_at = now()
WHERE tenant_id = $1 AND id = $2 AND content_id = $3
AND (pages, duration_ms, width, height, has_audio) IS DISTINCT FROM ($4, $5, $6, $7, $8) RETURNING id`,
		s.o.Tenant, v.Version, v.Content, f.Pages, f.Duration, f.Width, f.Height, f.HasAudio).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	} else if err != nil {
		return err
	}
	return s.o.Facets.RefreshTx(ctx, tx, []string{id})
}
