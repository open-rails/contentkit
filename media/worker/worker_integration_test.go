package worker_test

import (
	"bytes"
	"context"
	"crypto/sha256"
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

func (allow) CanUpload(context.Context, access.Actor, contentref.ContentRef) (media.UploadGrant, error) {
	return media.UploadGrant{Allowed: true}, nil
}

var alice = access.Actor{ID: "alice", Kind: "user"}

// host is a host that only presigns, commits and reads, with the worker
// running beside it on the same database and bucket.
type host struct {
	*s3test.Env
	pool      *pgxpool.Pool
	kinds     *media.Registry
	manifests *media.Manifests
	uploads   *media.Uploads
	queue     *workqueue.Queue
	worker    *worker.Worker

	mu      sync.Mutex
	encoded map[string]media.SlotListing // Hooks.SlotEncoded, by ref#slot
	settled map[string][]media.Readiness // Hooks.ItemReady, by ref
	schema  string                       // the host's River schema
	workers string                       // the host's worker schema
}

// lastSettled is the latest ItemReady report for ref.
func (h *host) lastSettled(ref contentref.ContentRef) (media.Readiness, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	all := h.settled[ref.Content().String()]
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
	kinds, err := media.NewRegistry(
		media.Kind{Name: "gallery", Versioned: true, Types: []string{"image/png"}, MaxBytes: 10 << 20,
			Specs: map[string]media.Spec{"thumb": {Width: 100, Height: 150, Fit: media.FitCover, Quality: 80}},
			Slots: map[string]media.Slot{"cover": {Aspect: media.Aspect3x1, Widths: []int{150, 300}}}},
		media.Kind{Name: "clip", Types: []string{"video/mp4"}, MaxBytes: 1 << 30, Video: &media.Video{Ladder: []int{240}}},
	)
	if err != nil {
		t.Fatal(err)
	}
	h := &host{Env: env, pool: pool, kinds: kinds, encoded: map[string]media.SlotListing{}, settled: map[string][]media.Readiness{}}

	// The host's River: publishes and sweeps the worker hands back run here.
	schema := pgtest.EmptySchema(t, ctx, pool)
	h.schema = schema
	if err := riverhelpers.ApplyMigrations(ctx, pool, schema); err != nil {
		t.Fatal(err)
	}
	jobs, err := media.NewJobs(media.JobsConfig{Store: env.Store, Locker: s3test.Locker(t, env.Store), Kinds: kinds, Tenants: []string{env.Tenant}, Resolver: editorResolver{}})
	if err != nil {
		t.Fatal(err)
	}
	hostClient, err := riverhelpers.New(ctx, pool, &river.Config{Schema: schema, FetchPollInterval: 100 * time.Millisecond,
		FetchCooldown: 50 * time.Millisecond}, jobs.RiverJobs())
	if err != nil {
		t.Fatal(err)
	}
	if err := hostClient.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = hostClient.StopAndCancel(ctx)
	})
	h.manifests = s3test.Manifests(t, env.Store, kinds, media.ManifestOptions{Sweeps: jobs})
	h.workers = workerSchema(t, pool)
	if err := workqueue.Migrate(ctx, pool, h.workers); err != nil {
		t.Fatal(err)
	}
	if h.queue, err = workqueue.New(pool, kinds, h.workers); err != nil {
		t.Fatal(err)
	}
	if h.uploads, err = media.NewUploads(media.UploadOptions{Store: env.Store, Kinds: kinds, Manifests: h.manifests,
		Authorizer: allow{}, Queue: h.queue}); err != nil {
		t.Fatal(err)
	}

	// The worker, built from the same registry, with the host's hooks.
	hostQueue, err := media.NewHostQueue(pool, kinds, schema, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	var store media.Store = env.Store
	if workerStore != nil {
		store = workerStore(env)
	}
	h.worker, err = worker.New(ctx, worker.Config{Pool: pool, Schema: h.workers, Store: store, Kinds: kinds, HostSchema: schema,
		TempDir: t.TempDir(), Threads: 2, RiverHooks: riverHooks, ImageTimeout: imageTimeout,
		Hooks: media.Hooks{SlotEncoded: func(_ context.Context, ref contentref.ContentRef, slot string, l media.SlotListing) {
			h.mu.Lock()
			h.encoded[ref.String()+"#"+slot] = l
			h.mu.Unlock()
		}, ItemReady: func(ctx context.Context, tx pgx.Tx, ref contentref.ContentRef, r media.Readiness) error {
			if r.Ready() {
				if err := hostQueue.ExposeTx(ctx, tx, ref); err != nil {
					return err
				}
			}
			h.mu.Lock()
			h.settled[ref.String()] = append(h.settled[ref.String()], r)
			h.mu.Unlock()
			return nil
		}}})
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

func (h *host) put(t *testing.T, p *media.PresignedRequest, body []byte) {
	t.Helper()
	req, _ := http.NewRequest(p.Method, p.URL, bytes.NewReader(body))
	for k := range p.Header {
		req.Header.Set(k, p.Header.Get(k))
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("put: %d", resp.StatusCode)
	}
}

// upload presigns and PUTs body like a browser (slot: and commits the slot).
func (h *host) upload(t *testing.T, ref contentref.ContentRef, slot, typ string, body []byte) string {
	t.Helper()
	sum := sha256.Sum256(body)
	p, err := h.uploads.Presign(context.Background(), alice, media.PresignRequest{Ref: ref, Type: typ, Size: int64(len(body)), SHA256: sum[:], Slot: slot})
	if err != nil {
		t.Fatal(err)
	}
	if p.Put != nil {
		h.put(t, p.Put, body)
	}
	if slot != "" {
		if err := h.uploads.CommitSlot(context.Background(), alice, media.SlotCommit{Ref: ref, Slot: slot, SHA256: sum[:]}); err != nil {
			t.Fatal(err)
		}
	}
	return p.Name
}

// stage puts body where a completed multipart upload lands, returning its name.
func (h *host) stage(t *testing.T, ref contentref.ContentRef, typ string, body []byte) string {
	t.Helper()
	item, _ := h.kinds.Item(ref)
	name := media.NewUploadName()
	key, _ := item.Original(name)
	if _, err := h.Store.Put(context.Background(), key, bytes.NewReader(body), int64(len(body)), media.PutOptions{ContentType: typ}); err != nil {
		t.Fatal(err)
	}
	return name
}

func (h *host) commit(t *testing.T, ref contentref.ContentRef, ops ...media.Op) {
	t.Helper()
	if _, err := h.uploads.Commit(context.Background(), alice, ref, ops); err != nil {
		t.Fatal(err)
	}
}

func (h *host) exists(t *testing.T, key string) bool {
	t.Helper()
	_, err := h.Store.Head(context.Background(), key)
	if err != nil && !errors.Is(err, media.ErrNotFound) {
		t.Fatal(err)
	}
	return err == nil
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
			kinds, err := media.NewRegistry(media.Kind{Name: "clip", Types: []string{"video/mp4"}, Video: &media.Video{Ladder: []int{240}}})
			if err != nil {
				t.Fatal(err)
			}
			w, err := worker.New(context.Background(), worker.Config{Pool: pool, Schema: schema, Store: env.Store, Kinds: kinds,
				Queue: tc.workerQueue, TempDir: scratch, Threads: 2})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(active); err != nil {
				t.Fatalf("one-shot worker swept another process's scratch: %v", err)
			}
			ref := contentref.New(env.Tenant, "clip", newID())
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

// The host enqueues; the worker derives variants and slot outputs (running
// the host's SlotEncoded hook), and places a staged upload at its SHA-256.
func TestWorkerProcessesImagesAndPlacesStagedUploads(t *testing.T) {
	h := newHost(t)
	ctx := context.Background()
	ref := contentref.NewVersion(h.Tenant, "gallery", newID(), "en")
	h.commit(t, ref, media.Op{Op: media.OpInsert, Name: "001.png", Original: h.upload(t, ref, "", "image/png", pngImage(t, 300, 450, 1))})
	staged := pngImage(t, 400, 600, 2)
	sum := sha256.Sum256(staged)
	name := h.stage(t, ref, "image/png", staged)
	h.commit(t, ref, media.Op{Op: media.OpInsert, Name: "002.png", Original: name})
	work := ref.Content()
	h.upload(t, work, "cover", "image/png", pngImage(t, 600, 200, 3))

	item, _ := h.kinds.Item(ref)
	eventually(t, "variants, placement and the cover", time.Minute, func() bool {
		m, _, err := h.manifests.Get(ctx, ref)
		if err != nil || len(m.Files) != 2 {
			return false
		}
		rec, err := h.manifests.Slot(ctx, work, "cover")
		if err != nil || rec.Result == nil || len(rec.Result.Outputs) == 0 {
			return false
		}
		cover, _ := item.Public(rec.Result.Outputs[0].Blob)
		return m.Files[0].Variants["thumb"].Blob != "" && m.Files[1].Variants["thumb"].Blob != "" &&
			m.Files[1].Original == media.SHA256Name(sum[:]) && h.exists(t, cover)
	})
	if key, _ := item.Original(name); h.exists(t, key) {
		t.Fatal("temp upload kept after placement")
	}
	var listing media.SlotListing
	var ok bool
	eventually(t, "SlotEncoded", 10*time.Second, func() bool { // it runs just after the outputs are recorded
		h.mu.Lock()
		defer h.mu.Unlock()
		listing, ok = h.encoded[work.String()+"#cover"]
		return ok
	})
	if !ok || listing.Aspect != media.Aspect3x1 || len(listing.Outputs) == 0 {
		t.Fatalf("the host's SlotEncoded hook did not run in the worker: %+v %v", listing, ok)
	}
}

// A staged video is hashed while the worker downloads it for ffmpeg, placed,
// then encoded; the manifest names the placed original throughout.
func TestWorkerPlacesAndEncodesStagedVideo(t *testing.T) {
	if os.Getenv("CONTENTKIT_TEST_FFMPEG") == "" {
		t.Skip("CONTENTKIT_TEST_FFMPEG not set")
	}
	h := newHost(t)
	ctx := context.Background()
	src := filepath.Join(t.TempDir(), "clip.mp4")
	if out, err := exec.Command("ffmpeg", "-v", "error", "-f", "lavfi", "-i", "testsrc=duration=3:size=426x240:rate=24",
		"-f", "lavfi", "-i", "sine=duration=3", "-shortest", "-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:a", "aac", src).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v %s", err, out)
	}
	body, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	ref := contentref.New(h.Tenant, "clip", newID())
	name := h.stage(t, ref, "video/mp4", body)
	h.commit(t, ref, media.Op{Op: media.OpInsert, Name: "source", Original: name})
	if r, err := h.manifests.Readiness(ctx, ref); err != nil || r.State != media.StateProcessing {
		t.Fatalf("a staged video is %+v, %v; want processing", r, err)
	}
	eventually(t, "the encode", 2*time.Minute, func() bool {
		m, _, err := h.manifests.Get(ctx, ref)
		return err == nil && m.Files[0].HLS != nil && len(m.Files[0].HLS.Video) > 0
	})
	m, _, _ := h.manifests.Get(ctx, ref)
	if f := m.Files[0]; f.Original != media.SHA256Name(sum[:]) || f.HLS.Source != f.Original {
		t.Fatalf("not placed before the encode: %+v", f)
	}
	item, _ := h.kinds.Item(ref)
	if key, _ := item.Original(name); h.exists(t, key) {
		t.Fatal("temp upload kept after placement")
	}
	// Ready only once the poster is grabbed and rendered too.
	eventually(t, "ItemReady", time.Minute, func() bool { r, ok := h.lastSettled(ref); return ok && r.Ready() })
	rec, err := h.manifests.Slot(ctx, ref, media.PosterSlot)
	if err != nil || rec.Result == nil || len(rec.Result.Outputs) == 0 {
		t.Fatalf("ready before the poster rendered: %+v %v", rec, err)
	}
}

// Hooks.ItemReady runs in the worker once an item settles: failed while a
// file cannot be processed (viewers never see it; its editor does), ready once
// it is removed, with HostQueue.ExposeTx enqueuing the host's Expose in the
// hook's transaction.
// A worker started while the bucket is down waits (taking no jobs, so none
// burn attempts), then works the queue once the bucket answers.
func TestWorkerWaitsForTheBucket(t *testing.T) {
	h, proxy := proxiedWorker(t, func(p *tcpproxy.Proxy) { p.Down() })
	ctx := context.Background()
	ref := contentref.NewVersion(h.Tenant, "gallery", newID(), "en")
	h.commit(t, ref, media.Op{Op: media.OpInsert, Name: "001.png", Original: h.upload(t, ref, "", "image/png", pngImage(t, 300, 450, 31))})
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
	eventually(t, "the variant after the bucket returned", time.Minute, func() bool {
		m, _, err := h.manifests.Get(ctx, ref)
		return err == nil && len(m.Files) == 1 && m.Files[0].Variants["thumb"].Blob != ""
	})
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

func (h *host) thumbed(t *testing.T, ref contentref.ContentRef) func() bool {
	return func() bool {
		m, _, err := h.manifests.Get(context.Background(), ref)
		return err == nil && len(m.Files) == 1 && m.Files[0].Variants["thumb"].Blob != ""
	}
}

// A bucket that accepts connections and never answers does not wedge the
// worker: each readiness check has a deadline, and it starts once the bucket
// answers.
func TestWorkerSurvivesAHungBucket(t *testing.T) {
	h, proxy := proxiedWorker(t, func(p *tcpproxy.Proxy) { p.Hang(t) })
	ref := contentref.NewVersion(h.Tenant, "gallery", newID(), "en")
	h.commit(t, ref, media.Op{Op: media.OpInsert, Name: "001.png", Original: h.upload(t, ref, "", "image/png", pngImage(t, 300, 450, 41))})
	time.Sleep(2 * time.Second)
	proxy.Up(t)
	eventually(t, "the variant after the bucket answered", 90*time.Second, h.thumbed(t, ref))
}

// A bucket outage after the worker started snoozes jobs instead of spending
// their attempts, so a long outage never discards them.
func TestWorkerSnoozesJobsDuringAnOutage(t *testing.T) {
	defer func(d time.Duration) { media.UnavailableSnooze = d }(media.UnavailableSnooze)
	media.UnavailableSnooze = 2 * time.Second
	h, proxy := proxiedWorker(t, func(*tcpproxy.Proxy) {})
	ctx := context.Background()
	first := contentref.NewVersion(h.Tenant, "gallery", newID(), "en")
	h.commit(t, first, media.Op{Op: media.OpInsert, Name: "001.png", Original: h.upload(t, first, "", "image/png", pngImage(t, 300, 450, 51))})
	eventually(t, "the worker running", time.Minute, h.thumbed(t, first))

	proxy.Down()
	ref := contentref.NewVersion(h.Tenant, "gallery", newID(), "en")
	h.commit(t, ref, media.Op{Op: media.OpInsert, Name: "001.png", Original: h.upload(t, ref, "", "image/png", pngImage(t, 300, 450, 52))})
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
	eventually(t, "the variant after the outage", time.Minute, h.thumbed(t, ref))
}

func TestItemReadyAfterProcessing(t *testing.T) {
	h := newHost(t)
	ctx := context.Background()
	ref := contentref.NewVersion(h.Tenant, "gallery", newID(), "en")
	h.commit(t, ref,
		media.Op{Op: media.OpInsert, Name: "001.png", Original: h.upload(t, ref, "", "image/png", pngImage(t, 300, 450, 21))},
		media.Op{Op: media.OpInsert, Name: "002.png", Original: h.upload(t, ref, "", "image/png", []byte("not a png at all"))})
	eventually(t, "the failure reported", time.Minute, func() bool { r, ok := h.lastSettled(ref); return ok && r.State == media.StateFailed })
	if r, _ := h.lastSettled(ref); len(r.Failed) != 1 || r.Failed[0] != "en/002.png" || len(r.Processing) != 0 {
		t.Fatalf("readiness: %+v", r)
	}
	if got := h.read(t, ref, access.Actor{Anonymous: true}, false); len(got) != 1 || got[0].Name != "001.png" {
		t.Fatalf("a viewer reads %+v; want only the processed file", got)
	}
	if got := h.read(t, ref, alice, false); len(got) != 2 || got[1].Failed == "" {
		t.Fatalf("the editor reads %+v; want both, the failure named", got)
	}
	h.commit(t, ref, media.Op{Op: media.OpRemove, Name: "002.png"})
	eventually(t, "ready after the removal", time.Minute, func() bool { r, ok := h.lastSettled(ref); return ok && r.Ready() })
	var n int
	if err := h.pool.QueryRow(ctx, "SELECT count(*) FROM "+h.schema+".river_job WHERE kind = 'contentkit_media_expose'").Scan(&n); err != nil || n == 0 {
		t.Fatalf("no Expose enqueued in the host schema: %d %v", n, err)
	}
}

// An edit that lands as a slot job finishes, while River still has it
// running, is absorbed as a follow-up job and still rendered.
func TestWorkerRendersASlotEditLandingAsTheJobFinishes(t *testing.T) {
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
			once.Do(func() { edited <- cur.h.uploads.EditSlot(context.Background(), alice, cur.ref, "cover", late) })
		}
		return err
	}))
	ref := contentref.New(h.Tenant, "gallery", newID())
	tg.Store(&target{h: h, ref: ref})
	h.upload(t, ref, "cover", "image/png", pngImage(t, 300, 400, 13))
	select {
	case err := <-edited:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Minute):
		t.Fatal("the slot job did not run")
	}
	eventually(t, "the late edit rendered", time.Minute, func() bool {
		rec, err := h.manifests.Slot(context.Background(), ref, "cover")
		return err == nil && rec.Result != nil && rec.Edit.Hash() == late.Hash() && rec.Result.Of == rec.Fingerprint(h.cover(t))
	})
}

func (h *host) cover(t *testing.T) media.Slot {
	t.Helper()
	k, err := h.kinds.Kind("gallery")
	if err != nil {
		t.Fatal(err)
	}
	return k.Slots["cover"]
}

type editorResolver struct{}

func (editorResolver) Resolve(_ context.Context, refs []contentref.ContentRef, a access.Actor) (map[contentref.ContentKey]access.Resolution, error) {
	out := map[contentref.ContentKey]access.Resolution{}
	for _, ref := range refs {
		out[ref.Key()] = access.Resolution{Visible: true, Accessible: true, Editor: a.ID == alice.ID}
	}
	return out, nil
}

func (h *host) read(t *testing.T, ref contentref.ContentRef, actor access.Actor, unattached bool) []media.FileInfo {
	t.Helper()
	r, err := media.NewReader(media.ReaderOptions{Manifests: h.manifests, Kinds: h.kinds, Resolver: editorResolver{},
		Delivery: media.Delivery{Mode: media.DeliverURL, BaseURL: "https://media.invalid", SigningKey: token.Key{ID: "k", Secret: bytes.Repeat([]byte("k"), 32)}}})
	if err != nil {
		t.Fatal(err)
	}
	res, err := r.Read(context.Background(), ref, actor, media.ReadOptions{Unattached: unattached})
	if err != nil {
		t.Fatal(err)
	}
	return res.Files
}

// ProcessOnUpload: an unattached file is processed before it is attached and
// readers leave it out until then; attaching does not reprocess; discarding a
// file mid-encode cancels the job and deletes its objects.
func TestProcessOnUploadAttachAndDiscard(t *testing.T) {
	if os.Getenv("CONTENTKIT_TEST_FFMPEG") == "" {
		t.Skip("CONTENTKIT_TEST_FFMPEG not set")
	}
	h := newHost(t)
	ctx := context.Background()
	gallery := contentref.NewVersion(h.Tenant, "gallery", newID(), "en")
	name := h.upload(t, gallery, "", "image/png", pngImage(t, 300, 450, 7))
	h.commit(t, gallery, media.Op{Op: media.OpInsert, Name: "001.png", Original: name, Unattached: true})
	eventually(t, "the unattached image derived", time.Minute, func() bool {
		m, _, err := h.manifests.Get(ctx, gallery)
		return err == nil && m.Files[0].Unattached && m.Files[0].Variants["thumb"].Blob != ""
	})
	if got := h.read(t, gallery, access.Actor{Anonymous: true}, true); len(got) != 0 {
		t.Fatalf("a viewer sees an unattached file: %+v", got)
	}
	if got := h.read(t, gallery, alice, false); len(got) != 0 {
		t.Fatalf("an editor's plain read lists an unattached file: %+v", got)
	}
	if got := h.read(t, gallery, alice, true); len(got) != 1 || !got[0].Unattached || got[0].Width == 0 {
		t.Fatalf("the editor's unattached file: %+v", got)
	}
	before, _, _ := h.manifests.Get(ctx, gallery)
	h.commit(t, gallery, media.Op{Op: media.OpAttach, Name: "001.png"})
	h.commit(t, gallery, media.Op{Op: media.OpAttach, Name: "001.png"}) // idempotent
	after, _, _ := h.manifests.Get(ctx, gallery)
	if f := after.Files[0]; f.Unattached || f.Variants["thumb"] != before.Files[0].Variants["thumb"] {
		t.Fatalf("attach changed the derived file: %+v", f)
	}
	if got := h.read(t, gallery, access.Actor{Anonymous: true}, false); len(got) != 1 {
		t.Fatalf("the attached file is not read: %+v", got)
	}

	// A long staged video, discarded while its encode runs.
	src := filepath.Join(t.TempDir(), "long.mp4")
	if out, err := exec.Command("ffmpeg", "-v", "error", "-f", "lavfi", "-i", "testsrc=duration=90:size=426x240:rate=30",
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", src).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v %s", err, out)
	}
	body, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	clip := contentref.New(h.Tenant, "clip", newID())
	staged := h.stage(t, clip, "video/mp4", body)
	h.commit(t, clip, media.Op{Op: media.OpInsert, Name: "long", Original: staged, Unattached: true})
	progress, err := workqueue.NewProgressSource(h.pool, h.workers)
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "the encode running", time.Minute, func() bool {
		st, err := progress.EncodeProgress(ctx, clip)
		p, ok := st.Files["long"]
		return err == nil && ok && p.Phase == media.PhaseEncoding
	})
	h.commit(t, clip, media.Op{Op: media.OpRemove, Name: "long"})
	match, _, _ := workqueue.RefMatch(clip)
	eventually(t, "the encode cancelled", 30*time.Second, func() bool {
		var n int
		err := h.pool.QueryRow(ctx, "SELECT count(*) FROM "+h.workers+".river_job WHERE kind = $1 AND args @> $2 AND state = 'cancelled'",
			(workqueue.VideoChunkArgs{}).Kind(), match).Scan(&n)
		return err == nil && n > 0
	})
	item, _ := h.kinds.Item(clip)
	stagedKey, _ := item.Original(staged)
	placedKey, _ := item.Original(media.SHA256Name(sum[:]))
	if h.exists(t, stagedKey) || h.exists(t, placedKey) {
		t.Fatal("the discarded video's original was kept")
	}
	time.Sleep(2 * time.Second) // a cancelled job must not write back
	if m, _, err := h.manifests.Get(ctx, clip); err != nil || len(m.Files) != 0 {
		t.Fatalf("after discard: %+v %v", m, err)
	}
}

// Two hosts on one database, with the same kind names: each worker drains
// only its host's schema, and progress and cancel read only their own.
func TestHostsShareADatabase(t *testing.T) {
	ctx := context.Background()
	var mu sync.Mutex
	ran := map[string][]string{} // worker → tenants of the jobs it ran
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
	refA, refB := contentref.New(a.Tenant, "gallery", newID()), contentref.New(b.Tenant, "gallery", newID())
	a.upload(t, refA, "cover", "image/png", pngImage(t, 300, 400, 3))
	b.upload(t, refB, "cover", "image/png", pngImage(t, 300, 400, 4))
	eventually(t, "both covers", time.Minute, func() bool {
		ra, oka := a.lastSettled(refA)
		rb, okb := b.lastSettled(refB)
		return oka && okb && ra.Ready() && rb.Ready()
	})
	mu.Lock()
	for name, tenant := range map[string]string{"a": a.Tenant, "b": b.Tenant} {
		if len(ran[name]) == 0 {
			t.Fatalf("worker %s ran nothing", name)
		}
		for _, got := range ran[name] {
			if got != tenant {
				t.Fatalf("worker %s ran a job of tenant %s: %v", name, got, ran)
			}
		}
	}
	mu.Unlock()

	// A third host without a running worker: its queued encode is its own.
	c := workerSchema(t, a.pool)
	if err := workqueue.Migrate(ctx, a.pool, c); err != nil {
		t.Fatal(err)
	}
	qc, err := workqueue.New(a.pool, a.kinds, c)
	if err != nil {
		t.Fatal(err)
	}
	clip := contentref.New(a.Tenant, "clip", newID())
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
		if _, err := workqueue.New(a.pool, a.kinds, bad); err == nil {
			t.Fatalf("schema %q accepted", bad)
		}
	}
}

// A host runs its media worker as its unprivileged app role; the worker
// schema is migrated by the host's migration step, so worker.New must not
// need DDL rights (CREATE on public for migration tracking).
func TestWorkerRunsAsAnUnprivilegedRole(t *testing.T) {
	env := s3test.Open(t)
	ctx := context.Background()
	admin := pgtest.Pool(t, nil)
	host := pgtest.EmptySchema(t, ctx, admin)
	if err := riverhelpers.ApplyMigrations(ctx, admin, host); err != nil {
		t.Fatal(err)
	}
	schema := workerSchema(t, admin)
	if err := workqueue.Migrate(ctx, admin, schema); err != nil {
		t.Fatal(err)
	}
	dsn := pgtest.MediaWorkerRole(t, ctx, admin, schema, host)
	app, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	kinds, err := media.NewRegistry(media.Kind{Name: "gallery", Versioned: true, Types: []string{"image/png"}, MaxBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := worker.New(ctx, worker.Config{Pool: app, Schema: schema, Store: env.Store, Kinds: kinds, HostSchema: host,
		TempDir: t.TempDir(), Threads: 1}); err != nil {
		t.Fatalf("worker as the app role: %v", err)
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

// imageJob reports the state of the worker's one unfinished image job.
func (h *host) imageJob(t *testing.T) (state string, attempt, errs, snoozes int) {
	t.Helper()
	err := h.pool.QueryRow(context.Background(), `SELECT state, attempt, coalesce(cardinality(errors), 0), coalesce((metadata->>'snoozes')::int, 0)
		FROM `+h.workers+`.river_job WHERE kind = 'contentkit_media_image' ORDER BY id DESC LIMIT 1`).Scan(&state, &attempt, &errs, &snoozes)
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

// A job that outruns its own timeout spends its attempts (the bucket is
// fine; the job is not), and is discarded when they run out.
func TestWorkerTimedOutJobSpendsAttempts(t *testing.T) {
	defer func(d time.Duration) { imageTimeout = d }(imageTimeout)
	imageTimeout = 2 * time.Second
	var name atomic.Value
	name.Store("\x00")
	h := faultyWorker(t, func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, name.Load().(string)) {
			<-r.Context().Done() // this one object never answers
			return true
		}
		return false
	})
	ref := contentref.NewVersion(h.Tenant, "gallery", newID(), "en")
	original := h.upload(t, ref, "", "image/png", pngImage(t, 300, 450, 61))
	name.Store(original)
	h.commit(t, ref, media.Op{Op: media.OpInsert, Name: "001.png", Original: original})
	eventually(t, "the timed-out attempt recorded", time.Minute, func() bool { _, _, errs, _ := h.imageJob(t); return errs > 0 })
	if state, _, _, snoozes := h.imageJob(t); state == "scheduled" || snoozes != 0 {
		t.Fatalf("timed-out job: state %s, snoozes %d; want an attempt spent, no snooze", state, snoozes)
	}
	h.discardNext(t)
	eventually(t, "discarded after its attempts", time.Minute, func() bool { state, _, _, _ := h.imageJob(t); return state == "discarded" })
}

// A deterministic 500 on one object of a healthy bucket spends attempts too.
func TestWorkerBrokenObjectSpendsAttempts(t *testing.T) {
	var name atomic.Value
	name.Store("\x00")
	h := faultyWorker(t, func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, name.Load().(string)) {
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `<Error><Code>InternalError</Code></Error>`)
			return true
		}
		return false
	})
	ref := contentref.NewVersion(h.Tenant, "gallery", newID(), "en")
	original := h.upload(t, ref, "", "image/png", pngImage(t, 300, 450, 62))
	name.Store(original)
	h.commit(t, ref, media.Op{Op: media.OpInsert, Name: "001.png", Original: original})
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
	first := contentref.NewVersion(h.Tenant, "gallery", newID(), "en")
	h.commit(t, first, media.Op{Op: media.OpInsert, Name: "001.png", Original: h.upload(t, first, "", "image/png", pngImage(t, 300, 450, 71))})
	eventually(t, "the worker running", time.Minute, h.thumbed(t, first))
	proxy.Down()
	ref := contentref.NewVersion(h.Tenant, "gallery", newID(), "en")
	h.commit(t, ref, media.Op{Op: media.OpInsert, Name: "001.png", Original: h.upload(t, ref, "", "image/png", pngImage(t, 300, 450, 72))})
	eventually(t, "an attempt spent after the snooze cap", time.Minute, func() bool {
		_, _, errs, snoozes := h.imageJob(t)
		return errs > 0 && snoozes >= 2
	})
}
