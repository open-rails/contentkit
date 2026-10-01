// Package worker is the media worker: the one process that places uploads
// and runs every producer, from the host's worker River schema
// (Config.Schema) in its database: staged uploads hashed and placed at their
// blobs (media.Manifests.Place), images, zips, public presets and editor
// views (media/image, libvips), and HLS, MP4, audio, subtitles and frames
// (media/video, ffmpeg).
// The host only presigns, commits, exposes and reads. After each job the
// worker asks the host, through its media queue, to report the item's
// readiness (Hooks.ItemReady) and to purge the public URLs it changed
// (Hooks.PurgePublic), so the stock worker needs no host code.
//
// The registry is the host's (media.Config, built by the same code or read
// from its JSON); cmd/media-worker is the stock build for hosts whose kinds
// are plain data.
package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/helpers/deps"
	riverhelpers "github.com/open-rails/helpers/river"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/image"
	"github.com/open-rails/contentkit/media/video"
	"github.com/open-rails/contentkit/media/workqueue"
)

// Config configures the worker.
type Config struct {
	Pool *pgxpool.Pool // the host database: Schema, manifest locks, progress
	// Schema is the host's worker River schema, the one its workqueue.Queue
	// inserts into; required, and never shared with another host.
	Schema string
	// Queue selects a one-task process: encode runs video chunks; light runs
	// video planning/assembly plus the existing image and audio queues.
	// Empty keeps the long-running all-queue worker.
	Queue string
	Store media.Store
	// Kinds is the host's registry. Only Hooks.Failed runs here; ItemReady
	// and PurgePublic run in the host through its media queue.
	Kinds *media.Registry
	// HostSchema and HostQueue are the host's River schema ("" is the
	// connection's search path) and media queue (default media.DefaultQueue):
	// sweeps, readiness and purges run there. Grace is the host's
	// JobsConfig.Grace (default 24 h).
	HostSchema string
	HostQueue  string
	Grace      time.Duration

	TempDir string // scratch for video sources and outputs; default os.TempDir()
	Threads int    // ffmpeg threads; default GOMAXPROCS
	// Preset, TopPreset, VideoEncoder (Encoder) and VideoCodecs (Codecs) are
	// video.Config's.
	Preset, TopPreset, VideoEncoder string
	VideoCodecs                     []media.Codec
	// VideoWorkers and ImageWorkers are concurrent jobs per process
	// (defaults 1 and 2; ImageWorkers also bounds place jobs); ImageSources
	// bounds sources decoded at once per image job (default 2).
	VideoWorkers int
	AudioWorkers int // default 2
	ImageWorkers int
	ImageSources int
	// MaxPixels, MaxFrames and MaxAnimationSeconds bound decoded images
	// (image.Config's defaults when zero).
	MaxPixels           int
	MaxFrames           int
	MaxAnimationSeconds float64
	// VideoTimeout bounds audio-only encoding (default 48 h); video chunks,
	// planning and assembly each have a one-hour timeout. ImageTimeout bounds
	// one image job (default 1 h).
	VideoTimeout time.Duration
	ImageTimeout time.Duration
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
	if c.Queue != "" && c.Queue != workqueue.VideoLightQueue && c.Queue != workqueue.VideoEncodeQueue {
		return fmt.Errorf("media/worker: unsupported queue %q", c.Queue)
	}
	if c.VideoWorkers <= 0 {
		c.VideoWorkers = 1
	}
	if c.Queue != "" {
		c.VideoWorkers = 1
		c.AudioWorkers = 1
		c.ImageWorkers = 1
	}
	if c.ImageWorkers <= 0 {
		c.ImageWorkers = 2
	}
	if c.VideoTimeout <= 0 {
		c.VideoTimeout = 48 * time.Hour
	}
	if c.ImageTimeout <= 0 {
		c.ImageTimeout = time.Hour
	}
	if c.ShutdownGrace <= 0 {
		c.ShutdownGrace = 30 * time.Second
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	return nil
}

// Worker is a configured worker; Run drains jobs or exits after one queued job.
type Worker struct {
	c             Config
	client        *river.Client[pgx.Tx]
	singleStarted *atomic.Bool
	singleDone    <-chan struct{}
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
	// One-shot workers use pod-private scratch, which Kubernetes removes with
	// the pod. Sweeping a shared path here could erase another active worker's
	// chunk when multiple one-shot processes start on the same host.
	if c.Queue == "" {
		if err := video.SweepTemp(c.TempDir); err != nil {
			return nil, fmt.Errorf("media/worker: sweep scratch: %w", err)
		}
	}
	host, err := media.NewHostQueue(c.Pool, c.Kinds, c.HostSchema, c.HostQueue, c.Grace)
	if err != nil {
		return nil, err
	}
	manifests, err := media.NewManifests(c.Store, c.Kinds, media.ManifestOptions{Locker: media.PGLocker(c.Pool), Sweeps: host})
	if err != nil {
		return nil, err
	}
	queue, err := workqueue.New(c.Pool, c.Kinds, c.Schema)
	if err != nil {
		return nil, err
	}
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
	videos, err := video.Contribution(video.WorkerConfig{Encoder: enc, Pool: c.Pool, Schema: c.Schema, Timeout: c.VideoTimeout,
		MaxWorkers: c.VideoWorkers, AudioWorkers: c.AudioWorkers, Queue: c.Queue, Logger: c.Logger})
	if err != nil {
		return nil, err
	}
	hooks := append(c.RiverHooks[:len(c.RiverHooks):len(c.RiverHooks)], &readyHook{manifests: manifests, host: host})
	var middleware []rivertype.Middleware
	if c.Metrics != nil {
		middleware = []rivertype.Middleware{c.Metrics}
	}
	contributions := []riverhelpers.Contribution{videos}
	var singleStarted *atomic.Bool
	var singleDone <-chan struct{}
	if c.Queue != workqueue.VideoEncodeQueue {
		images, err := image.New(image.Config{Store: c.Store, Manifests: manifests, Purge: host.Purge,
			Workers: c.ImageSources, MaxPixels: c.MaxPixels, MaxFrames: c.MaxFrames, MaxAnimationSeconds: c.MaxAnimationSeconds})
		if err != nil {
			return nil, err
		}
		imageJobs := riverhelpers.NewContribution("contentkit-media-image", func(_ context.Context, cfg *river.Config) error {
			for _, q := range []string{workqueue.PlaceQueue, workqueue.ImageQueue} {
				if _, ok := cfg.Queues[q]; ok {
					return fmt.Errorf("media/worker: queue %q already registered", q)
				}
			}
			cfg.Queues[workqueue.PlaceQueue] = river.QueueConfig{MaxWorkers: c.ImageWorkers}
			cfg.Queues[workqueue.ImageQueue] = river.QueueConfig{MaxWorkers: c.ImageWorkers}
			if err := river.AddWorkerSafely(cfg.Workers, &placeWorker{c: c, manifests: manifests, queue: queue}); err != nil {
				return err
			}
			return river.AddWorkerSafely(cfg.Workers, &imageWorker{c: c, images: images, manifests: manifests})
		}, nil, nil)
		contributions = append(contributions, imageJobs)
	}
	if c.Queue != "" {
		started := &atomic.Bool{}
		done := make(chan struct{})
		singleStarted, singleDone = started, done
		middleware = append(middleware, river.WorkerMiddlewareFunc(func(ctx context.Context, _ *rivertype.JobRow, work func(context.Context) error) error {
			if !started.CompareAndSwap(false, true) {
				return river.JobSnooze(5 * time.Second)
			}
			defer close(done)
			return work(ctx)
		}))
	}
	client, err := riverhelpers.New(ctx, c.Pool, &river.Config{Schema: c.Schema,
		JobTimeout: time.Hour, RescueStuckJobsAfter: 2 * time.Hour,
		Logger: c.Logger, Hooks: hooks, Middleware: middleware}, contributions...)
	if err != nil {
		return nil, err
	}
	return &Worker{c: c, client: client, singleStarted: singleStarted, singleDone: singleDone}, nil
}

// Client is the worker's River client (tests subscribe to its events).
func (w *Worker) Client() *river.Client[pgx.Tx] { return w.client }

// checkTimeout bounds one readiness check, so a bucket that accepts
// connections and never answers cannot wedge Run.
const checkTimeout = 10 * time.Second

// Run waits for the bucket (every job needs it; waiting burns no job
// attempts), then works jobs until ctx is done. Single-queue workers exit
// after one task or one idle minute. Running jobs get ShutdownGrace before
// cancellation.
func (w *Worker) Run(ctx context.Context) error {
	for attempt := 0; ; attempt++ {
		checkCtx, cancel := context.WithTimeout(ctx, checkTimeout)
		err := w.c.Store.Check(checkCtx, "_media-worker/")
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
	if err := w.client.Start(ctx); err != nil {
		return err
	}
	w.c.Logger.Info("media-worker: started", "schema", w.c.Schema, "video_workers", w.c.VideoWorkers, "image_workers", w.c.ImageWorkers)
	if w.c.Queue == "" {
		<-ctx.Done()
	} else {
		idle := time.NewTimer(time.Minute)
		defer idle.Stop()
		select {
		case <-ctx.Done():
		case <-w.singleDone:
		case <-idle.C:
			if w.singleStarted.Load() {
				select {
				case <-ctx.Done():
				case <-w.singleDone:
				}
			}
		}
	}
	soft, cancel := context.WithTimeout(context.Background(), w.c.ShutdownGrace)
	defer cancel()
	if err := w.client.Stop(soft); err == nil {
		return nil
	}
	hard, cancelHard := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelHard()
	if err := w.client.StopAndCancel(hard); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return nil
}

type placeWorker struct {
	river.WorkerDefaults[workqueue.PlaceArgs]
	c         Config
	manifests *media.Manifests
	queue     *workqueue.Queue
}

func (w *placeWorker) Timeout(*river.Job[workqueue.PlaceArgs]) time.Duration { return w.c.ImageTimeout }

// Work places an item's staged uploads, after any equal job it follows, then
// enqueues its processing.
func (w *placeWorker) Work(ctx context.Context, job *river.Job[workqueue.PlaceArgs]) (err error) {
	defer func() { err = media.SnoozeUnavailable(ctx, w.c.Store, job.JobRow, err) }()
	a := job.Args
	if _, err := w.c.Kinds.Item(a.Ref); err != nil {
		return river.JobCancel(err)
	}
	if err := media.WaitFor(ctx, river.ClientFromContext[pgx.Tx](ctx), a.After); err != nil {
		return err
	}
	if _, err := w.manifests.Place(ctx, a.Ref); err != nil {
		return full(ctx, w.manifests, a.Ref, err)
	}
	return w.queue.Enqueue(ctx, media.ProcessJob{Ref: a.Ref, Preset: a.Preset, Force: a.Force})
}

type imageWorker struct {
	river.WorkerDefaults[workqueue.ImageArgs]
	c         Config
	images    *image.Processor
	manifests *media.Manifests
}

func (w *imageWorker) Timeout(*river.Job[workqueue.ImageArgs]) time.Duration { return w.c.ImageTimeout }

// Work runs one image job, after any equal job it follows.
func (w *imageWorker) Work(ctx context.Context, job *river.Job[workqueue.ImageArgs]) (err error) {
	defer func() { err = media.SnoozeUnavailable(ctx, w.c.Store, job.JobRow, err) }()
	a := job.Args
	pj := media.ProcessJob{Ref: a.Ref, Preset: a.Preset, Force: a.Force, Editor: a.Editor}
	if _, err := w.c.Kinds.Item(pj.Ref); err != nil {
		return river.JobCancel(err)
	}
	if err := media.WaitFor(ctx, river.ClientFromContext[pgx.Tx](ctx), job.Args.After); err != nil {
		return err
	}
	return full(ctx, w.manifests, pj.Ref, w.images.Process(ctx, pj))
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
