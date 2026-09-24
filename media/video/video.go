// Package video encodes an item's video files with ffmpeg into a byte-range
// HLS ladder (one single-file fMP4 blob per rendition and audio track),
// WebVTT subtitles, a thumbnail sprite and one muxed MP4 download per
// quality, and records them in the manifest's hls and downloads. It then
// grabs the item's poster frame and renders its hover preview from their
// selections; Frames serves the poster picker's frame grabs in the host. Jobs run in
// cmd/media-worker on River schema Schema.
package video

import (
	"context"
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
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media"
)

// Config configures an Encoder.
type Config struct {
	Store media.Store
	// Locker serializes manifest edits when Store lacks conditional PUT; it
	// must share the hosts' lock space (media.PGLocker on the host database).
	Locker  media.Locker
	TempDir string // scratch for the source and outputs; default os.TempDir()
	Threads int    // CPU threads for ffmpeg; default GOMAXPROCS (the container's CPU limit)
	// Parallel caps the chunks of one video encoded at once, sharing Threads;
	// default Threads/chunkThreads.
	Parallel int
	// Encoder is EncoderAuto (default), EncoderX264 or EncoderNVENC. NVENC
	// is checked by a probe encode in New; a file it fails on is re-encoded
	// with x264.
	Encoder string
	Hooks   media.Hooks // Failed: a source that can never be encoded
	Logger  *slog.Logger
	// Slots is the host's media queue (media.NewProcessInserter): a grabbed
	// poster frame is handed to the image job through it. Without it, frame
	// posters are grabbed but not encoded.
	Slots media.ProcessQueue
	// ProgressInterval throttles progress reports; default 2 s.
	ProgressInterval time.Duration
}

// Encoder runs encode jobs. It is idempotent: a file whose hls and downloads
// match its source and the job's Spec is skipped, and outputs are byte-identical on
// retry, so blob names repeat.
type Encoder struct {
	c Config
}

// Job encodes the video files of one manifest. Versioned and Video are the
// kind's: the manifest's address and its ladder and aspect bounds.
type Job struct {
	Ref       contentref.ContentRef `json:"ref"`
	Versioned bool                  `json:"versioned,omitempty"`
	Video     media.Video           `json:"video"`
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
	if c.Parallel <= 0 {
		c.Parallel = max(1, c.Threads/chunkThreads)
	}
	switch c.Encoder {
	case "", EncoderAuto:
		c.Encoder = EncoderX264
		if nvencWorks(context.Background()) == nil {
			c.Encoder = EncoderNVENC
		}
	case EncoderNVENC:
		if err := nvencWorks(context.Background()); err != nil {
			return nil, fmt.Errorf("media/video: NVENC unavailable: %w", err)
		}
	case EncoderX264:
	default:
		return nil, fmt.Errorf("media/video: unknown Encoder %q", c.Encoder)
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.ProgressInterval <= 0 {
		c.ProgressInterval = 2 * time.Second
	}
	return &Encoder{c: c}, nil
}

// PermanentError marks a source that can never be encoded (no video stream,
// unreadable container, aspect out of range); retrying it is pointless.
type PermanentError struct{ Err error }

func (e *PermanentError) Error() string { return "media/video: " + e.Err.Error() }
func (e *PermanentError) Unwrap() error { return e.Err }

// IsVideo reports whether a manifest file is encoded by this package.
func IsVideo(f media.File) bool { return strings.HasPrefix(f.Type, "video/") }

// DownloadKey is the manifest downloads key of a file's rung, e.g. "source-1080p".
func DownloadKey(file string, rung int) string { return file + "-" + strconv.Itoa(rung) + "p" }

var downloadKey = regexp.MustCompile(`^(.+)-(\d+)p$`)

// Encode brings every video file of the job's manifest up to date, promoting
// each file's outputs in its own manifest edit. report, when set, receives
// the files' progress.
func (e *Encoder) Encode(ctx context.Context, job Job, report Report) error {
	kinds, err := media.NewRegistry(media.Kind{Name: job.Ref.ContentKind, Versioned: job.Versioned, Video: &job.Video})
	if err != nil {
		return &PermanentError{err}
	}
	r := recipeOf(&job.Video)
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
	var stale []media.File
	for _, f := range man.Files {
		if IsVideo(f) && !fresh(man, f, r) {
			stale = append(stale, f)
		}
	}
	names := make([]string, len(stale))
	for i, f := range stale {
		names[i] = f.Name
	}
	prog := newProgress(ctx, report, e.c.ProgressInterval, time.Now, names)
	var errs []error
	for _, f := range stale {
		err := e.file(ctx, ms, item, r, f.Name, f.Source(), prog.file(f.Name))
		prog.done(f.Name)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var perm *PermanentError
		if errors.As(err, &perm) {
			err = e.fail(ctx, ms, item, r.failSpec, f.Name, f.Source(), perm.Err)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s %q: %w", job.Ref, f.Name, err))
		}
	}
	if len(errs) == 0 {
		// Drop downloads of files that are gone or no longer video.
		man, err := ms.Edit(ctx, job.Ref, func(m *media.Manifest) error {
			for k, d := range m.Downloads {
				if name, ok := videoDownload(k, d); ok {
					if i := m.File(name); i < 0 || !IsVideo(m.Files[i]) {
						delete(m.Downloads, k)
					}
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
		defer prog.item(media.PhaseImages)()
		return e.images(ctx, ms, item, man)
	}
	return errors.Join(errs...)
}

// fail records that a source can never be encoded: the file's hls becomes
// {source, spec, error} with no renditions (fresh until the source or spec
// changes), its downloads are dropped, and Hooks.Failed is told.
func (e *Encoder) fail(ctx context.Context, ms *media.Manifests, item media.Item, spec, name, source string, cause error) error {
	e.c.Logger.WarnContext(ctx, "media/video: cannot encode", "ref", item.Ref().String(), "file", name, "error", cause)
	_, err := ms.Edit(ctx, item.Ref(), func(m *media.Manifest) error {
		i := m.File(name)
		if i < 0 || m.Files[i].Source() != source {
			return errStale
		}
		m.Files[i].HLS = &media.HLS{Source: source, Spec: spec, Error: cause.Error()}
		for k, d := range m.Downloads {
			if n, ok := videoDownload(k, d); ok && n == name {
				delete(m.Downloads, k)
			}
		}
		return nil
	})
	if errors.Is(err, errStale) {
		return nil
	} else if err != nil {
		return err
	}
	if e.c.Hooks.Failed != nil {
		e.c.Hooks.Failed(ctx, item.Ref(), name, &PermanentError{cause})
	}
	return nil
}

// jobRecipe is a job's video settings and its Spec; failSpec also covers
// the aspect bounds, so a failure is retried when they change.
type jobRecipe struct {
	video          *media.Video
	spec, failSpec string
}

func recipeOf(v *media.Video) jobRecipe {
	spec := Spec(v.Ladder)
	lo, hi := v.Aspects()
	return jobRecipe{video: v, spec: spec, failSpec: fmt.Sprintf("%s|aspect:%g-%g", spec, lo, hi)}
}

func fresh(m *media.Manifest, f media.File, r jobRecipe) bool {
	h, spec := f.HLS, r.spec
	if h == nil || h.Source != f.Source() {
		return false
	}
	if h.Error != "" {
		return h.Spec == r.failSpec
	}
	if h.Spec != spec || len(h.Video) == 0 {
		return false
	}
	for _, r := range h.Video {
		if d, ok := m.Downloads[DownloadKey(f.Name, r.Rung)]; !ok || d.Spec != spec || d.Inputs != h.Source {
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

func (e *Encoder) file(ctx context.Context, ms *media.Manifests, item media.Item, r jobRecipe, name, source string, fp *fileProgress) error {
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
	srcObj, err := e.fetch(ctx, srcKey, src, fp)
	if errors.Is(err, media.ErrNotFound) {
		return e.stale(ctx, ms, item, name, source, err)
	} else if err != nil {
		return err
	}
	fp.set(media.PhaseProbing)
	pr, err := probe(ctx, src)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &PermanentError{err}
	}
	p, err := newPlan(pr, r.video)
	if err != nil {
		return &PermanentError{err}
	}
	out := filepath.Join(dir, "out")
	if err := os.Mkdir(out, 0o700); err != nil {
		return err
	}
	e.c.Logger.InfoContext(ctx, "media/video: encoding", "key", srcKey, "duration", p.duration, "rungs", p.rungs,
		"audio", len(p.audio), "subs", len(p.subs), "encoder", e.c.Encoder, "threads", e.c.Threads, "parallel", e.c.Parallel)
	fp.probed(p.duration, out)
	enc := encoding{codec: e.c.Encoder, threads: e.c.Threads, parallel: e.c.Parallel}
	err = ladder(ctx, src, out, p, enc, fp)
	if err != nil && enc.codec == EncoderNVENC && ctx.Err() == nil {
		e.c.Logger.WarnContext(ctx, "media/video: NVENC failed; encoding with x264", "key", srcKey, "error", err)
		enc.codec = EncoderX264
		if err = errors.Join(os.RemoveAll(out), os.Mkdir(out, 0o700)); err == nil {
			err = ladder(ctx, src, out, p, enc, fp)
		}
	}
	if err != nil {
		return err
	}
	if err := os.Remove(src); err != nil {
		return err
	}
	fp.uploads(uploadBytes(out, len(p.rungs)))
	fp.set(media.PhaseMuxing)

	// Each rung is muxed and uploaded alongside the others and the tracks;
	// its files are removed as soon as they are stored.
	hls := &media.HLS{Source: source, Spec: r.spec, Audio: make([]media.AudioTrack, len(p.audio)),
		Subs: make([]media.Subtitle, len(p.subs)), Video: make([]media.Rendition, len(p.rungs))}
	dls := make([]media.Download, len(p.rungs))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(uploadParallel)
	for i, a := range p.audio {
		g.Go(func() error {
			blob, pl, err := e.stream(gctx, item, out, fmt.Sprintf("a%d", i), "audio/mp4", fp)
			if err != nil {
				return err
			}
			peak, _ := bandwidth(pl.segments)
			hls.Audio[i] = media.AudioTrack{ID: a.id, Lang: a.lang, Label: a.label, Default: a.def,
				Bandwidth: peak, Codecs: "mp4a.40.2", Blob: blob, Segments: pl.segments}
			return nil
		})
	}
	for i, s := range p.subs {
		g.Go(func() error {
			blob, _, err := e.put(gctx, item, filepath.Join(out, fmt.Sprintf("s%d.vtt", i)), "text/vtt", fp)
			hls.Subs[i] = media.Subtitle{ID: s.id, Lang: s.lang, Label: s.label, Forced: s.forced, Blob: blob}
			return err
		})
	}
	g.Go(func() error {
		blob, _, err := e.put(gctx, item, filepath.Join(out, "sprite.jpg"), "image/jpeg", fp)
		hls.Sprite = &media.Sprite{Blob: blob, Cols: spriteCols, Rows: spriteRows, Width: p.tileW, Height: p.tileH,
			Interval: p.duration / (spriteCols * spriteRows)}
		return err
	})
	for i, rg := range p.rungs {
		g.Go(func() error {
			v := fmt.Sprintf("v%d", i)
			codec, w, h, err := avcCodec(gctx, filepath.Join(out, v+".mp4"))
			if err != nil {
				return err
			}
			if w != rg.w || h != rg.h {
				return fmt.Errorf("rung %dp encoded %dx%d, want %dx%d", rg.n, w, h, rg.w, rg.h)
			}
			dl := filepath.Join(out, "d"+v+".mp4")
			if err := mux(gctx, out, i, p, dl); err != nil {
				return err
			}
			fp.set(media.PhaseUploading)
			dlBlob, dlSize, err := e.put(gctx, item, dl, "video/mp4", fp)
			if err != nil {
				return err
			}
			blob, pl, err := e.stream(gctx, item, out, v, "video/mp4", fp)
			if err != nil {
				return err
			}
			peak, avg := bandwidth(pl.segments)
			hls.Video[i] = media.Rendition{Rung: rg.n, Width: w, Height: h, Bandwidth: peak, Average: avg, Codecs: codec,
				Blob: blob, Segments: pl.segments}
			dls[i] = media.Download{Blob: dlBlob, Type: "video/mp4", Size: dlSize, Spec: r.spec, Inputs: source}
			return errors.Join(os.Remove(dl), os.Remove(filepath.Join(out, v+".mp4")))
		})
	}
	if err := g.Wait(); err != nil {
		return err
	}
	downloads := map[string]media.Download{}
	for i, rg := range p.rungs {
		downloads[DownloadKey(name, rg.n)] = dls[i]
	}

	fp.set(media.PhasePublishing)
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
func (e *Encoder) stream(ctx context.Context, item media.Item, dir, name, contentType string, fp *fileProgress) (string, playlist, error) {
	path := filepath.Join(dir, name+".mp4")
	st, err := os.Stat(path)
	if err != nil {
		return "", playlist{}, err
	}
	pl, err := parsePlaylist(filepath.Join(dir, name+".m3u8"), st.Size())
	if err != nil {
		return "", playlist{}, err
	}
	blob, _, err := e.put(ctx, item, path, contentType, fp)
	return blob, pl, err
}

func (e *Encoder) fetch(ctx context.Context, key, path string, fp *fileProgress) (media.Object, error) {
	rc, obj, err := e.c.Store.Get(ctx, key, media.GetOptions{})
	if err != nil {
		return obj, err
	}
	defer rc.Close()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return obj, err
	}
	start := time.Now()
	n, err := io.Copy(f, rc)
	fp.transferred(n, time.Since(start))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && n != obj.Size {
		err = fmt.Errorf("media/video: read %d of %d bytes of %s", n, obj.Size, key)
	}
	return obj, err
}

// uploadBytes is what remains to upload after the ladder pass: every output,
// plus one muxed download per rung (its rendition and all audio).
func uploadBytes(dir string, rungs int) int64 {
	var total, audio int64
	entries, _ := os.ReadDir(dir)
	for _, d := range entries {
		info, err := d.Info()
		if err != nil || !info.Mode().IsRegular() || strings.HasSuffix(d.Name(), ".m3u8") {
			continue
		}
		total += info.Size()
		switch {
		case strings.HasPrefix(d.Name(), "a"):
			audio += info.Size()
		case strings.HasPrefix(d.Name(), "v"):
			total += info.Size()
		}
	}
	return total + int64(rungs)*audio
}

// uploadParallel bounds the concurrent muxes and uploads of one file.
const uploadParallel = 4

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
