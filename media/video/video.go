// Package video encodes an item's video files with ffmpeg into a byte-range
// HLS ladder (one single-file fMP4 blob per rendition and audio track),
// WebVTT subtitles, a thumbnail sprite and one muxed MP4 download per
// quality, and records them in the manifest's hls and downloads. It then
// grabs the item's poster frame from its selection; Frames serves the poster picker's frame grabs in the host. Jobs run in
// cmd/media-worker on the host's worker River schema.
package video

import (
	"cmp"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
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
	// Codecs are the ladder's codecs, in the order players are offered
	// them; every rung is encoded in each. Default DefaultCodecs.
	Codecs []media.Codec
	// Preset is the libx264/libx265 preset of rungs up to 1080, TopPreset of
	// the rungs above; both default "fast".
	Preset, TopPreset string
	// Encoder is EncoderAuto (default), EncoderCPU or EncoderNVENC. Each
	// codec's encoder is checked by a probe encode in New; a file NVENC
	// fails on is re-encoded on the CPU.
	Encoder string
	Hooks   media.Hooks // Failed: a source that can never be encoded
	Logger  *slog.Logger
	// Slots is the worker's queue (workqueue.Queue): a grabbed poster frame
	// is handed to its image job through it.
	// Without it, frame posters are grabbed but not encoded.
	Slots media.ProcessQueue
	// Sweeps schedules the folder's sweep after the encoder's manifest edits
	// (media.HostQueue); optional.
	Sweeps media.SweepScheduler
	// ProgressInterval throttles progress reports; default 2 s.
	ProgressInterval time.Duration
}

// Encoder runs encode jobs. It is idempotent: a file whose hls and downloads
// match its source and the job's Spec is skipped, and outputs are byte-identical on
// retry, so blob names repeat.
type Encoder struct {
	c        Config
	encoders map[media.Codec]string // ffmpeg encoder per codec
}

// Job encodes the video files of one manifest, and its audio files when
// Audio is set. Versioned, Video and Audio are the kind's: the manifest's
// address, its ladder and aspect bounds, and its audio settings.
type Job struct {
	Ref       contentref.ContentRef `json:"ref"`
	Versioned bool                  `json:"versioned,omitempty"`
	Video     media.Video           `json:"video"`
	Audio     *media.Audio          `json:"audio,omitempty"`
	only      encodeOnly            // the worker's job kinds: video or audio files only
}

type encodeOnly int

const (
	encodeAll encodeOnly = iota
	encodeVideo
	encodeAudio
)

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
	if len(c.Codecs) == 0 {
		c.Codecs = DefaultCodecs
	}
	for i, codec := range c.Codecs {
		if slices.Contains(c.Codecs[:i], codec) {
			return nil, fmt.Errorf("media/video: codec %q listed twice", codec)
		}
	}
	c.Encoder = cmp.Or(c.Encoder, EncoderAuto)
	encoders, err := resolveEncoders(context.Background(), c.TempDir, c.Encoder, c.Codecs)
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
	return &Encoder{c: c, encoders: encoders}, nil
}

// downloadCodec is the codec of the MP4 downloads: H.264 when the ladder
// has it (it plays everywhere), else the first.
func (e *Encoder) downloadCodec() media.Codec {
	if slices.Contains(e.c.Codecs, media.CodecH264) {
		return media.CodecH264
	}
	return e.c.Codecs[0]
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
// each file's outputs in its own manifest edit, one stage (rung) at a time,
// smallest first. report, when set, receives the files' progress.
func (e *Encoder) Encode(ctx context.Context, job Job, report Report) error {
	_, err := e.encode(ctx, job, report, false)
	return err
}

// encode runs the stale files' next stage, or every stage, and reports
// whether a file still has a stage to run.
func (e *Encoder) encode(ctx context.Context, job Job, report Report, oneStage bool) (more bool, err error) {
	kinds, err := media.NewRegistry(media.Kind{Name: job.Ref.ContentKind, Versioned: job.Versioned, Video: &job.Video})
	if err != nil {
		return false, &PermanentError{err}
	}
	r := e.recipeOf(&job.Video)
	item, err := kinds.Item(job.Ref)
	if err != nil {
		return false, &PermanentError{err}
	}
	if _, err := item.Section(); err != nil {
		return false, &PermanentError{err}
	}
	ms, err := media.NewManifests(e.c.Store, kinds, media.ManifestOptions{Locker: e.c.Locker, CacheSize: 1, Sweeps: e.c.Sweeps})
	if err != nil {
		return false, err
	}
	man, _, err := ms.Get(ctx, job.Ref)
	if errors.Is(err, media.ErrNotFound) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	var aspec string
	if job.Audio != nil {
		aspec = AudioSpec(*job.Audio)
	}
	var stale []media.File
	for _, f := range man.Files {
		if job.only != encodeAudio && IsVideo(f) && !fresh(man, f, r) ||
			job.only != encodeVideo && job.Audio != nil && IsAudio(f) && !audioFresh(man, f, aspec) {
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
		cur, source := f.HLS, f.Source()
		failSpec := r.failSpec
		if IsAudio(f) {
			failSpec = aspec
			source, err = e.audioFile(ctx, ms, item, *job.Audio, f.Name, source, prog.file(f.Name))
		}
		for IsVideo(f) {
			cur, err = e.file(ctx, ms, item, r, f.Name, source, cur, prog.file(f.Name))
			if cur != nil {
				source = cur.Source // a staged source is placed by its first stage
			}
			if err != nil || cur == nil || len(cur.Pending) == 0 {
				break
			}
			if oneStage {
				more = true
				break
			}
		}
		prog.done(f.Name)
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		var perm *PermanentError
		if errors.As(err, &perm) {
			err = e.fail(ctx, ms, item, failSpec, f.Name, source, perm.Err)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s %q: %w", job.Ref, f.Name, err))
		}
	}
	if len(errs) == 0 {
		// Drop downloads of files that are gone or no longer video or audio.
		man, err := ms.Edit(ctx, job.Ref, func(m *media.Manifest) error {
			for k, d := range m.Downloads {
				if name, ok := videoDownload(k, d); ok {
					if i := m.File(name); i < 0 || !IsVideo(m.Files[i]) {
						delete(m.Downloads, k)
					}
				} else if name, ok := audioDownload(k, d); ok {
					if i := m.File(name); i < 0 || !IsAudio(m.Files[i]) {
						delete(m.Downloads, k)
					}
				}
			}
			return nil
		})
		if err != nil {
			return more, err
		}
		if job.only == encodeAudio || !slices.ContainsFunc(man.Files, IsVideo) {
			return more, nil
		}
		defer prog.item(media.PhaseImages)()
		return more, e.images(ctx, ms, item, man)
	}
	return more, errors.Join(errs...)
}

// fail records that a source can never be encoded: the file's hls becomes
// {source, spec, error} with no renditions (fresh until the source or spec
// changes), its downloads and audio variant are dropped, and Hooks.Failed is told.
func (e *Encoder) fail(ctx context.Context, ms *media.Manifests, item media.Item, spec, name, source string, cause error) error {
	e.c.Logger.WarnContext(ctx, "media/video: cannot encode", "ref", item.Ref().String(), "file", name, "error", cause)
	_, err := ms.Edit(ctx, item.Ref(), func(m *media.Manifest) error {
		i := m.File(name)
		if i < 0 || m.Files[i].Source() != source {
			return errStale
		}
		m.Files[i].HLS = &media.HLS{Source: source, Spec: spec, Error: cause.Error()}
		delete(m.Files[i].Variants, media.AudioVariant)
		delete(m.Downloads, media.AudioDownloadKey(name))
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

func (e *Encoder) recipeOf(v *media.Video) jobRecipe {
	spec := e.Spec(*v)
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
	if h.Spec != spec || len(h.Video) == 0 || len(h.Pending) > 0 {
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

// file runs a file's next stage: one rung, smallest first, in every codec.
// Stage k+1 extends cur (the manifest hls) when cur holds exactly the
// plan's k smallest rungs of this source and spec; otherwise the file
// starts over at stage 1, which also makes the tracks and the sprite. It
// returns the published hls, nil if the result was dropped.
func (e *Encoder) file(ctx context.Context, ms *media.Manifests, item media.Item, r jobRecipe, name, source string, cur *media.HLS, fp *fileProgress) (*media.HLS, error) {
	dir, err := os.MkdirTemp(e.c.TempDir, tempPattern)
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)

	src := filepath.Join(dir, "source")
	srcKey, placed, srcObj, err := e.source(ctx, ms, item, name, source, src, fp)
	if err != nil || srcKey == "" {
		return nil, err
	}
	source = placed
	fp.set(media.PhaseProbing)
	pr, err := probe(ctx, src)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &PermanentError{err}
	}
	p, err := newPlan(pr, r.video)
	if err != nil {
		return nil, &PermanentError{err}
	}
	stages := slices.Clone(p.rungs)
	slices.Reverse(stages)
	k := e.stagesDone(cur, source, r.spec, stages)
	todo, extend, dl := stages[k], k > 0, e.downloadCodec()
	out := filepath.Join(dir, "out")
	if err := os.Mkdir(out, 0o700); err != nil {
		return nil, err
	}
	e.c.Logger.InfoContext(ctx, "media/video: encoding", "key", srcKey, "duration", p.duration, "rung", todo.n, "codecs", e.c.Codecs,
		"stage", k+1, "stages", len(stages), "audio", len(p.audio), "subs", len(p.subs), "encoders", e.encoders, "threads", e.c.Threads)
	fp.stage(k+1, len(stages))
	fp.probed(p.duration, out)
	ps := pass{rung: todo, codecs: slices.Clone(e.c.Codecs), sprite: !extend, enc: encoding{encoders: maps.Clone(e.encoders), threads: e.c.Threads,
		preset: e.c.Preset, topPreset: e.c.TopPreset, animation: r.video.Profile == media.VideoAnimation}}
	// The plan's top rung is copied from a source compliant in one of the
	// codecs, on the segments of that codec's published rung below.
	if extend && todo.n == p.rungs[0].n {
		c, ok, why := passthroughable(ctx, src, p, todo)
		below := slices.IndexFunc(cur.Video, func(v media.Rendition) bool { return v.Rung == stages[k-1].n && v.Codec == c })
		switch {
		case !ok || !slices.Contains(ps.codecs, c) || below < 0:
			e.c.Logger.DebugContext(ctx, "media/video: no passthrough", "key", srcKey, "codec", c, "reason", why)
		default:
			v := renditionName(todo.n, c)
			if err := copyRung(ctx, src, out, p, v, c); err != nil {
				return nil, err
			}
			if sameSegments(out, v, cur.Video[below].Segments) {
				e.c.Logger.InfoContext(ctx, "media/video: top rung copied from the source", "key", srcKey, "rung", todo.n, "codec", c)
				ps.codecs = slices.DeleteFunc(ps.codecs, func(x media.Codec) bool { return x == c })
			} else {
				e.c.Logger.WarnContext(ctx, "media/video: passthrough segments differ; encoding the rung", "key", srcKey, "rung", todo.n)
				if err := errors.Join(os.Remove(filepath.Join(out, v+".mp4")), os.Remove(filepath.Join(out, v+".m3u8"))); err != nil {
					return nil, err
				}
			}
		}
	}
	err = ladder(ctx, src, out, p, ps, fp)
	if err != nil && ctx.Err() == nil {
		cpu := false
		for c, enc := range ps.enc.encoders {
			if enc == nvencEncoders[c] {
				ps.enc.encoders[c], cpu = cpuEncoders[c], true
			}
		}
		if cpu {
			e.c.Logger.WarnContext(ctx, "media/video: NVENC failed; encoding on the CPU", "key", srcKey, "error", err)
			for _, c := range ps.codecs {
				if err = removeRendition(out, renditionName(todo.n, c)); err != nil {
					return nil, err
				}
			}
			err = ladder(ctx, src, out, p, ps, fp)
		}
	}
	if err != nil {
		return nil, err
	}
	if err := os.Remove(src); err != nil {
		return nil, err
	}
	fp.uploads(uploadBytes(out, !extend, renditionName(todo.n, dl)))
	fp.set(media.PhaseMuxing)

	// Each rendition is muxed and uploaded alongside the others and the
	// tracks; its files are removed as soon as they are stored. A later
	// stage's tracks only feed its download: the first stage stored them.
	hls := &media.HLS{Source: source, Spec: r.spec, Audio: make([]media.AudioTrack, len(p.audio)),
		Subs: make([]media.Subtitle, len(p.subs)), Video: make([]media.Rendition, len(e.c.Codecs)), Pending: rungNames(stages[k+1:])}
	var download media.Download
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(uploadParallel)
	if !extend {
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
	}
	for i, c := range e.c.Codecs {
		g.Go(func() error {
			v := renditionName(todo.n, c)
			path := filepath.Join(out, v+".mp4")
			codecs, err := codecString(path)
			if err != nil {
				return err
			}
			w, h, err := renditionFrame(gctx, path)
			if err != nil {
				return err
			}
			if w != todo.w || h != todo.h {
				return fmt.Errorf("rung %dp %s encoded %dx%d, want %dx%d", todo.n, c, w, h, todo.w, todo.h)
			}
			if c == dl {
				f := filepath.Join(out, "d"+v+".mp4")
				if err := mux(gctx, out, v, p, f); err != nil {
					return err
				}
				fp.set(media.PhaseUploading)
				blob, size, err := e.put(gctx, item, f, "video/mp4", fp)
				if err != nil {
					return err
				}
				download = media.Download{Blob: blob, Type: "video/mp4", Size: size, Spec: r.spec, Inputs: source}
				if err := os.Remove(f); err != nil {
					return err
				}
			}
			fp.set(media.PhaseUploading)
			blob, pl, err := e.stream(gctx, item, out, v, "video/mp4", fp)
			if err != nil {
				return err
			}
			peak, avg := bandwidth(pl.segments)
			hls.Video[i] = media.Rendition{Rung: todo.n, Codec: c, Width: w, Height: h, Bandwidth: peak, Average: avg, Codecs: codecs,
				Blob: blob, Segments: pl.segments}
			return os.Remove(path)
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}

	fp.set(media.PhasePublishing)
	if testBeforePromote != nil {
		testBeforePromote()
	}
	// Fence: promote only if the original is unchanged and the manifest file
	// still derives from it (a later stage: from the stages it extends).
	if obj, err := e.c.Store.Head(ctx, srcKey); errors.Is(err, media.ErrNotFound) || err == nil && obj.ETag != srcObj.ETag {
		return nil, e.stale(ctx, ms, item, name, source, errStale)
	} else if err != nil {
		return nil, err
	}
	var published *media.HLS
	_, err = ms.Edit(ctx, item.Ref(), func(m *media.Manifest) error {
		i := m.File(name)
		if i < 0 || m.Files[i].Source() != source {
			return errStale
		}
		f := &m.Files[i]
		if extend {
			h := f.HLS
			if h == nil || h.Source != source || h.Spec != r.spec || !slices.Equal(h.Pending, cur.Pending) || len(h.Video) != len(cur.Video) {
				return errStale
			}
			next := *h
			next.Video = append(slices.Clone(h.Video), hls.Video...)
			next.Pending = hls.Pending
			f.HLS = &next
		} else {
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
		}
		orderVideo(f.HLS.Video, e.c.Codecs)
		if m.Downloads == nil {
			m.Downloads = map[string]media.Download{}
		}
		m.Downloads[DownloadKey(name, todo.n)] = download
		published = f.HLS
		return nil
	})
	if errors.Is(err, errStale) {
		return nil, e.stale(ctx, ms, item, name, source, err)
	}
	return published, err
}

// stagesDone is how many of stages (rungs, smallest first) cur holds for
// source and spec: its renditions are exactly those rungs in every codec
// and its pending rungs the rest; any other cur starts over.
func (e *Encoder) stagesDone(cur *media.HLS, source, spec string, stages []rung) int {
	if cur == nil || cur.Source != source || cur.Spec != spec || cur.Error != "" || len(cur.Pending) == 0 {
		return 0
	}
	k := len(stages) - len(cur.Pending)
	if k <= 0 || !slices.Equal(cur.Pending, rungNames(stages[k:])) || len(cur.Video) != k*len(e.c.Codecs) {
		return 0
	}
	for _, s := range stages[:k] {
		for _, c := range e.c.Codecs {
			if !slices.ContainsFunc(cur.Video, func(v media.Rendition) bool { return v.Rung == s.n && v.Codec == c }) {
				return 0
			}
		}
	}
	return k
}

func rungNames(rungs []rung) []int {
	var out []int
	for _, r := range rungs {
		out = append(out, r.n)
	}
	return out
}

// removeRendition removes a rendition's files from a failed pass.
func removeRendition(dir, v string) error {
	for _, f := range []string{v + ".mp4", v + ".m3u8"} {
		if err := os.Remove(filepath.Join(dir, f)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
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

// fetch downloads key to path, returning its SHA-256 computed on the way.
func (e *Encoder) fetch(ctx context.Context, key, path string, fp *fileProgress) (media.Object, []byte, error) {
	rc, obj, err := e.c.Store.Get(ctx, key, media.GetOptions{})
	if err != nil {
		return obj, nil, err
	}
	defer rc.Close()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return obj, nil, err
	}
	h := sha256.New()
	start := time.Now()
	n, err := io.Copy(io.MultiWriter(f, h), rc)
	fp.transferred(n, time.Since(start))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && n != obj.Size {
		err = fmt.Errorf("media/video: read %d of %d bytes of %s", n, obj.Size, key)
	}
	return obj, h.Sum(nil), err
}

// uploadBytes is what remains to upload after a pass: every rendition,
// the tracks and sprite of a first stage, and the muxed download (rendition
// dl and all audio).
func uploadBytes(dir string, tracks bool, dl string) int64 {
	var total, audio int64
	entries, _ := os.ReadDir(dir)
	for _, d := range entries {
		info, err := d.Info()
		if err != nil || !info.Mode().IsRegular() || strings.HasSuffix(d.Name(), ".m3u8") {
			continue
		}
		switch {
		case strings.HasPrefix(d.Name(), "a"):
			audio += info.Size()
		case d.Name() == dl+".mp4":
			total += 2 * info.Size()
		case strings.HasPrefix(d.Name(), "v"):
			total += info.Size()
		default:
			if tracks {
				total += info.Size()
			}
		}
	}
	if tracks {
		total += audio
	}
	return total + audio
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
