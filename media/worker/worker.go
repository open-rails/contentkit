// Package worker is the media worker: the one process that places uploads
// and runs every producer, from the host's worker River schema
// (Config.Schema) in its database: staged uploads hashed and placed at their
// blobs (media.Manifests.Place), images, zips, public presets and editor
// views (media/image, libvips), and HLS, MP4, subtitles and frames
// (media/video, ffmpeg).
// The host presigns, commits and reads. Each processing attempt records its
// retained progress/completion fact and wakes version metadata work in one
// transaction. Stock workers need no host callbacks.
//
// The registry is the host's (media.Config, built by the same code or read
// from its JSON); cmd/media-worker is the stock build for hosts whose kinds
// are plain data.
package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/helpers/deps"
	riverhelpers "github.com/open-rails/helpers/river"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/image"
	"github.com/open-rails/contentkit/media/versions"
	"github.com/open-rails/contentkit/media/video"
	"github.com/open-rails/contentkit/media/workqueue"
)

// Config configures the worker.
type Config struct {
	Pool *pgxpool.Pool // the host database: Schema, manifest locks, progress
	// Schema is the host's worker River schema, the one its workqueue.Queue
	// inserts into; required, and never shared with another host.
	Schema string
	// ContentSchema holds ContentKit's media journal and quota tables. Every
	// writer for this host must use the same schema; migration is host-owned.
	ContentSchema string
	// AccountSchema and AccountWorkerSchema are the shared tenant avatar state
	// and queue from contentkit.Config.AccountMedia. Empty retains legacy mode.
	AccountSchema, AccountWorkerSchema string
	Store                              media.Store
	// Kinds is the host's registry. Failed and the catalog retirement barrier
	// and optional completion callbacks run here.
	Kinds *media.Registry
	// HostSchema is the host's River schema ("" is the connection's search path):
	// metadata snapshots, sweeps and purges run there. Grace is the host's
	// JobsConfig.Grace (default 24 h).
	HostSchema string
	Grace      time.Duration

	TempDir string // scratch for video sources and outputs; default os.TempDir()
	Threads int    // ffmpeg threads; default GOMAXPROCS
	// Preset, TopPreset, VideoEncoder (Encoder) and VideoCodecs (Codecs) are
	// video.Config's.
	Preset, TopPreset, VideoEncoder string
	VideoCodecs                     []media.Codec
	// IngestWorkers and DerivativeWorkers bound concurrent item jobs (defaults
	// 2 and 1). A derivative job handles both images and video for its item.
	IngestWorkers     int
	DerivativeWorkers int
	// ImageSources bounds sources decoded at once within one derivative job.
	ImageSources int
	// MaxPixels, MaxFrames and MaxAnimationSeconds bound decoded images
	// (image.Config's defaults when zero).
	MaxPixels           int
	MaxFrames           int
	MaxAnimationSeconds float64
	// JobTimeout bounds one ingest or derivative attempt (default 1 h).
	JobTimeout time.Duration
	// ShutdownGrace is how long running jobs get to finish on shutdown
	// before they are cancelled and retried elsewhere; default 30 s.
	ShutdownGrace time.Duration
	Logger        *slog.Logger
	// RiverHooks are added to the worker's River client (observability).
	RiverHooks []rivertype.Hook
	Metrics    *Metrics
}

func (c *Config) defaults() error {
	if c.Pool == nil || c.Store == nil || c.Kinds == nil {
		return errors.New("media/worker: Config needs a Pool, a Store and the host's Kinds")
	}
	if err := workqueue.ValidSchema(c.Schema); err != nil {
		return err
	}
	if c.ContentSchema == "" {
		return errors.New("media/worker: Config.ContentSchema is required")
	}
	if c.DerivativeWorkers <= 0 {
		c.DerivativeWorkers = 1
	}
	if c.IngestWorkers <= 0 {
		c.IngestWorkers = 2
	}
	if c.JobTimeout <= 0 {
		c.JobTimeout = time.Hour
	}
	if c.ShutdownGrace <= 0 {
		c.ShutdownGrace = 30 * time.Second
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	return nil
}

// Worker is a configured worker; Run drains jobs until shutdown.
type Worker struct {
	c       Config
	account *Worker
	client  *river.Client[pgx.Tx]
}

// New builds the River client with the image and video workers. It needs no
// DDL rights: the host applies workqueue.Migrate(Schema) in its migration
// step, so the worker can run as the host's unprivileged app role. Missing
// encode tables are reported before the worker is built.
func New(ctx context.Context, c Config) (*Worker, error) {
	if err := c.defaults(); err != nil {
		return nil, err
	}
	for _, table := range []string{"encode_run", "encode_chunk"} {
		var exists bool
		name := pgx.Identifier{c.Schema, table}.Sanitize()
		if err := c.Pool.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL", name).Scan(&exists); err != nil {
			return nil, fmt.Errorf("media/worker: check %s.%s: %w", c.Schema, table, err)
		}
		if !exists {
			return nil, fmt.Errorf("media/worker: missing %s.%s; the host must run workqueue.Migrate",
				c.Schema, table)
		}
	}
	for _, object := range []string{"content_media_commits", "content_media_processing", "eventlog_pending", "content_event_ids"} {
		name := pgx.Identifier{c.ContentSchema, object}.Sanitize()
		var exists bool
		if err := c.Pool.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL", name).Scan(&exists); err != nil {
			return nil, fmt.Errorf("media/worker: check completion storage: %w", err)
		}
		if !exists {
			return nil, fmt.Errorf("media/worker: missing %s; the host must apply ContentKit migrations", name)
		}
	}
	if err := video.SweepTemp(c.TempDir); err != nil {
		return nil, fmt.Errorf("media/worker: sweep scratch: %w", err)
	}
	publish, err := versions.ProcessingPublisher(c.Pool, c.HostSchema, c.ContentSchema, c.Kinds)
	if err != nil {
		return nil, err
	}
	queue, err := workqueue.NewWithAccounts(c.Pool, c.Kinds, c.Schema, c.AccountWorkerSchema)
	if err != nil {
		return nil, err
	}
	journal, err := media.NewPGJournal(c.Pool, c.ContentSchema, queue)
	if err != nil {
		return nil, err
	}
	if c.AccountSchema != "" {
		scope, ok := c.Kinds.Scope()
		if !ok || c.AccountWorkerSchema == "" {
			return nil, errors.New("media/worker: account storage requires scoped kinds and queue")
		}
		account, err := media.NewPGJournal(c.Pool, c.AccountSchema, queue)
		if err != nil {
			return nil, err
		}
		scope.AppID = ""
		journal, err = media.RouteJournal(journal, account, scope.Partition())
		if err != nil {
			return nil, err
		}
	}
	if err := media.ValidateVersionStorageMigration(ctx, c.Kinds, journal); err != nil {
		return nil, fmt.Errorf("media/worker: version storage migration required: %w", err)
	}
	// A stock worker can replace published renditions too. Install the same
	// synchronous catalog barrier as embedded writers before any DELETE.
	registryConfig := c.Kinds.Config()
	registryConfig.Hooks.CanUpload = preparationUploads{}

	registryConfig.Hooks.BeforeRetire = versions.RetirementBarrier(c.Pool, c.ContentSchema, c.Kinds)
	paths := versions.ChapterPathHooks(c.Pool, c.ContentSchema, c.Kinds, publish)
	registryConfig.Hooks.ValidatePathsChanged = paths.ValidatePathsChanged
	registryConfig.Hooks.PathsChanged = paths.PathsChanged
	c.Kinds, err = media.NewRegistry(registryConfig)
	if err != nil {
		return nil, err
	}
	manifests, err := media.NewManifests(c.Store, c.Kinds, media.ManifestOptions{Locker: media.PGLocker(c.Pool), Journal: journal, CleanupGrace: c.Grace})
	if err != nil {
		return nil, err
	}
	if err := manifests.ValidatePublicationLayoutMigration(ctx); err != nil {
		return nil, fmt.Errorf("media/worker: publication layout migration required: %w", err)
	}
	uploads, err := media.NewUploads(media.UploadOptions{Store: c.Store, Manifests: manifests})
	if err != nil {
		return nil, err
	}
	prepare := versions.PrepareDerivatives(c.Pool, c.ContentSchema, manifests, uploads)
	var observeEncode func(video.EncodeObservation)
	if c.Metrics != nil {
		observeEncode = c.Metrics.ObserveEncode
	}
	enc, err := video.New(ctx, video.Config{Manifests: manifests, Queue: queue, TempDir: c.TempDir,
		Threads: c.Threads, Preset: c.Preset, TopPreset: c.TopPreset, Encoder: c.VideoEncoder, Codecs: c.VideoCodecs, Logger: c.Logger,
		ObserveEncode: observeEncode})
	if err != nil {
		return nil, err
	}
	hooks := append(c.RiverHooks[:len(c.RiverHooks):len(c.RiverHooks)], &completionHook{manifests: manifests, publish: publish})
	var middleware []rivertype.Middleware
	if c.Metrics != nil {
		middleware = []rivertype.Middleware{c.Metrics}
	}
	images, err := image.New(image.Config{Store: c.Store, Manifests: manifests, Workers: c.ImageSources, MaxPixels: c.MaxPixels, MaxFrames: c.MaxFrames, MaxAnimationSeconds: c.MaxAnimationSeconds})
	if err != nil {
		return nil, err
	}
	processing := riverhelpers.NewContribution("contentkit-media-processing", func(_ context.Context, cfg *river.Config) error {
		for _, q := range []string{workqueue.IngestQueue, workqueue.DerivativeQueue} {
			if _, exists := cfg.Queues[q]; exists {
				return fmt.Errorf("media/worker: queue %q already registered", q)
			}
		}
		cfg.Queues[workqueue.IngestQueue] = river.QueueConfig{MaxWorkers: c.IngestWorkers}
		cfg.Queues[workqueue.DerivativeQueue] = river.QueueConfig{MaxWorkers: c.DerivativeWorkers}
		if err := river.AddWorkerSafely(cfg.Workers, &ingestWorker{c: c, manifests: manifests, queue: queue}); err != nil {
			return err
		}
		return river.AddWorkerSafely(cfg.Workers, &derivativeWorker{c: c, images: images, manifests: manifests, prepare: prepare, video: video.WorkerConfig{Encoder: enc, Pool: c.Pool, Schema: c.Schema, Logger: c.Logger}})
	}, nil, nil)
	client, err := riverhelpers.New(ctx, c.Pool, &river.Config{Schema: c.Schema,
		JobTimeout: time.Hour, RescueStuckJobsAfter: 2 * time.Hour,
		Logger: c.Logger, Hooks: hooks, Middleware: middleware}, processing)
	if err != nil {
		return nil, err
	}
	out := &Worker{c: c, client: client}
	if c.AccountSchema != "" {
		child := c
		child.ContentSchema = c.AccountSchema
		child.Schema = c.AccountWorkerSchema
		child.AccountSchema = ""
		child.AccountWorkerSchema = ""
		out.account, err = New(ctx, child)
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// checkTimeout bounds one readiness check, so a bucket that accepts
// connections and never answers cannot wedge Run.
const checkTimeout = 10 * time.Second

// Run waits for the bucket (every job needs it; waiting burns no job
// attempts), then works jobs until ctx is done. Running jobs get ShutdownGrace before
// cancellation. A non-nil error may leave jobs unwinding: the caller must not
// close their database pool until the worker's River client has stopped.
func (w *Worker) Run(ctx context.Context) error {
	if w.account != nil {
		run, cancel := context.WithCancel(ctx)
		defer cancel()
		out := make(chan error, 2)
		own := *w
		own.account = nil
		go func() { out <- own.Run(run) }()
		go func() { out <- w.account.Run(run) }()
		first := <-out
		cancel()
		return errors.Join(first, <-out)
	}

	for attempt := 0; ; attempt++ {
		checkCtx, cancel := context.WithTimeout(ctx, checkTimeout)
		err := w.c.Store.Check(checkCtx)
		cancel()
		if err == nil {
			break
		}
		wait := deps.Backoff(attempt, 500*time.Millisecond, 30*time.Second)
		w.c.Logger.Warn("media-worker: waiting for the bucket", "attempt", attempt+1, "retry_in", wait.Round(time.Millisecond), "error", err.Error())
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
	}
	// River ends on its context's cancellation only: a context that ends by
	// deadline makes its notifier retry without backoff. Pass ctx's end on
	// as a cancellation.
	runCtx, cancelRun := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelRun()
	stopRun := context.AfterFunc(ctx, cancelRun)
	defer stopRun()
	if err := w.client.Start(runCtx); err != nil {
		return err
	}
	w.c.Logger.Info("media-worker: started", "schema", w.c.Schema, "derivative_workers", w.c.DerivativeWorkers, "ingest_workers", w.c.IngestWorkers)
	<-ctx.Done()
	return stopJobs(w.client, w.c.ShutdownGrace, 30*time.Second)
}

// stopJobs allows a short graceful drain, then interrupts work and waits for
// River to acknowledge it. Its error must retain the unfinished-work signal.
func stopJobs(client *river.Client[pgx.Tx], grace, hardLimit time.Duration) error {
	soft, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	if err := client.Stop(soft); err == nil {
		return nil
	}
	hard, cancelHard := context.WithTimeout(context.Background(), hardLimit)
	defer cancelHard()
	if err := client.StopAndCancel(hard); err != nil {
		return fmt.Errorf("media-worker: shutdown incomplete: %w", err)
	}
	return nil
}

type ingestWorker struct {
	river.WorkerDefaults[workqueue.IngestArgs]
	c         Config
	manifests *media.Manifests
	queue     *workqueue.Queue
}

func (w *ingestWorker) Timeout(*river.Job[workqueue.IngestArgs]) time.Duration {
	return w.c.JobTimeout
}

// Work places an item's staged uploads, after any equal job it follows, then
// enqueues its processing.
func (w *ingestWorker) Work(ctx context.Context, job *river.Job[workqueue.IngestArgs]) (err error) {
	if err := w.c.restoreRescuedAttempt(ctx, job.JobRow); err != nil {
		return err
	}
	fail := func(ctx context.Context, cause error) error {
		if err := full(ctx, w.manifests, job.Args.Ref, w.manifests.FailIngest(ctx, job.Args.Ref, cause)); err != nil {
			return err
		}
		return w.queue.Enqueue(ctx, media.ProcessJob{Ref: job.Args.Ref, Preset: job.Args.Preset})
	}
	if err := w.c.checkAttemptBudget(ctx, job.JobRow, fail); err != nil {
		return err
	}
	defer func() { err = w.c.finishAttempt(ctx, job.JobRow, err, fail) }()
	a := job.Args
	item, err := w.c.Kinds.Item(a.Ref)
	if err != nil {
		return river.JobCancel(err)
	}
	if err := media.WaitFor(ctx, river.ClientFromContext[pgx.Tx](ctx), a.After); err != nil {
		return err
	}
	if a.Commit != "" {
		if err := w.manifests.CompleteCommit(ctx, a.Ref, a.Commit); err != nil {
			return err
		}
	}
	if err := w.manifests.Recover(ctx, a.Ref); err != nil {
		return err
	}
	if _, err := w.manifests.Place(ctx, a.Ref); err != nil {
		return full(ctx, w.manifests, a.Ref, err)
	}
	if err := w.manifests.IngestArchives(ctx, a.Ref, w.c.TempDir); err != nil {
		return full(ctx, w.manifests, a.Ref, err)
	}
	if !a.Force && a.Preset == "" {
		m, _, err := w.manifests.Get(ctx, a.Ref)
		if err != nil {
			return err
		}
		if !item.Kind().NeedsProcessing(m) {
			return nil // Placed audio needs no image/video job; staged images still do.
		}
	}
	return w.queue.Enqueue(ctx, media.ProcessJob{Ref: a.Ref, Preset: a.Preset, Force: a.Force})
}

type derivativeWorker struct {
	river.WorkerDefaults[workqueue.DerivativeArgs]
	c         Config
	images    *image.Processor
	manifests *media.Manifests
	video     video.WorkerConfig
	prepare   func(context.Context, contentref.ContentRef) error
}

func (w *derivativeWorker) Timeout(*river.Job[workqueue.DerivativeArgs]) time.Duration {
	return w.c.JobTimeout
}

// Work generates image outputs and advances video outputs through their existing
// resumable chunks. Both formats share this one durable item obligation.
func (w *derivativeWorker) Work(ctx context.Context, job *river.Job[workqueue.DerivativeArgs]) (err error) {
	if err := w.c.restoreRescuedAttempt(ctx, job.JobRow); err != nil {
		return err
	}
	fail := func(ctx context.Context, cause error) error {
		return full(ctx, w.manifests, job.Args.Ref, w.images.FailPending(ctx, media.ProcessJob{
			Ref: job.Args.Ref, Preset: job.Args.Preset, Editor: job.Args.Editor,
		}, cause))
	}
	if err := w.c.checkAttemptBudget(ctx, job.JobRow, fail); err != nil {
		return err
	}
	defer func() { err = w.c.finishAttempt(ctx, job.JobRow, err, fail) }()
	a := job.Args
	pj := media.ProcessJob{Ref: a.Ref, Preset: a.Preset, Force: a.Force, Editor: a.Editor}
	if _, err := w.c.Kinds.Item(pj.Ref); err != nil {
		return river.JobCancel(err)
	}
	if err := media.WaitFor(ctx, river.ClientFromContext[pgx.Tx](ctx), job.Args.After); err != nil {
		return err
	}
	if err := w.manifests.Recover(ctx, pj.Ref); err != nil {
		return err
	}
	if !a.Editor {
		if err := w.prepare(ctx, a.Ref); err != nil {
			return err
		}
	}
	var checkpoint struct {
		ImagesForced bool `json:"contentkit_images_forced"`
	}
	if err := json.Unmarshal(job.Metadata, &checkpoint); err != nil {
		return err
	}
	pj.Force = a.Force && !checkpoint.ImagesForced
	if err := full(ctx, w.manifests, pj.Ref, w.images.Process(ctx, pj)); err != nil {
		return err
	}
	if pj.Force {
		if _, err := w.c.Pool.Exec(ctx, `UPDATE `+pgx.Identifier{w.c.Schema, "river_job"}.Sanitize()+` SET metadata=jsonb_set(metadata,'{contentkit_images_forced}','true') WHERE id=$1`, job.ID); err != nil {
			return err
		}
	}
	if a.Editor {
		return nil
	}
	// Image analysis may have established page selection or changed the cover
	// source. Prepare those inputs, then finish their image outputs in this job.
	if err := w.prepare(ctx, a.Ref); err != nil {
		return err
	}
	pj.Force = false
	if err := full(ctx, w.manifests, pj.Ref, w.images.Process(ctx, pj)); err != nil {
		return err
	}
	return w.video.Run(ctx, job)
}

// full marks the item Full when a record of its outputs did not fit
// (ErrManifestTooLarge) and ends the job: a retry would redo the work only
// to be refused again.
func full(ctx context.Context, ms *media.Manifests, ref contentref.ContentRef, err error) error {
	if !errors.Is(err, media.ErrManifestTooLarge) {
		return err
	}
	return river.JobCancel(errors.Join(err, ms.SetFull(ctx, ref, err)))
}

// preparationUploads authorizes only internal same-version cover preparation;
// this worker exposes no HTTP upload interface.
type preparationUploads struct{}

func (preparationUploads) CanUpload(_ context.Context, a access.Actor, _ media.UploadTarget) (media.UploadGrant, error) {
	return media.UploadGrant{Allowed: a.ID == "contentkit" && a.Kind == "contentkit:system" && !a.Anonymous, Exempt: true}, nil
}
