// Package video runs an item's video-family presets with ffmpeg: HLS (a
// byte-range fMP4 ladder with the source's audio and text tracks and a seek
// sprite), MP4 (a muxed H.264 file at one rung), Audio (an HLS track and an
// M4A) and Subtitles (clean WebVTT), and grabs the frames of uploads that
// declare Upload.Frames. Each output is recorded in the manifest with its
// provenance; a job redoes exactly the outputs whose fingerprint no longer
// matches (or that are pending). Jobs run in the media worker on the host's
// worker River schema (Contribution); Frames serves the editor's frame picker.
package video

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"time"

	"github.com/open-rails/contentkit/media"
)

// Config configures an Encoder.
type Config struct {
	// Manifests is the worker's: the host's registry, its manifest lock
	// (media.PGLocker on the host database) and folder sweeps.
	Manifests *media.Manifests
	// Queue hands a grabbed frame to the image job (workqueue.Queue);
	// without it, frames are grabbed but their presets not rendered.
	Queue   media.ProcessQueue
	TempDir string // scratch; default os.TempDir()
	Threads int    // CPU threads for ffmpeg; default GOMAXPROCS (the container's CPU limit)
	// Codecs are the HLS ladder's codecs, in the order players are offered
	// them; every rung is encoded in each. Default DefaultCodecs. MP4
	// presets are always H.264.
	Codecs []media.Codec
	// Preset is the libx264/libx265 preset of rungs up to 1080, TopPreset of
	// the rungs above; both default "fast".
	Preset, TopPreset string
	// Encoder is EncoderAuto (default), EncoderCPU or EncoderNVENC. Each
	// codec's encoder is checked by a probe encode in New; a pass NVENC
	// fails is re-encoded on the CPU.
	Encoder string
	Logger  *slog.Logger
	// ProgressInterval throttles progress reports; default 2 s.
	ProgressInterval time.Duration
	// ObserveEncode receives one measurement per ffmpeg video pass.
	ObserveEncode func(EncodeObservation)
}

// EncodeObservation measures one ffmpeg video pass. OutputSeconds counts
// successful rendition seconds, so CPU cost per output second can be derived
// from the exported counters without averaging ratios.
type EncodeObservation struct {
	SourceClass   string
	Duration      time.Duration
	CPU           time.Duration
	OutputSeconds float64
	Succeeded     bool
}

// Encoder holds the worker's ffmpeg setup. Outputs are byte-identical on
// retry, so blob names repeat and re-uploads are skipped.
type Encoder struct {
	c        Config
	ms       *media.Manifests
	store    media.Store
	encoders map[media.Codec]string // ffmpeg encoder per codec, H.264 always included
}

func New(ctx context.Context, c Config) (*Encoder, error) {
	if c.Manifests == nil {
		return nil, errors.New("media/video: Config.Manifests is required")
	}
	for _, tool := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(tool); err != nil {
			return nil, fmt.Errorf("media/video: %w", err)
		}
	}
	c.TempDir = cmp.Or(c.TempDir, os.TempDir())
	if c.Threads <= 0 {
		c.Threads = runtime.GOMAXPROCS(0)
	}
	if len(c.Codecs) == 0 {
		c.Codecs = DefaultCodecs
	}
	for i, codec := range c.Codecs {
		if slices.Contains(c.Codecs[:i], codec) {
			return nil, fmt.Errorf("media/video: codec %q listed twice", codec)
		}
	}
	c.Encoder = cmp.Or(c.Encoder, EncoderAuto)
	codecs := c.Codecs
	if !slices.Contains(codecs, media.CodecH264) {
		codecs = append(slices.Clone(codecs), media.CodecH264)
	}
	encoders, err := resolveEncoders(ctx, c.TempDir, c.Encoder, codecs)
	if err != nil {
		return nil, err
	}
	c.Preset = cmp.Or(c.Preset, "fast")
	c.TopPreset = cmp.Or(c.TopPreset, "fast")
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.ProgressInterval <= 0 {
		c.ProgressInterval = 2 * time.Second
	}
	return &Encoder{c: c, ms: c.Manifests, store: c.Manifests.Store(), encoders: encoders}, nil
}

// PermanentError marks a source that can never be processed (no video
// stream, unreadable container, aspect out of range); it is recorded as the
// upload's failure and reported to Hooks.Failed.
type PermanentError struct{ Err error }

func (e *PermanentError) Error() string { return "media/video: " + e.Err.Error() }
func (e *PermanentError) Unwrap() error { return e.Err }

// errStale reports that an upload changed while its outputs were made; the
// commit that changed it enqueued its own job.
var errStale = errors.New("media/video: upload changed during processing")

// settle records a permanent error or a checksum mismatch as upload f's
// failure, and drops errStale (the next job redoes the work).
func (e *Encoder) settle(ctx context.Context, item media.Item, f media.File, err error) error {
	var perm *PermanentError
	switch {
	case errors.Is(err, errStale):
		return nil
	case errors.Is(err, errChecksum):
		return e.checksumFailed(ctx, item, f.Path, f.Blob)
	case errors.As(err, &perm):
		return e.fail(ctx, item, f.Path, f.Blob, perm.Err)
	case err != nil:
		return fmt.Errorf("media/video: %s %q: %w", item.Ref(), f.Path, err)
	}
	return nil
}

// fail records that the upload at path cannot be processed from blob, and
// tells Hooks.Failed.
func (e *Encoder) fail(ctx context.Context, item media.Item, path, blob string, cause error) error {
	e.c.Logger.WarnContext(ctx, "media/video: cannot process", "ref", item.Ref().String(), "path", path, "error", cause)
	return e.failed(ctx, item, path, blob, cause, func(m *media.Manifest) { m.SetFailed(path, cause) })
}

// failed applies record to the upload at path while it still holds blob,
// then tells Hooks.Failed.
func (e *Encoder) failed(ctx context.Context, item media.Item, path, blob string, cause error, record func(*media.Manifest)) error {
	_, err := e.ms.EditExisting(ctx, item.Ref(), func(m *media.Manifest) error {
		if f, ok := m.Get(path); !ok || f.Blob != blob {
			return errStale
		}
		record(m)
		return nil
	})
	if errors.Is(err, errStale) || errors.Is(err, media.ErrNotFound) {
		return nil
	} else if err != nil {
		return err
	}
	if h := e.ms.Registry().Config().Hooks.Failed; h != nil {
		h(ctx, item.Ref(), path, cause)
	}
	return nil
}

// tempPattern names scratch directories; SweepTemp removes leftovers.
const tempPattern = "ck-video-*"

// SweepTemp removes scratch left in dir by a killed process. Run it at
// worker start, before any job.
func SweepTemp(dir string) error {
	matches, err := filepath.Glob(filepath.Join(cmp.Or(dir, os.TempDir()), tempPattern))
	if err != nil {
		return err
	}
	var errs []error
	for _, m := range matches {
		errs = append(errs, os.RemoveAll(m))
	}
	return errors.Join(errs...)
}

// testBeforePublish runs between the output uploads and the manifest edit.
var testBeforePublish func()
