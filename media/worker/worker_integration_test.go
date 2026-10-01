package worker_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	stdimage "image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	riverhelpers "github.com/open-rails/helpers/river"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/internal/tcpproxy"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/internal/s3test"
	mediaS3 "github.com/open-rails/contentkit/media/s3"
	"github.com/open-rails/contentkit/media/token"
	"github.com/open-rails/contentkit/media/worker"
	"github.com/open-rails/contentkit/media/workqueue"
)

type allow struct{}

func (allow) CanUpload(context.Context, access.Actor, media.UploadTarget) (media.UploadGrant, error) {
	return media.UploadGrant{Allowed: true}, nil
}

var alice = access.Actor{ID: "alice", Kind: "user"}

var pngs = []string{"image/png"}

// registry is the hosts' registry: paged galleries with a thumb and a
// cover, clips with an HLS ladder and a poster grabbed from the video.
func registry(ns string, hooks media.Hooks) media.Config {
	return media.Config{Namespace: ns, BaseURL: "https://media.example", Hooks: hooks, Kinds: []media.Kind{
		{Name: "gallery", KeepOriginals: true,
			Uploads: []media.Upload{{Path: "originals/{name}", Types: pngs, MaxBytes: 10 << 20, Pages: true}, {Path: "cover", Types: pngs, MaxBytes: 10 << 20}},
			Private: []media.Private{{Name: "thumb", From: "originals/{name}", To: "thumb/{name}.webp", Image: &media.Image{Width: 100, Height: 150, Fit: media.FitCover}}},
			Public:  []media.Public{{Name: "cover", From: "cover", To: "cover-{w}.webp", Widths: []int{150, 300}, Image: media.Image{Aspect: media.Ratio("3:1")}}},
		},
		{Name: "clip", KeepOriginals: true,
			Uploads: []media.Upload{{Path: "source", Types: []string{"video/mp4"}, MaxBytes: 1 << 30}, {Path: "poster", Types: pngs, MaxBytes: 10 << 20, Frames: "source"}},
			Private: []media.Private{{Name: "hls", From: "source", To: "hls/", HLS: &media.HLS{Ladder: []int{240}}}},
			Public:  []media.Public{{Name: "poster", From: "poster", To: "poster-{w}.webp", Widths: []int{160}}},
		},
	}}
}

// host is a host that only presigns, commits and reads, with the worker
// running beside it on the same database and bucket.
type host struct {
	*s3test.Env
	pool    *pgxpool.Pool
	reg     *media.Registry
	ms      *media.Manifests
	uploads *media.Uploads
	queue   *workqueue.Queue
	worker  *worker.Worker
	jobs    *media.Jobs
	hidden  sync.Map // item id → true: hidden from anonymous viewers

	mu      sync.Mutex
	settled map[string][]media.Readiness // Hooks.ItemReady, by ref
	purged  []string                     // Hooks.PurgePublic
	schema  string                       // the host's River schema
	workers string                       // the host's worker schema
}

// Resolve hides the ids in h.hidden from anonymous viewers.
func (h *host) Resolve(_ context.Context, refs []contentref.ContentRef, a access.Actor) (map[contentref.ContentKey]access.Resolution, error) {
	out := map[contentref.ContentKey]access.Resolution{}
	for _, ref := range refs {
		_, hidden := h.hidden.Load(ref.ContentID)
		out[ref.Key()] = access.Resolution{Visible: !hidden || !a.Anonymous, Accessible: true, Editor: a.ID == alice.ID}
	}
	return out, nil
}

// lastSettled is the latest ItemReady report for ref.
func (h *host) lastSettled(ref contentref.ContentRef) (media.Readiness, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	all := h.settled[ref.String()]
	if len(all) == 0 {
		return media.Readiness{}, false
	}
	return all[len(all)-1], true
}

func newHost(t *testing.T, riverHooks ...rivertype.Hook) *host {
	t.Helper()
	return newHostOn(t, nil, riverHooks...)
}

// imageTimeout overrides the worker's image job timeout in newHostOn (0: default).
var imageTimeout time.Duration

// newHostOn is newHost with the worker on workerStore(env) instead of env.Store.
func newHostOn(t *testing.T, workerStore func(*s3test.Env) media.Store, riverHooks ...rivertype.Hook) *host {
	t.Helper()
	env := s3test.Open(t)
	if !env.Store.Capabilities().ConditionalPut {
		t.Skip("backend lacks conditional PUT")
	}
	ctx := context.Background()
	pool := pgtest.Pool(t, nil)
	h := &host{Env: env, pool: pool, settled: map[string][]media.Readiness{}}
	var err error
	h.reg, err = media.NewRegistry(registry(env.Tenant, media.Hooks{Resolver: h, CanUpload: allow{},
		PurgePublic: func(_ context.Context, urls []string) {
			h.mu.Lock()
			h.purged = append(h.purged, urls...)
			h.mu.Unlock()
		},
		ItemReady: func(ctx context.Context, tx pgx.Tx, ref contentref.ContentRef, r media.Readiness) error {
			if r.Ready() {
				if err := h.jobs.ExposeTx(ctx, tx, ref); err != nil {
					return err
				}
			}
			h.mu.Lock()
			h.settled[ref.String()] = append(h.settled[ref.String()], r)
			h.mu.Unlock()
			return nil
		}}))
	if err != nil {
		t.Fatal(err)
	}
	h.workers = workerSchema(t, pool)
	if err := workqueue.Migrate(ctx, pool, h.workers); err != nil {
		t.Fatal(err)
	}
	if h.queue, err = workqueue.New(pool, h.reg, h.workers); err != nil {
		t.Fatal(err)
	}
	// The host's River: readiness, purges and sweeps the worker hands back run here.
	h.schema = pgtest.EmptySchema(t, ctx, pool)
	if err := riverhelpers.ApplyMigrations(ctx, pool, h.schema); err != nil {
		t.Fatal(err)
	}
	if h.jobs, err = media.NewJobs(media.JobsConfig{Store: env.Store, Registry: h.reg, Locker: s3test.Locker(t, env.Store), Pool: pool, Processes: h.queue}); err != nil {
		t.Fatal(err)
	}
	client, err := riverhelpers.New(ctx, pool, &river.Config{Schema: h.schema, FetchPollInterval: 100 * time.Millisecond,
		FetchCooldown: 50 * time.Millisecond}, h.jobs.RiverJobs())
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = client.StopAndCancel(ctx)
	})
	h.ms = h.jobs.Manifests()
	if h.uploads, err = media.NewUploads(media.UploadOptions{Store: env.Store, Manifests: h.ms, Queue: h.queue}); err != nil {
		t.Fatal(err)
	}

	// The worker, built from the same registry.
	var store media.Store = env.Store
	if workerStore != nil {
		store = workerStore(env)
	}
	h.worker, err = worker.New(ctx, worker.Config{Pool: pool, Schema: h.workers, Store: store, Kinds: h.reg, HostSchema: h.schema,
		TempDir: t.TempDir(), Threads: 2, RiverHooks: riverHooks, ImageTimeout: imageTimeout})
	if err != nil {
		t.Fatal(err)
	}
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- h.worker.Run(runCtx) }()
	t.Cleanup(func() {
		stop()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	return h
}

// upload presigns and PUTs body like a browser and commits it at path with
// extra ops.
func (h *host) upload(t *testing.T, ref contentref.ContentRef, path, typ string, body []byte, extra ...media.Op) string {
	t.Helper()
	p, blob := h.stage(t, ref, path, typ, body)
	h.commit(t, ref, append([]media.Op{{Op: media.OpPut, Path: p, Blob: blob}}, extra...)...)
	return p
}

// stage presigns and PUTs body like a browser, returning the path and blob
// to commit.
func (h *host) stage(t *testing.T, ref contentref.ContentRef, path, typ string, body []byte) (string, string) {
	t.Helper()
	sum := sha256.Sum256(body)
	p, err := h.uploads.Presign(context.Background(), alice, media.PresignRequest{Ref: ref, Path: path, Type: typ, Size: int64(len(body)), SHA256: sum[:]})
	if err != nil {
		t.Fatal(err)
	}
	if p.Put != nil {
		req, _ := http.NewRequest(p.Put.Method, p.Put.URL, bytes.NewReader(body))
		req.Header = p.Put.Header.Clone()
		req.ContentLength = int64(len(body))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("put: %d", resp.StatusCode)
		}
	}
	return p.Path, p.Blob
}

func (h *host) commit(t *testing.T, ref contentref.ContentRef, ops ...media.Op) {
	t.Helper()
	if _, err := h.uploads.Commit(context.Background(), alice, ref, ops); err != nil {
		t.Fatal(err)
	}
}

func (h *host) file(ref contentref.ContentRef, path string) (media.File, bool) {
	m, _, err := h.ms.Get(context.Background(), ref)
	if err != nil {
		return media.File{}, false
	}
	return m.Get(path)
}

func (h *host) has(ref contentref.ContentRef, path string) func() bool {
	return func() bool { _, ok := h.file(ref, path); return ok }
}

func (h *host) exists(t *testing.T, key string) bool {
	t.Helper()
	_, err := h.Store.Head(context.Background(), key)
	if err != nil && !errors.Is(err, media.ErrNotFound) {
		t.Fatal(err)
	}
	return err == nil
}

func (h *host) read(t *testing.T, ref contentref.ContentRef, actor access.Actor, editor bool) []media.FileInfo {
	t.Helper()
	r, err := media.NewReader(media.ReaderOptions{Manifests: h.ms,
		Delivery: media.Delivery{Mode: media.DeliverURL, SigningKey: token.Key{ID: "k", Secret: bytes.Repeat([]byte("k"), 32)}}})
	if err != nil {
		t.Fatal(err)
	}
	res, err := r.Read(context.Background(), ref, actor, media.ReadOptions{Editor: editor})
	if err != nil {
		t.Fatal(err)
	}
	return res.Files
}

// workerSchema is a fresh worker schema name, dropped after the test: each
// host has its own, as hosts sharing a database must.
func workerSchema(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	s := "ck_mw_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+s+" CASCADE") })
	return s
}

func eventually(t *testing.T, what string, within time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func pngImage(t *testing.T, w, h int, seed uint8) []byte {
	t.Helper()
	img := stdimage.NewRGBA(stdimage.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.RGBA{uint8(x) + seed, uint8(y) * seed, seed, 255})
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func newID() string { return uuid.Must(uuid.NewV7()).String() }

func (h *host) ref(t *testing.T, kind string) contentref.ContentRef {
	t.Helper()
	ref, err := h.reg.Ref(kind, newID())
	if err != nil {
		t.Fatal(err)
	}
	return ref
}

func TestOneShotWorkerStopsAfterOneJob(t *testing.T) {
	for _, tc := range []struct{ workerQueue, jobQueue string }{
		{workqueue.VideoLightQueue, workqueue.VideoLightQueue},
		{workqueue.VideoLightQueue, workqueue.ImageQueue},
		{workqueue.VideoLightQueue, workqueue.AudioQueue},
		{workqueue.VideoEncodeQueue, workqueue.VideoEncodeQueue},
	} {
		t.Run(tc.jobQueue, func(t *testing.T) {
			env := s3test.Open(t)
			pool := pgtest.Pool(t, nil)
			schema := workerSchema(t, pool)
			if err := workqueue.Migrate(context.Background(), pool, schema); err != nil {
				t.Fatal(err)
			}
			scratch := t.TempDir()
			active := filepath.Join(scratch, "ck-video-active")
			if err := os.Mkdir(active, 0o700); err != nil {
				t.Fatal(err)
			}
			reg, err := media.NewRegistry(registry(env.Tenant, media.Hooks{}))
			if err != nil {
				t.Fatal(err)
			}
			w, err := worker.New(context.Background(), worker.Config{Pool: pool, Schema: schema, Store: env.Store, Kinds: reg,
				Queue: tc.workerQueue, TempDir: scratch, Threads: 2})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(active); err != nil {
				t.Fatalf("one-shot worker swept another process's scratch: %v", err)
			}
			ref, _ := reg.Ref("clip", newID())
			var args river.JobArgs = workqueue.VideoPlanArgs{Ref: ref}
			switch tc.jobQueue {
			case workqueue.VideoEncodeQueue:
				args = workqueue.VideoChunkArgs{Ref: ref, RunID: uuid.NewString(), Index: 0}
			case workqueue.ImageQueue:
				args = workqueue.ImageArgs{Ref: contentref.New(env.Tenant, "missing", newID())}
			case workqueue.AudioQueue:
				args = workqueue.AudioArgs{Ref: ref}
			}
			var ids []int64
			for range 2 {
				result, err := w.Client().Insert(context.Background(), args, &river.InsertOpts{Queue: tc.jobQueue})
				if err != nil {
					t.Fatal(err)
				}
				ids = append(ids, result.Job.ID)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if err := w.Run(ctx); err != nil {
				t.Fatal(err)
			}
			if ctx.Err() != nil {
				t.Fatal("worker did not exit after its first job")
			}
			var terminal int
			if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM "+pgx.Identifier{schema, "river_job"}.Sanitize()+
				" WHERE id = ANY($1) AND state IN ('completed', 'cancelled', 'discarded')", ids).Scan(&terminal); err != nil {
				t.Fatal(err)
			}
			if terminal != 1 {
				t.Fatalf("expected one processed job, got %d", terminal)
			}
		})
	}
}

// The host commits; the worker renders the pages' thumbs and the public
// cover, the host's jobs report the item ready (Hooks.ItemReady, in a host
// transaction) and purge the cover's URLs (Hooks.PurgePublic).
func TestWorkerProcessesImagesAndRelays(t *testing.T) {
	h := newHost(t)
	ref := h.ref(t, "gallery")
	p, blob := h.stage(t, ref, "originals/001.png", "image/png", pngImage(t, 300, 450, 1))
	h.upload(t, ref, "cover", "image/png", pngImage(t, 600, 200, 3), media.Op{Op: media.OpPut, Path: p, Blob: blob})
	eventually(t, "ItemReady", time.Minute, func() bool { r, ok := h.lastSettled(ref); return ok && r.Ready() })
	if th, ok := h.file(ref, "thumb/001.webp"); !ok || th.W != 100 || th.H != 150 || th.FP == "" {
		t.Fatalf("thumb %+v", th)
	}
	item, _ := h.reg.Item(ref)
	key, _ := item.Public("cover-300.webp")
	if !h.exists(t, key) {
		t.Fatal("no public cover")
	}
	eventually(t, "the cover purged", 30*time.Second, func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		return slices.Contains(h.purged, h.reg.PublicURL(ref, "cover-300.webp"))
	})
	var n int
	if err := h.pool.QueryRow(context.Background(), "SELECT count(*) FROM "+h.schema+".river_job WHERE kind = 'contentkit_media_expose'").Scan(&n); err != nil || n == 0 {
		t.Fatalf("no Expose enqueued in the host schema: %d %v", n, err)
	}
}

// A video is encoded to the HLS ladder; its poster is grabbed from a frame
// and rendered public; the item is ready only once both are done.
func TestWorkerEncodesVideoAndPoster(t *testing.T) {
	if os.Getenv("CONTENTKIT_TEST_FFMPEG") == "" {
		t.Skip("CONTENTKIT_TEST_FFMPEG not set")
	}
	h := newHost(t)
	src := filepath.Join(t.TempDir(), "clip.mp4")
	if out, err := exec.Command("ffmpeg", "-v", "error", "-f", "lavfi", "-i", "testsrc=duration=3:size=426x240:rate=24",
		"-f", "lavfi", "-i", "sine=duration=3", "-shortest", "-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:a", "aac", src).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v %s", err, out)
	}
	body, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	ref := h.ref(t, "clip")
	h.upload(t, ref, "source.mp4", "video/mp4", body, media.Op{Op: media.OpFrame, Path: "poster", Auto: true})
	eventually(t, "ItemReady", 3*time.Minute, func() bool { r, ok := h.lastSettled(ref); return ok && r.Ready() })
	m, _, err := h.ms.Get(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	var video, audio bool
	for _, f := range m.Files {
		if f.Track != nil {
			video = video || f.Track.Kind == media.TrackVideo && f.Track.Index != ""
			audio = audio || f.Track.Kind == media.TrackAudio
		}
	}
	if !video || !audio {
		t.Fatalf("ladder %+v", m.Files)
	}
	if p, ok := m.Get("poster.png"); !ok || p.Blob == "" || p.Frame == nil || p.Frame.Of == "" {
		t.Fatalf("poster %+v", p)
	}
	item, _ := h.reg.Item(ref)
	key, _ := item.Public("poster-160.webp")
	if !h.exists(t, key) {
		t.Fatal("no public poster")
	}
	// The read API plays the producer's ladder.
	r, err := media.NewReader(media.ReaderOptions{Manifests: h.ms,
		Delivery: media.Delivery{Mode: media.DeliverURL, SigningKey: token.Key{ID: "k", Secret: bytes.Repeat([]byte("k"), 32)}}})
	if err != nil {
		t.Fatal(err)
	}
	g, err := r.Grant(context.Background(), ref, alice)
	if err != nil {
		t.Fatal(err)
	}
	master, err := g.MasterPlaylist("hls/", media.MasterOptions{})
	if err != nil || !strings.Contains(string(master), "#EXT-X-STREAM-INF:") || !strings.Contains(string(master), "TYPE=AUDIO") {
		t.Fatalf("master playlist %s %v", master, err)
	}
	for _, f := range m.Files {
		if f.Track != nil && f.Track.Kind == media.TrackVideo {
			pl, err := g.MediaPlaylist(context.Background(), f.Path)
			if err != nil || !strings.Contains(string(pl), "#EXT-X-BYTERANGE:") || !strings.Contains(string(pl), "#EXT-X-ENDLIST") {
				t.Fatalf("media playlist of %s: %s %v", f.Path, pl, err)
			}
		}
	}
	if vtt, err := g.SpriteVTT(context.Background(), "hls/"); err != nil || !strings.Contains(string(vtt), "#xywh=") {
		t.Fatalf("sprite %s %v", vtt, err)
	}
}

// ItemReady reports a failure while an upload cannot be processed (viewers
// never see it; its editor does), and ready once it is removed.
func TestItemReadyAfterProcessing(t *testing.T) {
	h := newHost(t)
	ref := h.ref(t, "gallery")
	h.upload(t, ref, "originals/001.png", "image/png", pngImage(t, 300, 450, 21))
	h.upload(t, ref, "originals/002.png", "image/png", []byte("not a png at all"))
	eventually(t, "the failure reported", time.Minute, func() bool { r, ok := h.lastSettled(ref); return ok && r.State == media.StateFailed })
	if r, _ := h.lastSettled(ref); len(r.Failed) != 1 || r.Failed[0] != "originals/002.png" || len(r.Processing) != 0 {
		t.Fatalf("readiness: %+v", r)
	}
	if got := h.read(t, ref, access.Actor{Anonymous: true}, false); len(got) != 1 || got[0].Path != "thumb/001.webp" {
		t.Fatalf("a viewer reads %+v; want only the processed page's thumb", got)
	}
	if got := h.read(t, ref, alice, true); len(got) != 3 || got[1].Failed == nil {
		t.Fatalf("the editor reads %+v; want both uploads, the failure named", got)
	}
	h.commit(t, ref, media.Op{Op: media.OpRemove, Path: "originals/002.png"})
	eventually(t, "ready after the removal", time.Minute, func() bool { r, ok := h.lastSettled(ref); return ok && r.Ready() })
}

// An edit that lands as the image job finishes, while River still has it
// running, is absorbed by a follow-up job and still rendered.
func TestWorkerRendersAnEditLandingAsTheJobFinishes(t *testing.T) {
	type target struct {
		h   *host
		ref contentref.ContentRef
	}
	var tg atomic.Pointer[target]
	var once sync.Once
	edited := make(chan error, 1)
	late := &media.Edit{Crop: &media.Crop{X: 0, Y: 0, W: 300, H: 100}}
	h := newHost(t, river.HookWorkEndFunc(func(ctx context.Context, job *rivertype.JobRow, err error) error {
		if cur := tg.Load(); cur != nil && job.Kind == (workqueue.ImageArgs{}).Kind() && err == nil {
			once.Do(func() {
				_, cerr := cur.h.uploads.Commit(context.Background(), alice, cur.ref, []media.Op{{Op: media.OpEdit, Path: "cover.png", Edit: late}})
				edited <- cerr
			})
		}
		return err
	}))
	ref := h.ref(t, "gallery")
	tg.Store(&target{h: h, ref: ref})
	h.upload(t, ref, "cover", "image/png", pngImage(t, 300, 400, 13))
	select {
	case err := <-edited:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Minute):
		t.Fatal("the image job did not run")
	}
	eventually(t, "the late edit rendered", time.Minute, func() bool {
		f, ok := h.file(ref, "cover.png")
		return ok && f.Edit.Hash() == late.Hash() && f.Pending == nil
	})
}

// ProcessOnUpload: an unattached upload is processed before it is attached
// and viewers do not see it until then; attaching does not reprocess.
func TestUnattachedThenAttached(t *testing.T) {
	h := newHost(t)
	ref := h.ref(t, "gallery")
	p, blob := h.stage(t, ref, "originals/001.png", "image/png", pngImage(t, 300, 450, 7))
	h.commit(t, ref, media.Op{Op: media.OpPut, Path: p, Blob: blob, Unattached: true})
	eventually(t, "the unattached page processed", time.Minute, h.has(ref, "thumb/001.webp"))
	if got := h.read(t, ref, access.Actor{Anonymous: true}, false); len(got) != 0 {
		t.Fatalf("a viewer sees an unattached upload's outputs: %+v", got)
	}
	before, _ := h.file(ref, "thumb/001.webp")
	h.commit(t, ref, media.Op{Op: media.OpAttach, Path: p})
	h.commit(t, ref, media.Op{Op: media.OpAttach, Path: p}) // idempotent
	if after, _ := h.file(ref, "thumb/001.webp"); after.Blob != before.Blob {
		t.Fatal("attach reprocessed")
	}
	if got := h.read(t, ref, access.Actor{Anonymous: true}, false); len(got) != 1 {
		t.Fatalf("the attached page is not read: %+v", got)
	}
}

// A worker started while the bucket is down waits (taking no jobs, so none
// burn attempts), then works the queue once the bucket answers.
func TestWorkerWaitsForTheBucket(t *testing.T) {
	h, proxy := proxiedWorker(t, func(p *tcpproxy.Proxy) { p.Down() })
	ctx := context.Background()
	ref := h.ref(t, "gallery")
	h.upload(t, ref, "originals/001.png", "image/png", pngImage(t, 300, 450, 31))
	attempted := func() int {
		var n int
		if err := h.pool.QueryRow(ctx, "SELECT coalesce(sum(attempt), 0) FROM "+h.workers+".river_job").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	time.Sleep(3 * time.Second)
	if n := attempted(); n != 0 {
		t.Fatalf("%d job attempts while the bucket was down", n)
	}
	proxy.Up(t)
	eventually(t, "the thumb after the bucket returned", time.Minute, h.has(ref, "thumb/001.webp"))
}

// proxiedWorker is newHostOn with the worker's store behind proxy.
func proxiedWorker(t *testing.T, setup func(*tcpproxy.Proxy)) (*host, *tcpproxy.Proxy) {
	var proxy *tcpproxy.Proxy
	h := newHostOn(t, func(env *s3test.Env) media.Store {
		proxy = tcpproxy.New(t, env.Config.Endpoint)
		setup(proxy)
		cfg := env.Config
		cfg.Endpoint, cfg.PublicEndpoint, cfg.Capabilities = proxy.URL, "", nil
		store, err := mediaS3.New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		return store
	})
	return h, proxy
}

// A bucket that accepts connections and never answers does not wedge the
// worker: each readiness check has a deadline, and it starts once the bucket
// answers.
func TestWorkerSurvivesAHungBucket(t *testing.T) {
	h, proxy := proxiedWorker(t, func(p *tcpproxy.Proxy) { p.Hang(t) })
	ref := h.ref(t, "gallery")
	h.upload(t, ref, "originals/001.png", "image/png", pngImage(t, 300, 450, 41))
	time.Sleep(2 * time.Second)
	proxy.Up(t)
	eventually(t, "the thumb after the bucket answered", 90*time.Second, h.has(ref, "thumb/001.webp"))
}

// A bucket outage after the worker started snoozes jobs instead of spending
// their attempts, so a long outage never discards them.
func TestWorkerSnoozesJobsDuringAnOutage(t *testing.T) {
	defer func(d time.Duration) { media.UnavailableSnooze = d }(media.UnavailableSnooze)
	media.UnavailableSnooze = 2 * time.Second
	h, proxy := proxiedWorker(t, func(*tcpproxy.Proxy) {})
	ctx := context.Background()
	first := h.ref(t, "gallery")
	h.upload(t, first, "originals/001.png", "image/png", pngImage(t, 300, 450, 51))
	eventually(t, "the worker running", time.Minute, h.has(first, "thumb/001.webp"))

	proxy.Down()
	ref := h.ref(t, "gallery")
	h.upload(t, ref, "originals/001.png", "image/png", pngImage(t, 300, 450, 52))
	time.Sleep(8 * time.Second)
	var attempts, errs int
	if err := h.pool.QueryRow(ctx, "SELECT coalesce(max(attempt), 0), coalesce(max(cardinality(errors)), 0) FROM "+h.workers+
		".river_job WHERE state <> 'completed'").Scan(&attempts, &errs); err != nil {
		t.Fatal(err)
	}
	if attempts > 1 || errs > 0 {
		var e string
		_ = h.pool.QueryRow(ctx, "SELECT errors::text FROM "+h.workers+".river_job WHERE state <> 'completed' LIMIT 1").Scan(&e)
		t.Fatalf("outage spent job attempts: attempt %d, %d errors: %s", attempts, errs, e)
	}
	proxy.Up(t)
	eventually(t, "the thumb after the outage", time.Minute, h.has(ref, "thumb/001.webp"))
}

// Two hosts on one database, with the same kind names: each worker drains
// only its host's schema, and progress and cancel read only their own.
func TestHostsShareADatabase(t *testing.T) {
	ctx := context.Background()
	var mu sync.Mutex
	ran := map[string][]string{} // worker → namespaces of the jobs it ran
	record := func(name string) rivertype.Hook {
		return river.HookWorkEndFunc(func(_ context.Context, job *rivertype.JobRow, err error) error {
			var args struct {
				Ref contentref.ContentRef `json:"ref"`
			}
			_ = json.Unmarshal(job.EncodedArgs, &args)
			mu.Lock()
			ran[name] = append(ran[name], args.Ref.TenantID)
			mu.Unlock()
			return err
		})
	}
	a, b := newHost(t, record("a")), newHost(t, record("b"))
	if a.workers == b.workers {
		t.Fatal("hosts share a worker schema")
	}
	refA, refB := a.ref(t, "gallery"), b.ref(t, "gallery")
	a.upload(t, refA, "cover", "image/png", pngImage(t, 300, 400, 3))
	b.upload(t, refB, "cover", "image/png", pngImage(t, 300, 400, 4))
	eventually(t, "both covers", time.Minute, func() bool {
		ra, oka := a.lastSettled(refA)
		rb, okb := b.lastSettled(refB)
		return oka && okb && ra.Ready() && rb.Ready()
	})
	mu.Lock()
	for name, ns := range map[string]string{"a": a.Tenant, "b": b.Tenant} {
		if len(ran[name]) == 0 {
			t.Fatalf("worker %s ran nothing", name)
		}
		for _, got := range ran[name] {
			if got != ns {
				t.Fatalf("worker %s ran a job of namespace %s: %v", name, got, ran)
			}
		}
	}
	mu.Unlock()

	// A third host without a running worker: its queued encode is its own.
	c := workerSchema(t, a.pool)
	if err := workqueue.Migrate(ctx, a.pool, c); err != nil {
		t.Fatal(err)
	}
	qc, err := workqueue.New(a.pool, a.reg, c)
	if err != nil {
		t.Fatal(err)
	}
	clip := a.ref(t, "clip")
	if err := qc.Enqueue(ctx, media.ProcessJob{Ref: clip}); err != nil {
		t.Fatal(err)
	}
	pa, _ := workqueue.NewProgressSource(a.pool, a.workers)
	pc, _ := workqueue.NewProgressSource(a.pool, c)
	if st, err := pa.EncodeProgress(ctx, clip); err != nil || st.Queued != nil || st.Files != nil {
		t.Fatalf("host a sees host c's encode: %+v %v", st, err)
	}
	if st, err := pc.EncodeProgress(ctx, clip); err != nil || st.Queued == nil {
		t.Fatalf("host c's encode not queued: %+v %v", st, err)
	}
	if n, err := a.queue.Cancel(ctx, clip); err != nil || n != 0 {
		t.Fatalf("host a cancelled %d of host c's jobs: %v", n, err)
	}
	if n, err := qc.Cancel(ctx, clip); err != nil || n == 0 {
		t.Fatalf("host c cancelled %d: %v", n, err)
	}
	for _, bad := range []string{"", "Media", "media-worker", "a.b", strings.Repeat("x", 64)} {
		if _, err := workqueue.New(a.pool, a.reg, bad); err == nil {
			t.Fatalf("schema %q accepted", bad)
		}
	}
}

// A host runs its media worker as its unprivileged app role; the worker
// schema is migrated by the host's migration step, so worker.New must not
// need DDL rights.
func TestWorkerRunsAsAnUnprivilegedRole(t *testing.T) {
	env := s3test.Open(t)
	ctx := context.Background()
	admin := pgtest.Pool(t, nil)
	hostSchema := pgtest.EmptySchema(t, ctx, admin)
	if err := riverhelpers.ApplyMigrations(ctx, admin, hostSchema); err != nil {
		t.Fatal(err)
	}
	schema := workerSchema(t, admin)
	if err := workqueue.Migrate(ctx, admin, schema); err != nil {
		t.Fatal(err)
	}
	dsn := pgtest.MediaWorkerRole(t, ctx, admin, schema, hostSchema)
	app, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	reg, err := media.NewRegistry(registry(env.Tenant, media.Hooks{}))
	if err != nil {
		t.Fatal(err)
	}
	cfg := worker.Config{Pool: app, Schema: schema, Store: env.Store, Kinds: reg, HostSchema: hostSchema, TempDir: t.TempDir(), Threads: 1}
	if _, err := worker.New(ctx, cfg); err != nil {
		t.Fatalf("worker as the app role: %v", err)
	}
	for _, table := range []string{"encode_run", "encode_chunk"} {
		t.Run(table, func(t *testing.T) {
			name := pgx.Identifier{schema, table}.Sanitize()
			hidden := pgx.Identifier{schema, table + "_missing"}.Sanitize()
			if _, err := admin.Exec(ctx, "ALTER TABLE "+name+" RENAME TO "+pgx.Identifier{table + "_missing"}.Sanitize()); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if _, err := admin.Exec(ctx, "ALTER TABLE "+hidden+" RENAME TO "+pgx.Identifier{table}.Sanitize()); err != nil {
					t.Error(err)
				}
			})
			if _, err := worker.New(ctx, cfg); err == nil || !strings.Contains(err.Error(), schema+"."+table) {
				t.Fatalf("worker accepted missing %s or did not identify it: %v", table, err)
			}
		})
	}
}

// faultyWorker is newHost with the worker's store behind an HTTP proxy whose
// fault answers a request itself (true) or lets it through.
func faultyWorker(t *testing.T, fault func(w http.ResponseWriter, r *http.Request) bool) *host {
	return newHostOn(t, func(env *s3test.Env) media.Store {
		target, _ := url.Parse(env.Config.Endpoint)
		proxy := httputil.NewSingleHostReverseProxy(target)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !fault(w, r) {
				proxy.ServeHTTP(w, r)
			}
		}))
		t.Cleanup(srv.Close)
		cfg := env.Config
		cfg.Endpoint, cfg.PublicEndpoint = srv.URL, ""
		store, err := mediaS3.New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		return store
	})
}

// imageJob reports the state of the worker's latest image job.
func (h *host) imageJob(t *testing.T) (state string, attempt, errs, snoozes int) {
	return h.job(t, workqueue.ImageArgs{}.Kind())
}

// job reports the state of the worker's latest job of kind.
func (h *host) job(t *testing.T, kind string) (state string, attempt, errs, snoozes int) {
	t.Helper()
	err := h.pool.QueryRow(context.Background(), `SELECT state, attempt, coalesce(cardinality(errors), 0), coalesce((metadata->>'snoozes')::int, 0)
		FROM `+h.workers+`.river_job WHERE kind = $1 ORDER BY id DESC LIMIT 1`, kind).Scan(&state, &attempt, &errs, &snoozes)
	if err != nil {
		t.Fatal(err)
	}
	return state, attempt, errs, snoozes
}

// discardNext makes the job's next failure its last and runs it now.
func (h *host) discardNext(t *testing.T) {
	t.Helper()
	if _, err := h.pool.Exec(context.Background(), `UPDATE `+h.workers+`.river_job SET max_attempts = attempt + 1, scheduled_at = now()
		WHERE kind = 'contentkit_media_image' AND state IN ('retryable', 'available')`); err != nil {
		t.Fatal(err)
	}
}

// blobName is a body's blob name.
func blobName(body []byte) string {
	sum := sha256.Sum256(body)
	return "sha256-" + hex.EncodeToString(sum[:])
}

// A job that outruns its own timeout spends its attempts (the bucket is
// fine; the job is not), and is discarded when they run out.
func TestWorkerTimedOutJobSpendsAttempts(t *testing.T) {
	defer func(d time.Duration) { imageTimeout = d }(imageTimeout)
	imageTimeout = 2 * time.Second
	body := pngImage(t, 300, 450, 61)
	name := blobName(body)
	h := faultyWorker(t, func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, name) {
			<-r.Context().Done() // this one object never answers
			return true
		}
		return false
	})
	h.upload(t, h.ref(t, "gallery"), "originals/001.png", "image/png", body)
	eventually(t, "the timed-out attempt recorded", time.Minute, func() bool { _, _, errs, _ := h.imageJob(t); return errs > 0 })
	if state, _, _, snoozes := h.imageJob(t); state == "scheduled" || snoozes != 0 {
		t.Fatalf("timed-out job: state %s, snoozes %d; want an attempt spent, no snooze", state, snoozes)
	}
	h.discardNext(t)
	eventually(t, "discarded after its attempts", time.Minute, func() bool { state, _, _, _ := h.imageJob(t); return state == "discarded" })
}

// A deterministic 500 on one object of a healthy bucket spends attempts too.
func TestWorkerBrokenObjectSpendsAttempts(t *testing.T) {
	body := pngImage(t, 300, 450, 62)
	name := blobName(body)
	h := faultyWorker(t, func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, name) {
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `<Error><Code>InternalError</Code></Error>`)
			return true
		}
		return false
	})
	h.upload(t, h.ref(t, "gallery"), "originals/001.png", "image/png", body)
	eventually(t, "the failed attempt recorded", time.Minute, func() bool { _, _, errs, _ := h.imageJob(t); return errs > 0 })
	if state, _, _, snoozes := h.imageJob(t); state == "scheduled" || snoozes != 0 {
		t.Fatalf("broken object: state %s, snoozes %d; want an attempt spent, no snooze", state, snoozes)
	}
	h.discardNext(t)
	eventually(t, "discarded after its attempts", time.Minute, func() bool { state, _, _, _ := h.imageJob(t); return state == "discarded" })
}

// Past MaxOutageSnoozes an outage spends attempts again (the backstop).
func TestWorkerOutageSnoozesAreCapped(t *testing.T) {
	defer func(d time.Duration, n int) { media.UnavailableSnooze, media.MaxOutageSnoozes = d, n }(media.UnavailableSnooze, media.MaxOutageSnoozes)
	media.UnavailableSnooze, media.MaxOutageSnoozes = time.Second, 2
	h, proxy := proxiedWorker(t, func(*tcpproxy.Proxy) {})
	first := h.ref(t, "gallery")
	h.upload(t, first, "originals/001.png", "image/png", pngImage(t, 300, 450, 71))
	eventually(t, "the worker running", time.Minute, h.has(first, "thumb/001.webp"))
	proxy.Down()
	h.upload(t, h.ref(t, "gallery"), "originals/001.png", "image/png", pngImage(t, 300, 450, 72))
	eventually(t, "an attempt spent after the snooze cap", time.Minute, func() bool {
		_, _, errs, snoozes := h.job(t, workqueue.PlaceArgs{}.Kind())
		return errs > 0 && snoozes >= 2
	})
}
