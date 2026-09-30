package media_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
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
)

func TestDeleteJobQuotaReleaseReplayed(t *testing.T) {
	env := s3test.Open(t)
	ctx := context.Background()
	quotaPool := pgtest.Pool(t, nil)
	limiter, err := media.NewPGLimiter(quotaPool, pgtest.Schema(t, ctx, quotaPool), media.PGLimits{})
	if err != nil {
		t.Fatal(err)
	}
	if err := limiter.Settle(ctx, media.Settlement{Tenant: env.Tenant, Owner: "owner", Delta: 100}); err != nil {
		t.Fatal(err)
	}
	kinds := registry(t)
	manifests := s3test.Manifests(t, env.Store, kinds, media.ManifestOptions{})
	ref := contentref.New(env.Tenant, "post", contentref.NewID())
	if _, err := manifests.Create(ctx, ref); err != nil {
		t.Fatal(err)
	}
	item, _ := kinds.Item(ref)
	body := strings.Repeat("o", 70)
	name := blobName(body)
	original, _ := item.Original(name)
	putObject(t, env.Store, original, body)
	if _, err := manifests.Edit(ctx, ref, func(m *media.Manifest) error {
		m.Files = []media.File{{Name: "file", Original: name, Type: "image/png", Size: int64(len(body))}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	releaser := &failingReleaser{QuotaReleaser: limiter, after: true}
	jobs, err := media.NewJobs(media.JobsConfig{Store: env.Store, Kinds: kinds,
		Locker: s3test.Locker(t, env.Store), Limiter: releaser})
	if err != nil {
		t.Fatal(err)
	}
	_, pool, schema := riverHost(t, jobs, make(chan string, 4))
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		return jobs.DeleteItemsTx(ctx, tx, media.Deletion{Ref: ref, Owner: "owner"})
	}); err != nil {
		t.Fatal(err)
	}
	jobTable := pgx.Identifier{schema, "river_job"}.Sanitize()
	waitFor(t, "deletion replay", func() bool {
		var state string
		var attempt int
		return pool.QueryRow(ctx, "SELECT state, attempt FROM "+jobTable+" WHERE kind = $1 AND args->>'final' IS NULL",
			"contentkit_media_delete_folder").Scan(&state, &attempt) == nil && state == "completed" && attempt >= 2
	})
	if n := releaser.calls.Load(); n != 1 {
		t.Fatalf("release called %d times, want replay to observe the applied record", n)
	}
	used, _, err := limiter.Usage(ctx, env.Tenant, "owner")
	if err != nil || used != 30 {
		t.Fatalf("replayed release: used=%d err=%v", used, err)
	}
}

type hostArgs struct{}

func (hostArgs) Kind() string { return "host_job" }

type hostWorker struct {
	river.WorkerDefaults[hostArgs]
	done chan string
}

func (w *hostWorker) Work(context.Context, *river.Job[hostArgs]) error { w.done <- "host"; return nil }

// imageArgs stands in for a later media lane registering through Jobs.Register.
type imageArgs struct{}

func (imageArgs) Kind() string { return "test_media_image" }

type imageWorker struct {
	river.WorkerDefaults[imageArgs]
	done chan string
}

func (w *imageWorker) Work(context.Context, *river.Job[imageArgs]) error {
	w.done <- "image"
	return nil
}

// riverHost composes media's jobs with a host contribution into one client on
// a fresh River schema, as a host does, and starts it.
func riverHost(t *testing.T, jobs *media.Jobs, done chan string) (*river.Client[pgx.Tx], *pgxpool.Pool, string) {
	t.Helper()
	ctx := context.Background()
	pool := pgtest.Pool(t, nil)
	schema := pgtest.EmptySchema(t, ctx, pool)
	if err := riverhelpers.ApplyMigrations(ctx, pool, schema); err != nil {
		t.Fatal(err)
	}
	if err := jobs.Register(func(cfg *river.Config) error {
		cfg.Queues["test_media_image"] = river.QueueConfig{MaxWorkers: 1}
		return river.AddWorkerSafely(cfg.Workers, &imageWorker{done: done})
	}); err != nil {
		t.Fatal(err)
	}
	host := riverhelpers.NewContribution("host", func(_ context.Context, cfg *river.Config) error {
		cfg.Queues["host"] = river.QueueConfig{MaxWorkers: 1}
		return river.AddWorkerSafely(cfg.Workers, &hostWorker{done: done})
	}, nil, nil)
	cfg := &river.Config{Schema: schema, FetchPollInterval: 100 * time.Millisecond, FetchCooldown: 50 * time.Millisecond}
	client, err := riverhelpers.New(ctx, pool, cfg, host, jobs.RiverJobs())
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
	return client, pool, schema
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(45 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func TestJobsComposeWithHostRiverAndSweepAfterEdit(t *testing.T) {
	env := s3test.Open(t)
	if !env.Store.Capabilities().ConditionalPut {
		t.Skip("backend lacks conditional PUT")
	}
	ctx := context.Background()
	r := registry(t)
	jobs, err := media.NewJobs(media.JobsConfig{Store: env.Store, Locker: s3test.Locker(t, env.Store), Kinds: r, Tenants: []string{env.Tenant}, Grace: 3 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan string, 4)
	client, pool, schema := riverHost(t, jobs, done)

	// One composition only; a failed second one leaves the first bound.
	if _, err := riverhelpers.New(ctx, pool, &river.Config{Schema: schema}, jobs.RiverJobs()); err == nil {
		t.Fatal("media jobs composed twice")
	}
	if err := jobs.Register(func(*river.Config) error { return nil }); err == nil {
		t.Fatal("Register after composition accepted")
	}
	if _, err := client.PeriodicJobs().AddSafely(river.NewPeriodicJob(river.PeriodicInterval(time.Hour),
		func() (river.JobArgs, *river.InsertOpts) { return hostArgs{}, nil },
		&river.PeriodicJobOpts{ID: "contentkit_media_sweep_pass"})); err == nil {
		t.Fatal("periodic sweep pass is not registered")
	}
	if _, err := client.Insert(ctx, hostArgs{}, &river.InsertOpts{Queue: "host"}); err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.Insert(ctx, imageArgs{}, &river.InsertOpts{Queue: "test_media_image"}); err != nil {
		t.Fatal(err)
	}
	ran := []string{<-done, <-done}
	slices.Sort(ran)
	if !slices.Equal(ran, []string{"host", "image"}) {
		t.Fatalf("workers ran %v", ran)
	}

	ms := s3test.Manifests(t, env.Store, r, media.ManifestOptions{Sweeps: jobs})
	ref := contentref.New(env.Tenant, "post", cid(501))
	item, _ := r.Item(ref)
	// The host creates the item before its first upload lands.
	if _, err := ms.Create(ctx, ref); err != nil {
		t.Fatal(err)
	}
	orphan, kept := item.PrivatePrefix()+blobName("orphan"), item.PrivatePrefix()+blobName("kept")
	putObject(t, env.Store, orphan, "orphan")
	putObject(t, env.Store, item.OriginalsPrefix()+blobName("orig"), "orig")
	putObject(t, env.Store, kept, "kept")
	if _, err := ms.Edit(ctx, ref, func(m *media.Manifest) error {
		m.Files = []media.File{{Name: "a.jpg", Original: blobName("orig"), Variants: map[string]media.Variant{"large": {Blob: blobName("kept")}}}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var scheduled time.Time
	if err := pool.QueryRow(ctx, "SELECT scheduled_at FROM "+pgx.Identifier{schema, "river_job"}.Sanitize()+
		" WHERE kind = 'contentkit_media_sweep' AND args->>'prefix' = $1", item.Prefix()).Scan(&scheduled); err != nil {
		t.Fatal("edit did not schedule a sweep:", err)
	}
	if d := time.Until(scheduled); d < time.Second || d > 4*time.Second {
		t.Fatalf("sweep scheduled %v from now, want about the 3s grace", d)
	}
	// A second edit inside the grace is absorbed by the waiting sweep.
	if _, err := ms.Edit(ctx, ref, func(m *media.Manifest) error { m.Meta = map[string]any{"k": 1}; return nil }); err != nil {
		t.Fatal(err)
	}
	// The media worker schedules into the host schema the same way.
	host, err := media.NewHostQueue(pool, r, schema, "", 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := host.ScheduleSweep(ctx, ref); err != nil {
		t.Fatal(err)
	}
	var n int
	_ = pool.QueryRow(ctx, "SELECT count(*) FROM "+pgx.Identifier{schema, "river_job"}.Sanitize()+" WHERE kind = 'contentkit_media_sweep'").Scan(&n)
	if n != 1 {
		t.Fatalf("%d sweep jobs for one folder", n)
	}
	waitFor(t, "the scheduled sweep", func() bool { return !slices.Contains(listKeys(t, env.Store, item.Prefix()), orphan) })
	left := listKeys(t, env.Store, item.Prefix())
	if !slices.Contains(left, kept) || !slices.Contains(left, item.OriginalsPrefix()+blobName("orig")) || len(left) != 3 {
		t.Fatalf("sweep left %v", left)
	}
}

func TestDeleteAndEraseRemoveFoldersIncludingLateUploads(t *testing.T) {
	env := s3test.Open(t)
	ctx := context.Background()
	r := registry(t)
	lpool := pgtest.Pool(t, nil)
	limiter, err := media.NewPGLimiter(lpool, pgtest.Schema(t, ctx, lpool), media.PGLimits{})
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := media.NewJobs(media.JobsConfig{Store: env.Store, Locker: s3test.Locker(t, env.Store), Kinds: r, Limiter: limiter, LateUploadWindow: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	_, pool, schema := riverHost(t, jobs, make(chan string, 4))
	s := env.Store

	post, other := contentref.New(env.Tenant, "post", cid(9)), contentref.New(env.Tenant, "post", cid(10))
	g := contentref.New(env.Tenant, "gallery", cid(3))
	for _, ref := range []contentref.ContentRef{post, other, g} {
		item, _ := r.Item(ref)
		putObject(t, s, item.PrivatePrefix()+blobName("b"), "b")
		putObject(t, s, item.OriginalsPrefix()+blobName("o"), "o")
	}
	// The post's commits charged its owner 700 bytes of originals; a gallery version 50.
	pi, _ := r.Item(post)
	putObject(t, s, pi.Prefix()+"manifest.json", `{"files":[{"name":"a","original":"`+blobName("o")+`","size":300},`+
		`{"name":"b","original":"`+blobName("o2")+`","size":400},{"name":"a2","original":"`+blobName("o")+`","size":300}]}`)
	oi, _ := r.Item(other)
	putObject(t, s, oi.Prefix()+"manifest.json", `{"files":[]}`)
	gi0, _ := r.Item(g)
	putObject(t, s, gi0.ManifestKey(), `{"files":[],"versions":{"v1":{"files":[{"name":"p","original":"`+blobName("o")+`","size":50}]}}}`)
	if err := limiter.Settle(ctx, media.Settlement{Tenant: env.Tenant, Owner: "chan-a", Delta: 1000}); err != nil {
		t.Fatal(err)
	}
	user, _ := r.Item(contentref.New(env.Tenant, "user", cid(11)))
	putObject(t, s, user.OriginalsPrefix()+blobName("avatar original"), "avatar original")
	putObject(t, s, user.PublicPrefix()+blobName("avatar"), "avatar")

	inTx := func(fn func(pgx.Tx) error, commit bool) {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if err := fn(tx); err != nil {
			t.Fatal(err)
		}
		if commit {
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := jobs.DeleteItemsTx(ctx, nil, media.Deletion{Ref: g.WithVersion("v1")}); err == nil {
		t.Fatal("a version ref must not delete the work's folder")
	}
	// A rolled-back host delete deletes nothing.
	inTx(func(tx pgx.Tx) error { return jobs.DeleteItemsTx(ctx, tx, media.Deletion{Ref: other, Owner: "chan-a"}) }, false)
	var n int
	_ = pool.QueryRow(ctx, "SELECT count(*) FROM "+pgx.Identifier{schema, "river_job"}.Sanitize()+" WHERE kind = 'contentkit_media_delete_folder'").Scan(&n)
	if n != 0 {
		t.Fatal("rolled-back delete enqueued a job")
	}

	// User erasure: the host's items for the user plus user/{id}/.
	inTx(func(tx pgx.Tx) error {
		return jobs.EraseUserTx(ctx, tx, env.Tenant, cid(11), media.Deletion{Ref: post, Owner: "chan-a"}, media.Deletion{Ref: g, Owner: "chan-a"})
	}, true)
	p, _ := r.Item(post)
	gi, _ := r.Item(g)
	gone := func() bool {
		return len(listKeys(t, s, p.Prefix())) == 0 && len(listKeys(t, s, gi.Prefix())) == 0 && len(listKeys(t, s, user.Prefix())) == 0
	}
	waitFor(t, "folder deletion", gone)
	waitFor(t, "quota release", func() bool {
		used, _, err := limiter.Usage(ctx, env.Tenant, "chan-a")
		return err == nil && used == 1000-700-50
	})

	// A presigned PUT issued before the delete lands after it; the second pass removes it.
	body := []byte("late upload")
	sum := sha256.Sum256(body)
	late := p.OriginalsPrefix() + media.SHA256Name(sum[:])
	req, err := s.PresignPut(ctx, late, media.PresignPut{ContentType: "image/jpeg", Size: int64(len(body)), SHA256: sum[:], TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	httpReq, _ := http.NewRequest(req.Method, req.URL, strings.NewReader(string(body)))
	httpReq.Header = req.Header.Clone()
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || len(listKeys(t, s, p.Prefix())) != 1 {
		t.Fatalf("late PUT: %d", resp.StatusCode)
	}
	waitFor(t, "the second deletion pass", gone)
	if used, _, _ := limiter.Usage(ctx, env.Tenant, "chan-a"); used != 250 {
		t.Fatalf("quota released twice: %d", used)
	}
	if got := listKeys(t, s, oi.Prefix()); len(got) != 3 {
		t.Fatalf("unrelated folder changed: %v", got)
	}
}

type pausedPublicCopyStore struct {
	media.Store
	key     string
	copying chan struct{}
	resume  chan struct{}
	paused  atomic.Bool
}

func (s *pausedPublicCopyStore) Copy(ctx context.Context, src, dst string, opts media.CopyOptions) (media.Object, error) {
	if dst == s.key && s.paused.CompareAndSwap(false, true) {
		close(s.copying)
		select {
		case <-s.resume:
		case <-ctx.Done():
			return media.Object{}, ctx.Err()
		}
	}
	return s.Store.Copy(ctx, src, dst, opts)
}

func TestPublicSyncCannotUndoCompletedHide(t *testing.T) {
	env := s3test.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	store := &pausedPublicCopyStore{Store: env.Store, copying: make(chan struct{}), resume: make(chan struct{})}
	resume := sync.OnceFunc(func() { close(store.resume) })
	var workers sync.WaitGroup
	defer func() { resume(); cancel(); workers.Wait() }()
	kinds, err := media.NewRegistry(media.Kind{Name: "clip", Video: &media.Video{}})
	if err != nil {
		t.Fatal(err)
	}
	ref := contentref.New(env.Tenant, "clip", cid(2))
	item, _ := kinds.Item(ref)
	blob := blobName("poster")
	private, _ := item.Private(blob)
	store.key, _ = item.Public(blob)
	manifests := s3test.Manifests(t, store, kinds, media.ManifestOptions{})
	if err := manifests.UpdateSlot(ctx, ref, media.PosterSlot, func(rec *media.SlotRecord) error {
		*rec = media.SlotRecord{Original: blobName("frame"), Result: &media.SlotResult{Source: blobName("frame"),
			Outputs: []media.SlotRendition{{Rung: 480, W: 480, H: 270, Blob: blob}}}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	putObject(t, env.Store, private, "poster")
	jobs, err := media.NewJobs(media.JobsConfig{Store: store, Kinds: kinds, Resolver: &flipVisible{}, Locker: s3test.Locker(t, store)})
	if err != nil {
		t.Fatal(err)
	}
	synced := make(chan error, 1)
	workers.Go(func() { _, err := manifests.SyncPublic(ctx, ref); synced <- err })
	select {
	case <-store.copying:
	case err := <-synced:
		t.Fatalf("sync never reached public copy: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	hideCtx, cancelHide := context.WithTimeout(ctx, time.Second)
	hideErr := jobs.Expose(hideCtx, ref)
	cancelHide()
	resume()
	if err := <-synced; err != nil {
		t.Fatal(err)
	}
	if hideErr == nil {
		if _, err := env.Store.Head(ctx, store.key); err == nil {
			t.Error("stale public sync restored an object after hide completed")
		} else {
			t.Fatalf("hide did not wait for the in-flight copy: %v", err)
		}
	} else if !errors.Is(hideErr, context.DeadlineExceeded) {
		t.Fatal(hideErr)
	}
	if err := jobs.Expose(ctx, ref); err != nil {
		t.Fatal(err)
	}
	if _, err := env.Store.Head(ctx, store.key); !errors.Is(err, media.ErrNotFound) {
		t.Fatalf("completed hide left public object: %v", err)
	}
	if _, err := env.Store.Head(ctx, private); err != nil {
		t.Fatalf("hide removed the private rendition: %v", err)
	}
}

// TestExposeTxThroughRiver hides and unhides an item from a host
// transaction through the composed River client: its cover's public/ copy is
// deleted (and reported for a CDN purge), then copied back.
func TestExposeTxThroughRiver(t *testing.T) {
	env := s3test.Open(t)
	ctx := context.Background()
	r, err := media.NewRegistry(media.Kind{Name: "clip", Video: &media.Video{}})
	if err != nil {
		t.Fatal(err)
	}
	res := &flipVisible{}
	res.visible.Store(true)
	var mu sync.Mutex
	var purged []string
	jobs, err := media.NewJobs(media.JobsConfig{Store: env.Store, Kinds: r, Resolver: res, Locker: s3test.Locker(t, env.Store),
		Hooks: media.Hooks{PublicRemoved: func(_ context.Context, _ contentref.ContentRef, keys []string) {
			mu.Lock()
			defer mu.Unlock()
			purged = append(purged, keys...)
		}}})
	if err != nil {
		t.Fatal(err)
	}
	_, pool, _ := riverHost(t, jobs, make(chan string, 4))
	ref := contentref.New(env.Tenant, "clip", cid(1))
	item, _ := r.Item(ref)
	blob := blobName("poster")
	private, _ := item.Private(blob)
	public, _ := item.Public(blob)
	ms := s3test.Manifests(t, env.Store, r, media.ManifestOptions{})
	if err := ms.UpdateSlot(ctx, ref, media.PosterSlot, func(rec *media.SlotRecord) error {
		*rec = media.SlotRecord{Original: blobName("frame"), Result: &media.SlotResult{Source: blobName("frame"),
			Outputs: []media.SlotRendition{{Rung: 480, W: 480, H: 270, Blob: blob}}}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	putObject(t, env.Store, private, "poster") // the image job renders after the commit
	expose := func() {
		t.Helper()
		// Twice in one transaction: exposes are not deduplicated.
		if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			if err := jobs.ExposeTx(ctx, tx, ref); err != nil {
				return err
			}
			return jobs.ExposeTx(ctx, tx, ref)
		}); err != nil {
			t.Fatal(err)
		}
	}
	exists := func() bool { return slices.Contains(listKeys(t, env.Store, item.PublicPrefix()), public) }
	expose()
	waitFor(t, "cover exposed", exists)
	res.visible.Store(false)
	expose()
	waitFor(t, "cover hidden", func() bool { return !exists() })
	root, _, err := ms.Root(ctx, ref)
	if err != nil || !root.Hidden {
		t.Fatalf("hidden not recorded: %v", err)
	}
	mu.Lock()
	if !slices.Contains(purged, public) {
		t.Fatalf("purge hook got %v", purged)
	}
	mu.Unlock()
	if !slices.Contains(listKeys(t, env.Store, item.PrivatePrefix()), private) {
		t.Fatal("hiding touched private/")
	}
	res.visible.Store(true)
	expose()
	waitFor(t, "cover unhidden", exists)
}

type flipVisible struct{ visible atomic.Bool }

func (f *flipVisible) Resolve(_ context.Context, refs []contentref.ContentRef, _ access.Actor) (map[contentref.ContentKey]access.Resolution, error) {
	out := map[contentref.ContentKey]access.Resolution{}
	for _, ref := range refs {
		out[ref.Key()] = access.Resolution{Visible: f.visible.Load()}
	}
	return out, nil
}
