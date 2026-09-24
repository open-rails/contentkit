package media_test

import (
	"context"
	"crypto/sha256"
	"net/http"
	"slices"
	"strings"
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
	jobs, err := media.NewJobs(media.JobsConfig{Store: env.Store, Kinds: r, Tenants: []string{env.Tenant}, Grace: 3 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan string, 4)
	processed := make(chan media.ProcessJob, 4)
	if err := jobs.AddProcessor(func(_ context.Context, job media.ProcessJob) error { processed <- job; return nil }); err != nil {
		t.Fatal(err)
	}
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
	// Jobs is the uploads' ProcessQueue.
	var _ media.ProcessQueue = jobs
	gv := contentref.NewVersion(env.Tenant, "gallery", "4", "v1")
	if err := jobs.Enqueue(ctx, media.ProcessJob{Ref: gv}); err != nil {
		t.Fatal(err)
	}
	if err := jobs.Enqueue(ctx, media.ProcessJob{Ref: contentref.New(env.Tenant, "nope", "1")}); err == nil {
		t.Fatal("unregistered kind enqueued")
	}
	if got := <-processed; !got.Ref.Equal(gv) || got.Slot != "" {
		t.Fatalf("processed %+v", got)
	}

	ms := s3test.Manifests(t, env.Store, r, media.ManifestOptions{Jobs: jobs})
	ref := contentref.New(env.Tenant, "post", "501")
	item, _ := r.Item(ref)
	orphan, kept := item.BlobsPrefix()+blobName("orphan"), item.BlobsPrefix()+blobName("kept")
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
	jobs, err := media.NewJobs(media.JobsConfig{Store: env.Store, Kinds: r, Limiter: limiter, LateUploadWindow: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	_, pool, schema := riverHost(t, jobs, make(chan string, 4))
	s := env.Store

	post, other := contentref.New(env.Tenant, "post", "9"), contentref.New(env.Tenant, "post", "10")
	g := contentref.New(env.Tenant, "gallery", "3")
	for _, ref := range []contentref.ContentRef{post, other, g} {
		item, _ := r.Item(ref)
		putObject(t, s, item.BlobsPrefix()+blobName("b"), "b")
		putObject(t, s, item.OriginalsPrefix()+blobName("o"), "o")
	}
	// The post's commits charged its owner 700 bytes of originals; a gallery version 50.
	pi, _ := r.Item(post)
	putObject(t, s, pi.Prefix()+"manifest.json", `{"files":[{"name":"a","original":"`+blobName("o")+`","size":300},`+
		`{"name":"b","original":"`+blobName("o2")+`","size":400},{"name":"a2","original":"`+blobName("o")+`","size":300}]}`)
	oi, _ := r.Item(other)
	putObject(t, s, oi.Prefix()+"manifest.json", `{"files":[]}`)
	gi0, _ := r.Item(g)
	putObject(t, s, gi0.ManifestsPrefix()+"v1.json", `{"files":[{"name":"p","original":"`+blobName("o")+`","size":50}]}`)
	if err := limiter.Settle(ctx, media.Settlement{Tenant: env.Tenant, Owner: "chan-a", Delta: 1000}); err != nil {
		t.Fatal(err)
	}
	user, _ := r.Item(contentref.New(env.Tenant, "user", "u1"))
	putObject(t, s, user.OriginalsPrefix()+"avatar", "avatar original")
	putObject(t, s, user.PublicPrefix()+"avatar_80.webp", "avatar")

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
		return jobs.EraseUserTx(ctx, tx, env.Tenant, "u1", media.Deletion{Ref: post, Owner: "chan-a"}, media.Deletion{Ref: g, Owner: "chan-a"})
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

// A commit that lands after a processor read the manifest, while its job is
// running, is absorbed by that job and must still be processed.
func TestProcessingRerunsWhenACommitLandsDuringTheRun(t *testing.T) {
	env := s3test.Open(t)
	if !env.Store.Capabilities().ConditionalPut {
		t.Skip("backend lacks conditional PUT")
	}
	ctx := context.Background()
	r := registry(t)
	jobs, err := media.NewJobs(media.JobsConfig{Store: env.Store, Kinds: r})
	if err != nil {
		t.Fatal(err)
	}
	ms := s3test.Manifests(t, env.Store, r, media.ManifestOptions{})
	ref := contentref.New(env.Tenant, "post", "77")
	insert := func(name string) {
		if _, err := ms.Edit(ctx, ref, func(m *media.Manifest) error {
			m.Files = append(m.Files, media.File{Name: name, Original: blobName(name)})
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	seen := make(chan []string, 8)
	var calls atomic.Int32
	if err := jobs.AddProcessor(func(ctx context.Context, job media.ProcessJob) error {
		calls.Add(1)
		man, _, err := ms.Get(ctx, job.Ref)
		if err != nil {
			return err
		}
		var names []string
		for _, f := range man.Files {
			names = append(names, f.Name)
		}
		if calls.Load() == 1 {
			// The late commit: its Enqueue is absorbed by this running job.
			insert("late.jpg")
			if err := jobs.Enqueue(ctx, job); err != nil {
				return err
			}
		}
		seen <- names
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	client, _, _ := riverHost(t, jobs, make(chan string, 4))
	completed, cancel := client.Subscribe(river.EventKindJobCompleted)
	defer cancel()

	insert("first.jpg")
	if err := jobs.Enqueue(ctx, media.ProcessJob{Ref: ref}); err != nil {
		t.Fatal(err)
	}
	next := func() []string {
		select {
		case got := <-seen:
			return got
		case <-time.After(45 * time.Second):
			t.Fatal("timed out waiting for a processor run")
			return nil
		}
	}
	if got := next(); !slices.Equal(got, []string{"first.jpg"}) {
		t.Fatalf("first run saw %v", got)
	}
	if got := next(); !slices.Equal(got, []string{"first.jpg", "late.jpg"}) {
		t.Fatalf("the late commit was not processed: %v", got)
	}
	for ev := range completed {
		if ev.Job.Kind == "contentkit_media_process" {
			break
		}
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("%d processor runs, want 2", n)
	}
}

// TestPublishTxThroughRiver publishes a video poster from a host transaction
// through the composed River client, and unpublishes it the same way.
func TestPublishTxThroughRiver(t *testing.T) {
	env := s3test.Open(t)
	ctx := context.Background()
	r, err := media.NewRegistry(media.Kind{Name: "clip", Video: &media.Video{}})
	if err != nil {
		t.Fatal(err)
	}
	res := &flipVisible{}
	res.visible.Store(true)
	jobs, err := media.NewJobs(media.JobsConfig{Store: env.Store, Kinds: r, Resolver: res})
	if err != nil {
		t.Fatal(err)
	}
	_, pool, _ := riverHost(t, jobs, make(chan string, 4))
	ref := contentref.New(env.Tenant, "clip", "1")
	item, _ := r.Item(ref)
	staged, _ := item.SlotOutput(media.PosterSlot, 480)
	public, _ := item.SlotPublic(media.PosterSlot, 480)
	putObject(t, env.Store, staged, "poster")
	publish := func() {
		t.Helper()
		// Twice in one transaction: publishes are not deduplicated.
		if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			if err := jobs.PublishTx(ctx, tx, ref); err != nil {
				return err
			}
			return jobs.PublishTx(ctx, tx, ref)
		}); err != nil {
			t.Fatal(err)
		}
	}
	exists := func() bool { return slices.Contains(listKeys(t, env.Store, item.PublicPrefix()), public) }
	publish()
	waitFor(t, "poster published", exists)
	res.visible.Store(false)
	publish()
	waitFor(t, "poster unpublished", func() bool { return !exists() })
	// Unpublished leaves nothing of its own behind (a deleted item's folder stays empty).
	if keys := listKeys(t, env.Store, item.Prefix()); len(keys) != 1 || keys[0] != staged {
		t.Fatalf("after unpublishing: %v", keys)
	}
}

type flipVisible struct{ visible atomic.Bool }

func (f *flipVisible) Resolve(context.Context, contentref.ContentRef, access.Actor) (access.Resolution, error) {
	return access.Resolution{Visible: f.visible.Load()}, nil
}
