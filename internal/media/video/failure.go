package video

import (
	"context"
	"errors"

	mediastore "github.com/open-rails/contentkit/internal/mediastore"
)

// FailPending records an exhausted derivative job's unfinished video-family
// work. Completed outputs and encoding checkpoints remain available for retry.
func (e *Encoder) FailPending(ctx context.Context, job mediastore.ProcessJob, cause error) error {
	if job.Editor {
		return nil
	}
	item, err := e.ms.Registry().Item(job.Ref)
	if err != nil {
		return err
	}
	_, err = e.ms.EditExisting(ctx, job.Ref, func(m *mediastore.Manifest) error {
		if m.Full {
			return nil
		}
		for _, f := range m.Files {
			if !f.IsUpload() || f.Gone || f.Fail() != nil {
				continue
			}
			pending := false
			if f.Blob != "" {
				for _, p := range item.Kind().PrivateFor(f.Path) {
					if (job.Preset == "" || job.Preset == p.Name) && videoFamily(p) && e.todo(m, f, p) {
						pending = true
						break
					}
				}
			}
			if job.Preset == "" && f.Frame != nil && f.Frame.From != nil {
				pending = pending || f.Blob == ""
			} else if job.Preset == "" && f.Frame != nil {
				v, ok := item.Kind().FramesVideo(m, f.Path)
				pending = pending || ok && v.Blob != "" && (f.Blob == "" || f.Frame.Of != v.Blob)
			}
			if pending {
				m.SetFailed(f.Path, cause)
			}
		}
		return nil
	})
	if errors.Is(err, mediastore.ErrNotFound) {
		return nil
	}
	return err
}
