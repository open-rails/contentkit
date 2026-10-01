package video

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"time"

	"github.com/open-rails/contentkit/media"
)

// Frames implements media.FrameGrabber for the editor's frame picker: it
// seeks the video upload's source by range and decodes the frame at t. The
// host needs ffmpeg.
type Frames struct{ store media.Store }

func NewFrames(store media.Store) (*Frames, error) {
	if store == nil {
		return nil, errors.New("media/video: Frames needs a Store")
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return nil, fmt.Errorf("media/video: %w", err)
	}
	return &Frames{store: store}, nil
}

var _ media.FrameGrabber = (*Frames)(nil)

func (fr *Frames) Frame(ctx context.Context, item media.Item, video media.File, t float64, width int) ([]byte, error) {
	key, err := item.Blob(video.Blob)
	if err != nil || video.Gone {
		return nil, errors.New("media/video: the video's source is gone")
	}
	req, err := fr.store.PresignGet(ctx, key, time.Minute)
	if err != nil {
		return nil, err
	}
	if video.W > 0 {
		width = min(width, video.W) // never above the video
	}
	return frameJPEG(ctx, frameInput{path: req.URL, opts: remoteInputOptions(sourceDemuxers), offset: t}, width&^1)
}
