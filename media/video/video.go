// Package video encodes an item's video files with ffmpeg into a byte-range
// HLS ladder (one single-file fMP4 blob per rendition and audio track),
// WebVTT subtitles, a thumbnail sprite and one muxed MP4 download per
// quality, and records them in the manifest's hls and downloads. Jobs run in
// cmd/media-worker on River schema Schema.
package video

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media"
)

// Spec identifies Recipe in manifest hls and downloads entries.
var Spec = func() string { s := sha256.Sum256([]byte(Recipe)); return hex.EncodeToString(s[:4]) }()

// Config configures an Encoder.
type Config struct {
	Store media.Store
	// Locker serializes manifest edits when Store lacks conditional PUT; it
	// must share the hosts' lock space (media.PGLocker on the host database).
	Locker  media.Locker
	TempDir string // scratch for the source and outputs; default os.TempDir()
	Threads int    // ffmpeg threads; default GOMAXPROCS (the container's CPU limit)
	Logger  *slog.Logger
}

// Encoder runs encode jobs. It is idempotent: a file whose hls and downloads
// match its source and Spec is skipped, and outputs are byte-identical on
// retry, so blob names repeat.
type Encoder struct {
	c Config
}

// Job encodes the video files of one manifest. Versioned is the kind's flag,
// needed to address the manifest.
type Job struct {
	Ref       contentref.ContentRef `json:"ref"`
	Versioned bool                  `json:"versioned,omitempty"`
}

func New(c Config) (*Encoder, error) {
	if c.Store == nil {
		return nil, errors.New("media/video: Encoder needs a Store")
	}
	if !c.Store.Capabilities().ConditionalPut && c.Locker == nil {
		return nil, errors.New("media/video: store lacks conditional PUT; Config.Locker is required")
	}
	for _, tool := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(tool); err != nil {
			return nil, fmt.Errorf("media/video: %w", err)
		}
	}
	if c.TempDir == "" {
		c.TempDir = os.TempDir()
	}
	if c.Threads <= 0 {
		c.Threads = runtime.GOMAXPROCS(0)
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	return &Encoder{c: c}, nil
}

// PermanentError marks a source that can never be encoded (no video stream,
// unreadable container); retrying it is pointless.
type PermanentError struct{ Err error }

func (e *PermanentError) Error() string { return "media/video: " + e.Err.Error() }
func (e *PermanentError) Unwrap() error { return e.Err }

// IsVideo reports whether a manifest file is encoded by this package.
func IsVideo(f media.File) bool { return strings.HasPrefix(f.Type, "video/") }

// DownloadKey is the manifest downloads key of a file's quality, e.g. "source-1080p".
func DownloadKey(file string, height int) string { return file + "-" + strconv.Itoa(height) + "p" }

var downloadKey = regexp.MustCompile(`^(.+)-(\d+)p$`)

// Encode brings every video file of the job's manifest up to date, promoting
// each file's outputs in its own manifest edit.
func (e *Encoder) Encode(ctx context.Context, job Job) error {
	kinds, err := media.NewRegistry(media.Kind{Name: job.Ref.ContentKind, Versioned: job.Versioned, Video: true})
	if err != nil {
		return &PermanentError{err}
	}
	item, err := kinds.Item(job.Ref)
	if err != nil {
		return &PermanentError{err}
	}
	if _, err := item.ManifestKey(); err != nil {
		return &PermanentError{err}
	}
	ms, err := media.NewManifests(e.c.Store, kinds, media.ManifestOptions{Locker: e.c.Locker, CacheSize: 1})
	if err != nil {
		return err
	}
	man, _, err := ms.Get(ctx, job.Ref)
	if errors.Is(err, media.ErrNotFound) {
		return nil
	} else if err != nil {
		return err
	}
	var errs []error
	permanent := true
	for _, f := range man.Files {
		if !IsVideo(f) || fresh(man, f) {
			continue
		}
		if err := e.file(ctx, ms, item, f.Name, f.Source()); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			var perm *PermanentError
			permanent = permanent && errors.As(err, &perm)
			errs = append(errs, fmt.Errorf("%s %q: %w", job.Ref, f.Name, err))
		}
	}
	if len(errs) == 0 {
		// Drop downloads of files that are gone or no longer video.
		_, err := ms.Edit(ctx, job.Ref, func(m *media.Manifest) error {
			for k, d := range m.Downloads {
				if name, ok := videoDownload(k, d); ok {
					if i := m.File(name); i < 0 || !IsVideo(m.Files[i]) {
						delete(m.Downloads, k)
					}
				}
			}
			return nil
		})
		return err
	}
	if permanent {
		return &PermanentError{errors.Join(errs...)}
	}
	return errors.Join(errs...)
}

func fresh(m *media.Manifest, f media.File) bool {
	h := f.HLS
	if h == nil || h.Source != f.Source() || h.Spec != Spec || len(h.Video) == 0 {
		return false
	}
	for _, r := range h.Video {
		if d, ok := m.Downloads[DownloadKey(f.Name, r.Height)]; !ok || d.Spec != Spec || d.Inputs != h.Source {
			return false
		}
	}
	return true
}

func videoDownload(key string, d media.Download) (file string, ok bool) {
	m := downloadKey.FindStringSubmatch(key)
	if m == nil || d.Type != "video/mp4" {
		return "", false
	}
	return m[1], true
}

// errStale reports that the file's source changed while it was encoded; the
// commit that changed it enqueued its own job.
var errStale = errors.New("source changed during encode")

// testBeforePromote runs between the blob uploads and the manifest edit.
var testBeforePromote func()

func (e *Encoder) file(ctx context.Context, ms *media.Manifests, item media.Item, name, source string) error {
	srcKey, err := item.Original(source)
	if err != nil {
		return &PermanentError{err}
	}
	dir, err := os.MkdirTemp(e.c.TempDir, tempPattern)
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)

	src := filepath.Join(dir, "source")
	srcObj, err := e.fetch(ctx, srcKey, src)
	if errors.Is(err, media.ErrNotFound) {
		return e.stale(ctx, ms, item, name, source, err)
	} else if err != nil {
		return err
	}
	pr, err := probe(ctx, src)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &PermanentError{err}
	}
	p, err := newPlan(pr)
	if err != nil {
		return &PermanentError{err}
	}
	out := filepath.Join(dir, "out")
	if err := os.Mkdir(out, 0o700); err != nil {
		return err
	}
	e.c.Logger.InfoContext(ctx, "media/video: encoding", "key", srcKey, "duration", p.duration, "rungs", p.rungs,
		"audio", len(p.audio), "subs", len(p.subs), "threads", e.c.Threads)
	if err := ladder(ctx, src, out, p, e.c.Threads); err != nil {
		return err
	}
	if err := os.Remove(src); err != nil {
		return err
	}

	hls := &media.HLS{Source: source, Spec: Spec}
	downloads := map[string]media.Download{}
	for i, a := range p.audio {
		blob, pl, err := e.stream(ctx, item, out, fmt.Sprintf("a%d", i), "audio/mp4")
		if err != nil {
			return err
		}
		peak, _ := bandwidth(pl.segments)
		hls.Audio = append(hls.Audio, media.AudioTrack{ID: a.id, Lang: a.lang, Label: a.label, Default: a.def,
			Bandwidth: peak, Codecs: "mp4a.40.2", Blob: blob, Segments: pl.segments})
	}
	for i, s := range p.subs {
		blob, _, err := e.put(ctx, item, filepath.Join(out, fmt.Sprintf("s%d.vtt", i)), "text/vtt")
		if err != nil {
			return err
		}
		hls.Subs = append(hls.Subs, media.Subtitle{ID: s.id, Lang: s.lang, Label: s.label, Forced: s.forced, Blob: blob})
	}
	blob, _, err := e.put(ctx, item, filepath.Join(out, "sprite.jpg"), "image/jpeg")
	if err != nil {
		return err
	}
	hls.Sprite = &media.Sprite{Blob: blob, Cols: spriteCols, Rows: spriteRows, Width: spriteW, Height: spriteH,
		Interval: p.duration / (spriteCols * spriteRows)}
	for i, height := range p.rungs {
		v := fmt.Sprintf("v%d", i)
		codec, w, h, err := avcCodec(ctx, filepath.Join(out, v+".mp4"))
		if err != nil {
			return err
		}
		dl := filepath.Join(out, "d"+v+".mp4")
		if err := mux(ctx, out, i, p, dl); err != nil {
			return err
		}
		dlBlob, dlSize, err := e.put(ctx, item, dl, "video/mp4")
		if err != nil {
			return err
		}
		_ = os.Remove(dl)
		blob, pl, err := e.stream(ctx, item, out, v, "video/mp4")
		if err != nil {
			return err
		}
		peak, avg := bandwidth(pl.segments)
		hls.Video = append(hls.Video, media.Rendition{Height: h, Width: w, Bandwidth: peak, Average: avg, Codecs: codec,
			Blob: blob, Segments: pl.segments})
		downloads[DownloadKey(name, height)] = media.Download{Blob: dlBlob, Type: "video/mp4", Size: dlSize, Spec: Spec, Inputs: source}
	}

	if testBeforePromote != nil {
		testBeforePromote()
	}
	// Fence: promote only if the original is unchanged and the manifest file
	// still derives from it.
	if obj, err := e.c.Store.Head(ctx, srcKey); errors.Is(err, media.ErrNotFound) || err == nil && obj.ETag != srcObj.ETag {
		return e.stale(ctx, ms, item, name, source, errStale)
	} else if err != nil {
		return err
	}
	_, err = ms.Edit(ctx, item.Ref(), func(m *media.Manifest) error {
		i := m.File(name)
		if i < 0 || m.Files[i].Source() != source {
			return errStale
		}
		f := &m.Files[i]
		f.HLS = hls
		if f.Meta == nil {
			f.Meta = map[string]any{}
		}
		f.Meta["duration"] = p.duration
		f.Meta["w"], f.Meta["h"] = p.width, p.height
		for k, d := range m.Downloads {
			if n, ok := videoDownload(k, d); ok && n == name {
				delete(m.Downloads, k)
			}
		}
		if m.Downloads == nil {
			m.Downloads = map[string]media.Download{}
		}
		for k, d := range downloads {
			m.Downloads[k] = d
		}
		return nil
	})
	if errors.Is(err, errStale) {
		return e.stale(ctx, ms, item, name, source, err)
	}
	return err
}

// stale drops a result whose source is no longer the file's; it is an error
// only if the manifest still points at that source.
func (e *Encoder) stale(ctx context.Context, ms *media.Manifests, item media.Item, name, source string, cause error) error {
	man, _, err := ms.Get(ctx, item.Ref())
	if err != nil && !errors.Is(err, media.ErrNotFound) {
		return err
	}
	if man != nil {
		if i := man.File(name); i >= 0 && man.Files[i].Source() == source {
			return cause
		}
	}
	e.c.Logger.InfoContext(ctx, "media/video: source changed; result dropped", "ref", item.Ref().String(), "file", name, "source", source)
	return nil
}

// stream uploads a single-file fMP4 rendition after validating its byte ranges.
func (e *Encoder) stream(ctx context.Context, item media.Item, dir, name, contentType string) (string, playlist, error) {
	path := filepath.Join(dir, name+".mp4")
	st, err := os.Stat(path)
	if err != nil {
		return "", playlist{}, err
	}
	pl, err := parsePlaylist(filepath.Join(dir, name+".m3u8"), st.Size())
	if err != nil {
		return "", playlist{}, err
	}
	blob, _, err := e.put(ctx, item, path, contentType)
	return blob, pl, err
}

func (e *Encoder) fetch(ctx context.Context, key, path string) (media.Object, error) {
	rc, obj, err := e.c.Store.Get(ctx, key, media.GetOptions{})
	if err != nil {
		return obj, err
	}
	defer rc.Close()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return obj, err
	}
	n, err := io.Copy(f, rc)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && n != obj.Size {
		err = fmt.Errorf("media/video: read %d of %d bytes of %s", n, obj.Size, key)
	}
	return obj, err
}

// tempPattern names per-file scratch directories; Sweep removes leftovers.
const tempPattern = "ck-video-*"

// SweepTemp removes scratch left in dir by a killed process. Run it at
// worker start, before any job.
func SweepTemp(dir string) error {
	if dir == "" {
		dir = os.TempDir()
	}
	matches, err := filepath.Glob(filepath.Join(dir, tempPattern))
	if err != nil {
		return err
	}
	var errs []error
	for _, m := range matches {
		errs = append(errs, os.RemoveAll(m))
	}
	return errors.Join(errs...)
}
