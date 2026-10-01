package video_test

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	riverhelpers "github.com/open-rails/helpers/river"
	"github.com/riverqueue/river"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/internal/s3test"
	"github.com/open-rails/contentkit/media/internal/videotest"
	"github.com/open-rails/contentkit/media/layout"
	"github.com/open-rails/contentkit/media/token"
	"github.com/open-rails/contentkit/media/video"
	"github.com/open-rails/contentkit/media/workqueue"
)

// CONTENTKIT_TEST_FFMPEG=1 (the CI video job) fails instead of skipping without ffmpeg.

// cid is the n-th test item id, a canonical UUIDv7.
func cid(n int) string { return fmt.Sprintf("01920000-0000-7000-8000-%012d", n) }

var (
	videoTypes = []string{"video/x-matroska", "video/mp4", "video/quicktime"}
	audioTypes = []string{"audio/wav", "audio/mpeg", "audio/flac"}
	imageTypes = []string{"image/png", "image/jpeg"}
)

// opts shape a test's video kind and worker.
type opts struct {
	ladder   []int         // HLS ladder; nil is the default
	mp4      []int         // MP4 presets "mp4-{n}" at these rungs
	codecs   []media.Codec // the worker's; default H.264 only
	chunk    time.Duration // WorkerConfig.ChunkTarget
	loudness float64
	profile  string             // the HLS and MP4 presets' profile
	limits   *media.VideoLimits // the source's and audio's Upload.Video
	encoder  string             // default EncoderCPU
	shared   bool               // also a "clip" kind in a shared namespace
}

// testKind is a Hentai0-like video kind: a source with an HLS ladder and MP4
// downloads, subtitle sidecars, a poster grabbed from the source, and audio.
func testKind(o opts) media.Kind {
	k := media.Kind{Name: "video", KeepOriginals: true,
		Uploads: []media.Upload{
			{Path: "source", Types: videoTypes, MaxBytes: 1 << 30, Video: o.limits},
			{Path: "subs/{name}", Types: media.SubtitleTypes, MaxBytes: 32 << 20},
			{Path: "poster", Types: imageTypes, MaxBytes: 10 << 20, Frames: "source"},
			{Path: "audio/{name}", Types: audioTypes, MaxBytes: 64 << 20, Video: o.limits},
		},
		Private: []media.Private{
			{Name: "hls", From: "source", To: "hls/", HLS: &media.HLS{Ladder: o.ladder, Profile: o.profile}},
			{Name: "vtt", From: "subs/{name}", To: "vtt/{name}.vtt", Subtitles: &media.Subtitles{}},
			{Name: "listen", From: "audio/{name}", To: "listen/{name}/", Audio: &media.Audio{Loudness: o.loudness}},
		},
		Public: []media.Public{{Name: "poster", From: "poster", To: "poster-{w}.webp", Widths: []int{320}}},
	}
	for _, n := range o.mp4 {
		k.Private = append(k.Private, media.Private{Name: fmt.Sprintf("mp4-%d", n), From: "source",
			To: fmt.Sprintf("video/source-%dp.mp4", n), Download: fmt.Sprintf("{title} (%dp).mp4", n), MP4: &media.MP4{Rung: n, Profile: o.profile}})
	}
	return k
}

type grants struct{}

func (grants) CanUpload(context.Context, access.Actor, media.UploadTarget) (media.UploadGrant, error) {
	return media.UploadGrant{Allowed: true, Exempt: true}, nil
}

// Resolve shows every item to everyone: a new item is not hidden.
func (grants) Resolve(_ context.Context, refs []contentref.ContentRef, _ access.Actor) (map[contentref.ContentKey]access.Resolution, error) {
	out := map[contentref.ContentKey]access.Resolution{}
	for _, r := range refs {
		out[r.Key()] = access.Resolution{Visible: true, Accessible: true}
	}
	return out, nil
}

// failure is one Hooks.Failed report.
type failure struct {
	ref  contentref.ContentRef
	path string
	err  error
}

// env is one test's media stack on real MinIO and PostgreSQL: the app's
// registry and uploads, the host's worker queue, and a River client running
// the video worker (start).
type env struct {
	t      *testing.T
	ctx    context.Context
	s3     *s3test.Env
	store  media.Store
	reg    *media.Registry
	ms     *media.Manifests
	up     *media.Uploads
	pool   *pgxpool.Pool
	schema string
	queue  *workqueue.Queue
	enc    *video.Encoder
	wc     video.WorkerConfig
	worker *river.Client[pgx.Tx]
	events <-chan *river.Event
	ref    contentref.ContentRef
	editor access.Actor

	mu       sync.Mutex
	failures []failure
}

func newEnv(t *testing.T, o opts) *env {
	t.Helper()
	videotest.RequireFFmpeg(t)
	s3 := s3test.Open(t)
	return newEnvOn(t, s3, o, "")
}

// newEnvOn builds the stack over s3; schema "" is a new one.
func newEnvOn(t *testing.T, s3 *s3test.Env, o opts, schema string) *env {
	t.Helper()
	e := &env{t: t, ctx: t.Context(), s3: s3, store: s3.Store, editor: access.Actor{ID: "editor", Kind: "user"}}
	cfg := media.Config{Namespace: s3.Tenant, Kinds: []media.Kind{testKind(o)},
		Hooks: media.Hooks{CanUpload: grants{}, Resolver: grants{}, Failed: func(_ context.Context, ref contentref.ContentRef, path string, err error) {
			e.mu.Lock()
			defer e.mu.Unlock()
			e.failures = append(e.failures, failure{ref, path, err})
		}}}
	if o.shared {
		clip := testKind(o)
		clip.Name, clip.Namespace = "clip", "sh"+s3.Tenant[1:]
		cfg.Kinds = append(cfg.Kinds, clip)
		t.Cleanup(func() { e.drop(clip.Namespace + "/") })
	}
	var err error
	if e.reg, err = media.NewRegistry(cfg); err != nil {
		t.Fatal(err)
	}
	e.pool = pgtest.Pool(t, nil)
	if e.schema = schema; schema == "" {
		e.schema = pgtest.EmptySchema(t, e.ctx, e.pool)
	}
	if err := workqueue.Migrate(e.ctx, e.pool, e.schema); err != nil {
		t.Fatal(err)
	}
	if e.ms, err = media.NewManifests(e.store, e.reg, media.ManifestOptions{Locker: media.PGLocker(e.pool)}); err != nil {
		t.Fatal(err)
	}
	if e.queue, err = workqueue.New(e.pool, e.reg, e.schema); err != nil {
		t.Fatal(err)
	}
	ring, err := token.NewRing(token.Key{ID: "k1", Secret: []byte("0123456789abcdef0123456789abcdef")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if e.up, err = media.NewUploads(media.UploadOptions{Store: e.store, Manifests: e.ms, Tickets: &ring, Queue: e.queue}); err != nil {
		t.Fatal(err)
	}
	if o.codecs == nil {
		o.codecs = []media.Codec{media.CodecH264}
	}
	if e.enc, err = video.New(e.ctx, video.Config{Manifests: e.ms, Queue: e.queue, TempDir: t.TempDir(), Threads: 2,
		Encoder: cmp.Or(o.encoder, video.EncoderCPU), Codecs: o.codecs, ProgressInterval: 200 * time.Millisecond}); err != nil {
		if o.encoder == video.EncoderNVENC {
			t.Skip(err)
		}
		t.Fatal(err)
	}
	e.wc = video.WorkerConfig{Encoder: e.enc, Pool: e.pool, Schema: e.schema, Timeout: time.Hour, ChunkTarget: o.chunk}
	e.ref = e.refOf("video", 1)
	return e
}

func (e *env) refOf(kind string, n int) contentref.ContentRef {
	e.t.Helper()
	ref, err := e.reg.Ref(kind, cid(n))
	if err != nil {
		e.t.Fatal(err)
	}
	return ref
}

func (e *env) item() media.Item {
	item, err := e.reg.Item(e.ref)
	if err != nil {
		e.t.Fatal(err)
	}
	return item
}

// drop deletes every object under prefix.
func (e *env) drop(prefix string) {
	ctx := context.Background()
	for o, err := range e.store.List(ctx, prefix) {
		if err != nil {
			return
		}
		_ = e.store.Delete(ctx, o.Key)
	}
}

// start runs the video worker until the test ends. Test hooks (SetBeforePublish)
// are set before it starts and restored after it stops.
func (e *env) start() {
	e.t.Helper()
	contribution, err := video.Contribution(e.wc)
	if err != nil {
		e.t.Fatal(err)
	}
	cfg := video.ClientConfig(e.wc)
	cfg.FetchPollInterval = 100 * time.Millisecond
	if e.worker, err = riverhelpers.New(e.ctx, e.pool, cfg, contribution); err != nil {
		e.t.Fatal(err)
	}
	var stop func()
	e.events, stop = e.worker.Subscribe(river.EventKindJobCompleted, river.EventKindJobFailed, river.EventKindJobCancelled)
	if err := e.worker.Start(e.ctx); err != nil {
		e.t.Fatal(err)
	}
	worker := e.worker
	e.t.Cleanup(func() {
		stop()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = worker.StopAndCancel(ctx)
	})
}

// stopWorker stops the worker (before restoring a test hook).
func (e *env) stopWorker() {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := e.worker.StopAndCancel(ctx); err != nil {
		e.t.Fatal(err)
	}
}

// upload presigns body for path and uploads it like the browser does (one
// PUT, or parts), returning the path and name to commit.
func (e *env) upload(ref contentref.ContentRef, path, typ string, body []byte) (string, string) {
	e.t.Helper()
	sum := sha256.Sum256(body)
	p, err := e.up.Presign(e.ctx, e.editor, media.PresignRequest{Ref: ref, Path: path, Type: typ, Size: int64(len(body)), SHA256: sum[:]})
	if err != nil {
		e.t.Fatalf("presign %s: %v", path, err)
	}
	send := func(r media.PresignedRequest, b []byte) {
		req, _ := http.NewRequestWithContext(e.ctx, r.Method, r.URL, bytes.NewReader(b))
		req.Header = r.Header.Clone()
		req.ContentLength = int64(len(b))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			e.t.Fatal(err)
		}
		msg, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			e.t.Fatalf("PUT %s: %d %s", path, resp.StatusCode, msg)
		}
	}
	switch {
	case p.Put != nil:
		send(*p.Put, body)
	case p.Multipart != nil:
		var parts []media.PartRequest
		for off, n := 0, int32(1); off < len(body); off, n = off+int(p.Multipart.MaxPartSize), n+1 {
			part := body[off:min(len(body), off+int(p.Multipart.MaxPartSize))]
			sum := sha256.Sum256(part)
			parts = append(parts, media.PartRequest{Number: n, Size: int64(len(part)), SHA256: sum[:]})
		}
		signed, err := e.up.PresignParts(e.ctx, e.editor, p.Multipart.Ticket, parts)
		if err != nil {
			e.t.Fatal(err)
		}
		for i, s := range signed {
			off := i * int(p.Multipart.MaxPartSize)
			send(s.PresignedRequest, body[off:min(len(body), off+int(p.Multipart.MaxPartSize))])
		}
		if _, err := e.up.Complete(e.ctx, e.editor, p.Multipart.Ticket); err != nil {
			e.t.Fatal(err)
		}
	}
	return p.Path, p.Blob
}

// put uploads a file and commits it at path (its stem; the extension comes
// from the type) with extra ops.
func (e *env) put(path, typ, file string, meta map[string]any, extra ...media.Op) *media.Manifest {
	e.t.Helper()
	body, err := os.ReadFile(file)
	if err != nil {
		e.t.Fatal(err)
	}
	p, blob := e.upload(e.ref, path, typ, body)
	return e.commit(append([]media.Op{{Op: media.OpPut, Path: p, Blob: blob, Meta: meta}}, extra...)...)
}

// commit commits ops; staged uploads are placed and the item enqueued, as
// the worker's place job does.
func (e *env) commit(ops ...media.Op) *media.Manifest {
	e.t.Helper()
	m, err := e.up.Commit(e.ctx, e.editor, e.ref, ops)
	if err != nil {
		e.t.Fatalf("commit %+v: %v", ops, err)
	}
	if len(m.StagedNames()) == 0 {
		return m
	}
	if _, err := e.ms.Place(e.ctx, e.ref); err != nil {
		e.t.Fatal(err)
	}
	if err := e.queue.Enqueue(e.ctx, media.ProcessJob{Ref: e.ref}); err != nil {
		e.t.Fatal(err)
	}
	m, _, err = e.ms.Get(e.ctx, e.ref)
	if err != nil {
		e.t.Fatal(err)
	}
	return m
}

// wait blocks until no video or audio job of the schema is waiting or
// running, failing on a failed job.
func (e *env) wait() {
	e.t.Helper()
	deadline := time.After(10 * time.Minute)
	idle := 0
	for idle < 2 {
		select {
		case ev := <-e.events:
			if ev.Kind == river.EventKindJobFailed {
				e.t.Fatalf("job %s %d failed: %+v", ev.Job.Kind, ev.Job.ID, ev.Job.Errors)
			}
			continue
		case <-deadline:
			e.t.Fatal("video jobs did not finish")
		case <-time.After(150 * time.Millisecond):
		}
		var active int
		if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM `+pgx.Identifier{e.schema, "river_job"}.Sanitize()+`
WHERE kind = ANY($1) AND state IN ('available', 'pending', 'retryable', 'running', 'scheduled')`, workqueue.EncodeKinds).Scan(&active); err != nil {
			e.t.Fatal(err)
		}
		if active == 0 {
			idle++
		} else {
			idle = 0
		}
	}
}

// next waits for the next completed job of kind.
func (e *env) next(kind string) {
	e.t.Helper()
	deadline := time.After(10 * time.Minute)
	for {
		select {
		case ev := <-e.events:
			if ev.Kind == river.EventKindJobFailed {
				e.t.Fatalf("job %s %d failed: %+v", ev.Job.Kind, ev.Job.ID, ev.Job.Errors)
			}
			if ev.Kind == river.EventKindJobCompleted && ev.Job.Kind == kind {
				return
			}
		case <-deadline:
			e.t.Fatalf("no %s job completed", kind)
		}
	}
}

func (e *env) manifest() *media.Manifest {
	e.t.Helper()
	m, _, err := e.ms.Get(e.ctx, e.ref)
	if err != nil {
		e.t.Fatal(err)
	}
	return m
}

func (e *env) file(m *media.Manifest, path string) media.File {
	e.t.Helper()
	f, ok := m.Get(path)
	if !ok {
		var paths []string
		for _, f := range m.Files {
			paths = append(paths, f.Path)
		}
		e.t.Fatalf("no %s in %v", path, paths)
	}
	return f
}

func (e *env) readiness(m *media.Manifest) media.Readiness { return e.item().Kind().Readiness(m) }

func (e *env) failed() []failure {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.failures)
}

// blob downloads a private blob to a file named for its type.
func (e *env) blob(name string) string {
	e.t.Helper()
	key, err := e.item().Blob(name)
	if err != nil {
		e.t.Fatal(err)
	}
	rc, obj, err := e.store.Get(e.ctx, key, media.GetOptions{})
	if err != nil {
		e.t.Fatalf("blob %s: %v", name, err)
	}
	defer rc.Close()
	ext := map[string]string{"video/mp4": ".mp4", "audio/mp4": ".mp4", "text/vtt": ".vtt", "image/jpeg": ".jpg", "image/png": ".png"}[obj.ContentType]
	path := filepath.Join(e.t.TempDir(), name+ext)
	f, err := os.Create(path)
	if err != nil {
		e.t.Fatal(err)
	}
	defer f.Close()
	if _, err := io.Copy(f, rc); err != nil {
		e.t.Fatal(err)
	}
	return path
}

func (e *env) index(f media.File) media.TrackIndex {
	e.t.Helper()
	if f.Track == nil || f.Track.Index == "" {
		e.t.Fatalf("%s has no track index", f.Path)
	}
	var idx media.TrackIndex
	if err := json.Unmarshal(must(os.ReadFile(e.blob(f.Track.Index))), &idx); err != nil {
		e.t.Fatal(err)
	}
	return idx
}

// runs are the schema's encode runs as "{spec preset}:{rung}".
func (e *env) runs() []string {
	e.t.Helper()
	rows, err := e.pool.Query(e.ctx, `SELECT spec, rung FROM `+pgx.Identifier{e.schema, "encode_run"}.Sanitize()+` ORDER BY spec, rung`)
	if err != nil {
		e.t.Fatal(err)
	}
	var out []string
	for rows.Next() {
		var spec string
		var rung int
		if err := rows.Scan(&spec, &rung); err != nil {
			e.t.Fatal(err)
		}
		preset, _, _ := strings.Cut(spec, "@")
		out = append(out, fmt.Sprintf("%s:%d", preset, rung))
	}
	return out
}

// blobOf is a file's content address.
func blobOf(t *testing.T, path string) string {
	t.Helper()
	sum := sha256.Sum256(must(os.ReadFile(path)))
	return layout.SHA256Name(sum[:])
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

type fixture struct {
	w, h, secs int
	rate       int // default 10
	audio      int
	subs       bool
	tone       int
}

// make writes an MKV: testsrc video, sine audio tracks tagged jpn/eng (the
// second titled "Commentary") and an English SRT track.
func (f fixture) make(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	out := filepath.Join(dir, "source.mkv")
	rate := cmp.Or(f.rate, 10)
	args := []string{"-v", "error", "-nostdin", "-f", "lavfi", "-i", fmt.Sprintf("testsrc=size=%dx%d:rate=%d:duration=%d", f.w, f.h, rate, f.secs)}
	for i := range f.audio {
		args = append(args, "-f", "lavfi", "-i", fmt.Sprintf("sine=frequency=%d:duration=%d", f.tone+220*i, f.secs))
	}
	if f.subs {
		srt := filepath.Join(dir, "en.srt")
		if err := os.WriteFile(srt, []byte("1\n00:00:00,500 --> 00:00:02,000\nHello\n\n2\n00:00:03,000 --> 00:00:05,000\nWorld\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		args = append(args, "-i", srt)
	}
	args = append(args, "-map", "0:v")
	for i := range f.audio {
		args = append(args, "-map", fmt.Sprintf("%d:a", i+1))
	}
	if f.subs {
		args = append(args, "-map", fmt.Sprintf("%d:s", f.audio+1), "-c:s", "srt", "-metadata:s:s:0", "language=eng")
	}
	args = append(args, "-c:v", "libx264", "-pix_fmt", "yuv444p", "-preset", "ultrafast", "-threads", "1", "-c:a", "aac")
	if f.audio > 0 {
		args = append(args, "-metadata:s:a:0", "language=jpn")
	}
	if f.audio > 1 {
		args = append(args, "-metadata:s:a:1", "language=eng", "-metadata:s:a:1", "title=Commentary")
	}
	args = append(args, "-y", out)
	if b, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
		t.Fatalf("fixture: %v: %s", err, b)
	}
	return out
}

type probed struct {
	Streams []struct {
		CodecType   string `json:"codec_type"`
		CodecName   string `json:"codec_name"`
		Width       int    `json:"width"`
		Height      int    `json:"height"`
		Disposition struct {
			Default int `json:"default"`
		} `json:"disposition"`
		Tags map[string]string `json:"tags"`
	} `json:"streams"`
	Packets []struct {
		Flags string `json:"flags"`
	} `json:"packets"`
	Format struct {
		Duration string `json:"duration"`
	} `json:"format"`
}

func ffprobe(t *testing.T, path string, extra ...string) probed {
	t.Helper()
	args := append([]string{"-v", "error", "-show_streams", "-show_format", "-of", "json"}, extra...)
	out, err := exec.Command("ffprobe", append(args, path)...).Output()
	if err != nil {
		t.Fatalf("ffprobe %s: %v", path, err)
	}
	var p probed
	if err := json.Unmarshal(out, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

func (p probed) duration(t *testing.T) float64 {
	var d float64
	if _, err := fmt.Sscan(p.Format.Duration, &d); err != nil {
		t.Fatalf("duration %q", p.Format.Duration)
	}
	return d
}

func (p probed) count(kind string) int {
	n := 0
	for _, s := range p.Streams {
		if s.CodecType == kind {
			n++
		}
	}
	return n
}

// checkByteRanges plays each segment as init + its byte range, and the whole
// track through a byte-range playlist, the way an HLS player reads it.
func checkByteRanges(t *testing.T, path string, segs []media.Segment, kind string, total float64) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	path = filepath.Join(t.TempDir(), "track.mp4") // the HLS demuxer checks segment extensions
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	init := segs[0].Offset
	end := init
	for i, s := range segs {
		if s.Offset != end {
			t.Fatalf("segment %d at %d, want %d", i, s.Offset, end)
		}
		end += s.Length
		if i < len(segs)-1 && math.Abs(s.Seconds-4) > 0.1 {
			t.Fatalf("segment %d is %.3fs; keyframes are every 4 s", i, s.Seconds)
		}
		one := filepath.Join(t.TempDir(), fmt.Sprintf("seg%d.mp4", i))
		if err := os.WriteFile(one, append(slices.Clone(data[:init]), data[s.Offset:s.Offset+s.Length]...), 0o600); err != nil {
			t.Fatal(err)
		}
		p := ffprobe(t, one, "-count_packets", "-show_entries", "packet=flags", "-read_intervals", "%+#1")
		if p.count(kind) != 1 || (kind == "video" && !strings.Contains(p.Packets[0].Flags, "K")) {
			t.Fatalf("segment %d does not start a decodable %s stream: %+v", i, kind, p)
		}
	}
	if end != int64(len(data)) {
		t.Fatalf("segments cover %d of %d bytes", end, len(data))
	}
	var pl strings.Builder
	fmt.Fprintf(&pl, "#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-TARGETDURATION:5\n#EXT-X-PLAYLIST-TYPE:VOD\n#EXT-X-MAP:URI=%q,BYTERANGE=\"%d@0\"\n", filepath.Base(path), init)
	for _, s := range segs {
		fmt.Fprintf(&pl, "#EXTINF:%.6f,\n#EXT-X-BYTERANGE:%d@%d\n%s\n", s.Seconds, s.Length, s.Offset, filepath.Base(path))
	}
	pl.WriteString("#EXT-X-ENDLIST\n")
	m3u8 := filepath.Join(filepath.Dir(path), "play.m3u8")
	if err := os.WriteFile(m3u8, []byte(pl.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	played := filepath.Join(t.TempDir(), "played.mp4")
	if b, err := exec.Command("ffmpeg", "-v", "error", "-nostdin", "-i", m3u8, "-c", "copy", "-y", played).CombinedOutput(); err != nil {
		t.Fatalf("play %s: %v: %s", m3u8, err, b)
	}
	if d := ffprobe(t, played).duration(t); math.Abs(d-total) > 0.5 {
		t.Fatalf("byte-range playback lasts %.2fs, want %ds", d, int(total))
	}
}

type packet struct {
	PTS   string `json:"pts_time"`
	Flags string `json:"flags"`
}

// keyframes are a rendition's keyframe times, ms.
func keyframes(t *testing.T, path string) []int {
	t.Helper()
	out, err := exec.Command("ffprobe", "-v", "error", "-select_streams", "v", "-show_entries", "packet=pts_time,flags", "-of", "json", path).Output()
	if err != nil {
		t.Fatalf("ffprobe %s: %v", path, err)
	}
	var p struct{ Packets []packet }
	if err := json.Unmarshal(out, &p); err != nil {
		t.Fatal(err)
	}
	var keys []int
	for _, pk := range p.Packets {
		var v float64
		fmt.Sscan(pk.PTS, &v)
		if strings.HasPrefix(pk.Flags, "K") {
			keys = append(keys, int(math.Round(v*1000)))
		}
	}
	slices.Sort(keys)
	return keys
}

// switchRungs decodes segments alternating between renditions, each as its
// rendition's init plus the segment (what a player switching levels at every
// segment appends): the frames' times must match playing either alone.
func switchRungs(t *testing.T, paths []string, segs [][]media.Segment) {
	t.Helper()
	dir := t.TempDir()
	play := func(pick func(j int) int) []string {
		var pts []string
		for j := range segs[0] {
			i := pick(j)
			data, err := os.ReadFile(paths[i])
			if err != nil {
				t.Fatal(err)
			}
			s := segs[i][j]
			seg := filepath.Join(dir, fmt.Sprintf("s%d-%d.mp4", j, i))
			if err := os.WriteFile(seg, append(slices.Clone(data[:segs[i][0].Offset]), data[s.Offset:s.Offset+s.Length]...), 0o600); err != nil {
				t.Fatal(err)
			}
			out, err := exec.Command("ffprobe", "-v", "error", "-show_entries", "frame=pts_time", "-of", "csv=p=0", seg).CombinedOutput()
			if err != nil || strings.Contains(string(out), "error") {
				t.Fatalf("segment %d of rendition %d: %v: %s", j, i, err, out)
			}
			pts = append(pts, strings.Fields(string(out))...)
		}
		return pts
	}
	switched := play(func(j int) int { return j % len(paths) })
	for i := range paths {
		if alone := play(func(int) int { return i }); !slices.Equal(switched, alone) {
			t.Fatalf("switching renditions plays %d frames, rendition %d alone %d", len(switched), i, len(alone))
		}
	}
}

// hlsOutputs are the item's outputs of preset hls from source.mkv as
// "{path}" in manifest order.
func outputPaths(m *media.Manifest, from, preset string) []string {
	var out []string
	for _, f := range m.Outputs(from, preset) {
		out = append(out, f.Path)
	}
	return out
}
