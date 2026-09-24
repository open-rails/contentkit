package video

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/open-rails/contentkit/media"
)

// Frames implements media.FrameGrabber for the poster picker: it reads the
// init segment and the one segment holding t from the narrowest HLS rendition
// at least the requested width, and decodes that frame. The host needs ffmpeg.
type Frames struct {
	store   media.Store
	tempDir string
}

func NewFrames(store media.Store, tempDir string) (*Frames, error) {
	if store == nil {
		return nil, errors.New("media/video: Frames needs a Store")
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return nil, fmt.Errorf("media/video: %w", err)
	}
	if tempDir == "" {
		tempDir = os.TempDir()
	}
	return &Frames{store: store, tempDir: tempDir}, nil
}

var _ media.FrameGrabber = (*Frames)(nil)

func (fr *Frames) Frame(ctx context.Context, item media.Item, f media.File, t float64, width int) ([]byte, error) {
	if f.HLS == nil || len(f.HLS.Video) == 0 {
		return nil, errors.New("media/video: file has no HLS video")
	}
	r := narrowest(f, width)
	width = min(width, r.Width) &^ 1 // never above the widest rendition
	dir, err := os.MkdirTemp(fr.tempDir, "ck-frame-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "frame.mp4")
	start, err := snippet(ctx, fr.store, item, r, t, t, path)
	if err != nil {
		return nil, err
	}
	return frameJPEG(ctx, path, t-start, width)
}
