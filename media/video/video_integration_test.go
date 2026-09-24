package video_test

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	riverhelpers "github.com/open-rails/helpers/river"
	"github.com/riverqueue/river"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/internal/s3test"
	"github.com/open-rails/contentkit/media/video"
)

// CONTENTKIT_TEST_FFMPEG=1 (the CI video job) fails instead of skipping without ffmpeg.
func requireFFmpeg(t *testing.T) {
	t.Helper()
	for _, tool := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(tool); err != nil {
			if os.Getenv("CONTENTKIT_TEST_FFMPEG") != "" {
				t.Fatal(err)
			}
			t.Skip(err)
		}
	}
}

type fixture struct {
	w, h, secs int
	rate       int // default 10
	audio      int
	subs       bool
	tone       int
}

// make writes an MKV: testsrc video (odd sizes allowed), sine audio tracks
// tagged jpn/eng (the second titled "Commentary") and an English SRT track.
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

type grants struct{}

func (grants) CanUpload(context.Context, access.Actor, contentref.ContentRef) (media.UploadGrant, error) {
	return media.UploadGrant{Allowed: true, Exempt: true}, nil
}

type env struct {
	*s3test.Env
	store     media.Store
	kinds     *media.Registry
	manifests *media.Manifests
	uploads   *media.Uploads
	encoder   *video.Encoder
	ref       contentref.ContentRef
	slotJobs  []media.ProcessJob // poster frames handed to the image job
}

var admin = access.Actor{ID: "admin", Kind: "user"}

func newEnv(t *testing.T, store func(media.Store) media.Store, queue media.ProcessQueue) *env {
	t.Helper()
	requireFFmpeg(t)
	s3 := s3test.Open(t)
	e := &env{Env: s3, store: s3.Store}
	if store != nil {
		e.store = store(s3.Store)
	}
	var err error
	if e.kinds, err = media.NewRegistry(media.Kind{Name: "video", Versioned: true, Video: &media.Video{}, Types: []string{"video/x-matroska", "video/mp4"}}); err != nil {
		t.Fatal(err)
	}
	locker := s3test.Locker(t, e.store)
	if e.manifests, err = media.NewManifests(e.store, e.kinds, media.ManifestOptions{Locker: locker}); err != nil {
		t.Fatal(err)
	}
	if e.uploads, err = media.NewUploads(media.UploadOptions{Store: e.store, Kinds: e.kinds, Manifests: e.manifests,
		Authorizer: grants{}, Queue: queue}); err != nil {
		t.Fatal(err)
	}
	slots := queueFunc(func(_ context.Context, j media.ProcessJob) error { e.slotJobs = append(e.slotJobs, j); return nil })
	if e.encoder, err = video.New(video.Config{Store: e.store, Locker: locker, TempDir: t.TempDir(), Threads: 2, Slots: slots}); err != nil {
		t.Fatal(err)
	}
	e.ref = contentref.NewVersion(s3.Tenant, "video", "88", "v1")
	return e
}

func (e *env) item(t *testing.T) media.Item {
	t.Helper()
	item, err := e.kinds.Item(e.ref)
	if err != nil {
		t.Fatal(err)
	}
	return item
}

// commit uploads path as a new original and inserts or replaces file "source".
func (e *env) commit(t *testing.T, path, op string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	name := media.SHA256Name(sum[:])
	key, _ := e.item(t).Original(name)
	if _, err := e.store.Put(context.Background(), key, bytes.NewReader(body), int64(len(body)),
		media.PutOptions{ContentType: map[bool]string{true: "video/mp4", false: "video/x-matroska"}[filepath.Ext(path) == ".mp4"], ChecksumSHA256: sum[:]}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.uploads.Commit(context.Background(), admin, e.ref, []media.Op{{Op: op, Name: "source", Original: name}}); err != nil {
		t.Fatal(err)
	}
	return name
}

func (e *env) manifest(t *testing.T) (*media.Manifest, string) {
	t.Helper()
	m, etag, err := e.manifests.Get(context.Background(), e.ref)
	if err != nil {
		t.Fatal(err)
	}
	return m, etag
}

func (e *env) encode(t *testing.T) {
	t.Helper()
	if err := e.encoder.Encode(context.Background(), video.Job{Ref: e.ref, Versioned: true}, nil); err != nil {
		t.Fatal(err)
	}
}

func (e *env) blob(t *testing.T, name string) string {
	t.Helper()
	key, _ := e.item(t).Blob(name)
	rc, _, err := e.store.Get(context.Background(), key, media.GetOptions{})
	if err != nil {
		t.Fatalf("blob %s: %v", name, err)
	}
	defer rc.Close()
	path := filepath.Join(t.TempDir(), name)
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(f, rc); err != nil {
		t.Fatal(err)
	}
	f.Close()
	return path
}

func (e *env) blobs(t *testing.T) []string {
	t.Helper()
	var out []string
	for obj, err := range e.store.List(context.Background(), e.item(t).BlobsPrefix()) {
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, obj.Key)
	}
	slices.Sort(out)
	return out
}

type probed struct {
	Streams []struct {
		CodecType   string `json:"codec_type"`
		CodecName   string `json:"codec_name"`
		Width       int    `json:"width"`
		Height      int    `json:"height"`
		NbPackets   string `json:"nb_read_packets"`
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
// rendition through a byte-range playlist, the way an HLS player reads it.
func checkByteRanges(t *testing.T, path string, segs []media.Segment, kind string, total float64) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	path = filepath.Join(t.TempDir(), "rendition.mp4") // the HLS demuxer checks segment extensions
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
		t.Fatalf("byte-range playback lasts %.2fs, want %.0fs", d, total)
	}
}

func TestEncoderRequiresLockerWithoutConditionalPut(t *testing.T) {
	env := s3test.Open(t).WithoutConditionalPut(t)
	if _, err := video.New(video.Config{Store: env.Store}); err == nil || !strings.Contains(err.Error(), "Config.Locker is required") {
		t.Fatalf("no conditional PUT and no Locker: %v", err)
	}
}

func TestLadderFromMultiTrackSource(t *testing.T) {
	e := newEnv(t, nil, nil)
	src := fixture{w: 1281, h: 721, secs: 9, audio: 2, subs: true, tone: 440}.make(t)
	source := e.commit(t, src, media.OpInsert)
	e.encode(t)

	m, _ := e.manifest(t)
	f := m.Files[m.File("source")]
	h := f.HLS
	if h == nil || h.Source != source || h.Spec != video.Spec(nil) {
		t.Fatalf("hls %+v", h)
	}
	var heights []int
	for _, r := range h.Video {
		heights = append(heights, r.Rung)
		if r.Height != r.Rung || r.Width%2 != 0 || !strings.HasPrefix(r.Codecs, "avc1.64") || r.Bandwidth < r.Average || r.Average <= 0 {
			t.Fatalf("rendition %+v", r)
		}
		checkByteRanges(t, e.blob(t, r.Blob), r.Segments, "video", 9)
	}
	if !slices.Equal(heights, []int{720, 480}) {
		t.Fatalf("ladder %v, want [720 480] (capped at the 721-line source)", heights)
	}
	if len(h.Audio) != 2 || h.Audio[0].Lang != "ja" || h.Audio[0].Label != "Japanese" || !h.Audio[0].Default ||
		h.Audio[1].Lang != "en" || h.Audio[1].Label != "Commentary" || h.Audio[1].Default {
		t.Fatalf("audio %+v", h.Audio)
	}
	for _, a := range h.Audio {
		checkByteRanges(t, e.blob(t, a.Blob), a.Segments, "audio", 9)
	}
	if len(h.Subs) != 1 || h.Subs[0].Lang != "en" || h.Subs[0].Label != "English" {
		t.Fatalf("subs %+v", h.Subs)
	}
	if vtt, _ := os.ReadFile(e.blob(t, h.Subs[0].Blob)); !bytes.HasPrefix(vtt, []byte("WEBVTT")) || !bytes.Contains(vtt, []byte("World")) {
		t.Fatalf("vtt %q", vtt)
	}
	sp := h.Sprite
	if sp == nil || sp.Cols != 10 || sp.Rows != 10 || math.Abs(sp.Interval-0.09) > 0.001 {
		t.Fatalf("sprite %+v", sp)
	}
	if p := ffprobe(t, e.blob(t, sp.Blob)); p.Streams[0].Width != 1600 || p.Streams[0].Height != 900 {
		t.Fatalf("sprite image %+v", p.Streams)
	}
	if d, _ := f.Meta["duration"].(float64); math.Abs(d-9) > 0.1 {
		t.Fatalf("meta %v", f.Meta)
	}

	for _, height := range heights {
		d, ok := m.Downloads[video.DownloadKey("source", height)]
		if !ok || d.Type != "video/mp4" || d.Spec != video.Spec(nil) || d.Inputs != source || d.Size <= 0 {
			t.Fatalf("download %dp: %+v", height, d)
		}
		p := ffprobe(t, e.blob(t, d.Blob))
		if p.count("video") != 1 || p.count("audio") != 2 || p.count("subtitle") != 1 || math.Abs(p.duration(t)-9) > 0.5 {
			t.Fatalf("download %dp streams %+v", height, p)
		}
		for _, s := range p.Streams {
			if s.CodecType == "video" && s.Height != height {
				t.Fatalf("download %dp has height %d", height, s.Height)
			}
			if s.CodecType == "audio" && s.Disposition.Default == 1 && s.Tags["language"] != "jpn" {
				t.Fatalf("download default audio %+v", s)
			}
		}
	}
}

func TestKindLadder(t *testing.T) {
	e := newEnv(t, nil, nil)
	e.commit(t, fixture{w: 640, h: 361, secs: 3, tone: 440}.make(t), media.OpInsert)
	encode := func(ladder []int, want ...int) {
		t.Helper()
		if err := e.encoder.Encode(context.Background(), video.Job{Ref: e.ref, Versioned: true, Video: media.Video{Ladder: ladder}}, nil); err != nil {
			t.Fatal(err)
		}
		m, _ := e.manifest(t)
		h := m.Files[0].HLS
		var heights, downloads []int
		for _, r := range h.Video {
			heights = append(heights, r.Rung)
		}
		for _, height := range want {
			if d, ok := m.Downloads[video.DownloadKey("source", height)]; ok && d.Spec == video.Spec(ladder) {
				downloads = append(downloads, height)
			}
		}
		if h.Spec != video.Spec(ladder) || !slices.Equal(heights, want) || !slices.Equal(downloads, want) || len(m.Downloads) != len(want) {
			t.Fatalf("ladder %v: hls %v %s, downloads %v", ladder, heights, h.Spec, m.Downloads)
		}
	}
	encode([]int{1080, 360, 240}, 360, 240)
	encode(nil, 360) // the default ladder is another recipe: re-encoded
}

func TestReplacedSourceKeepsPreviousHLSUntilPromotion(t *testing.T) {
	e := newEnv(t, nil, nil)
	ctx := context.Background()
	a := e.commit(t, fixture{w: 640, h: 361, secs: 5, audio: 1, tone: 440}.make(t), media.OpInsert)
	e.encode(t)
	m, _ := e.manifest(t)
	prev := m.Files[0].HLS
	if prev == nil || prev.Source != a || prev.Video[0].Rung != 360 {
		t.Fatalf("hls %+v", prev)
	}

	readable := func(when string) {
		t.Helper()
		m, _ := e.manifest(t)
		if h := m.Files[0].HLS; h == nil || h.Source != a {
			t.Fatalf("%s: previous hls gone: %+v", when, h)
		}
		for _, blob := range []string{prev.Video[0].Blob, prev.Audio[0].Blob} {
			key, _ := e.item(t).Blob(blob)
			if _, err := e.store.Head(ctx, key); err != nil {
				t.Fatalf("%s: %s: %v", when, blob, err)
			}
		}
	}
	b := e.commit(t, fixture{w: 640, h: 361, secs: 5, audio: 1, tone: 550}.make(t), media.OpReplace)
	readable("after replace")

	// A second replacement lands while b encodes: b's result must be dropped.
	cSrc := fixture{w: 640, h: 361, secs: 5, audio: 1, tone: 660}.make(t)
	var c string
	restore := video.SetBeforePromote(func() {
		readable("before promotion")
		c = e.commit(t, cSrc, media.OpReplace)
	})
	e.encode(t)
	restore()
	readable("after the stale encode")
	if c == "" || c == b {
		t.Fatal("replacement did not run")
	}

	e.encode(t)
	m, _ = e.manifest(t)
	if h := m.Files[0].HLS; h == nil || h.Source != c || h.Audio[0].Blob == prev.Audio[0].Blob {
		t.Fatalf("hls after promotion %+v", h)
	}
	if d := m.Downloads[video.DownloadKey("source", 360)]; d.Inputs != c {
		t.Fatalf("download %+v", d)
	}
}

// failManifest fails manifest writes while armed, as a crash between the blob
// uploads and promotion would.
type failManifest struct {
	media.Store
	armed atomic.Bool
}

func (s *failManifest) Put(ctx context.Context, key string, body io.Reader, size int64, o media.PutOptions) (media.Object, error) {
	if s.armed.Load() && strings.Contains(key, "/manifests/") {
		return media.Object{}, errors.New("injected manifest failure")
	}
	return s.Store.Put(ctx, key, body, size, o)
}

func TestRetriesAreIdempotent(t *testing.T) {
	defer video.SetMultipart(64<<10, 5<<20)() // blobs over 64 KiB take the multipart path
	var fail *failManifest
	e := newEnv(t, func(s media.Store) media.Store { fail = &failManifest{Store: s}; return fail }, nil)
	e.commit(t, fixture{w: 853, h: 481, secs: 5, audio: 1, subs: true, tone: 440}.make(t), media.OpInsert)

	fail.armed.Store(true)
	if err := e.encoder.Encode(context.Background(), video.Job{Ref: e.ref, Versioned: true}, nil); err == nil {
		t.Fatal("encode with a failing manifest write succeeded")
	}
	fail.armed.Store(false)
	first := e.blobs(t)
	if m, _ := e.manifest(t); m.Files[0].HLS != nil {
		t.Fatal("hls recorded despite the failed promotion")
	}

	e.encode(t)
	if again := e.blobs(t); !slices.Equal(first, again) {
		t.Fatalf("retry wrote new blobs:\n%v\n%v", first, again)
	}
	m, etag := e.manifest(t)
	for _, name := range m.Blobs() {
		key, _ := e.item(t).Blob(name)
		if !slices.Contains(first, key) {
			t.Fatalf("manifest references %s, not written by the first attempt", name)
		}
	}
	if m.Files[0].HLS.Video[0].Rung != 480 || len(m.Downloads) != 1 {
		t.Fatalf("manifest %+v", m)
	}

	e.encode(t)
	if _, after := e.manifest(t); after != etag {
		t.Fatal("encoding a fresh manifest rewrote it")
	}
}

// The host flow: an upload commit enqueues media's process job in the host
// schema, whose video processor inserts into media_worker, where the worker
// encodes it.
func TestWorkerEncodesCommittedUploads(t *testing.T) {
	requireFFmpeg(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	pool := pgtest.Pool(t, nil)
	if err := video.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "DELETE FROM "+video.Schema+".river_job"); err != nil {
		t.Fatal(err)
	}
	var jobs *media.Jobs
	e := newEnv(t, nil, queueFunc(func(ctx context.Context, j media.ProcessJob) error { return jobs.Enqueue(ctx, j) }))
	enq, err := video.NewEnqueuer(pool, e.kinds)
	if err != nil {
		t.Fatal(err)
	}
	if jobs, err = media.NewJobs(media.JobsConfig{Store: e.store, Kinds: e.kinds}); err != nil {
		t.Fatal(err)
	}
	if err := jobs.AddProcessor(enq.Processor()); err != nil {
		t.Fatal(err)
	}
	hostSchema := pgtest.EmptySchema(t, ctx, pool)
	if err := riverhelpers.ApplyMigrations(ctx, pool, hostSchema); err != nil {
		t.Fatal(err)
	}
	host, err := riverhelpers.New(ctx, pool, &river.Config{Schema: hostSchema, FetchPollInterval: 100 * time.Millisecond}, jobs.RiverJobs())
	if err != nil {
		t.Fatal(err)
	}
	if err := host.Start(ctx); err != nil {
		t.Fatal(err)
	}

	wc := video.WorkerConfig{Encoder: e.encoder, Pool: pool, Timeout: time.Hour}
	contribution, err := video.Contribution(wc)
	if err != nil {
		t.Fatal(err)
	}
	cfg := video.ClientConfig(wc)
	cfg.FetchPollInterval = 100 * time.Millisecond
	worker, err := riverhelpers.New(ctx, pool, cfg, contribution)
	if err != nil {
		t.Fatal(err)
	}
	done, stop := worker.Subscribe(river.EventKindJobCompleted, river.EventKindJobFailed, river.EventKindJobCancelled)
	defer stop()
	if err := worker.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() {
		stopCtx, c := context.WithTimeout(context.Background(), 30*time.Second)
		defer c()
		_ = worker.StopAndCancel(stopCtx)
		_ = host.StopAndCancel(stopCtx)
	}()

	source := e.commit(t, fixture{w: 640, h: 361, secs: 5, audio: 1, tone: 440}.make(t), media.OpInsert)
	if err := enq.Enqueue(ctx, media.ProcessJob{Ref: e.ref}); err != nil { // a duplicate: serialized, then a no-op
		t.Fatal(err)
	}
	for completed := 0; completed < 2; {
		select {
		case ev := <-done:
			if ev.Kind != river.EventKindJobCompleted {
				t.Fatalf("job %s: %+v", ev.Kind, ev.Job.Errors)
			}
			completed++
		case <-ctx.Done():
			t.Fatal("jobs did not complete")
		}
	}
	m, _ := e.manifest(t)
	if h := m.Files[0].HLS; h == nil || h.Source != source || len(m.Downloads) != 1 {
		t.Fatalf("manifest after worker: %+v", m)
	}
}

type queueFunc func(context.Context, media.ProcessJob) error

func (f queueFunc) Enqueue(ctx context.Context, j media.ProcessJob) error { return f(ctx, j) }
