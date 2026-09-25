// Package worker is the media worker: the one process that does all media
// work, from River schema workqueue.Schema in the host database. It hashes a
// staged upload while reading it for processing and places it at its content
// address (media.Manifests.Place), derives image variants, zips, slot outputs
// and inline images (media/image, libvips) and encodes video, posters and
// (media/video, ffmpeg). The host only presigns, commits,
// publishes and reads.
//
// The host builds the worker from the same code that builds its
// media.Registry, image.SpecChooser and media.Hooks, so the worker applies
// exactly the host's kinds, slots and policy (see Config). cmd/media-worker is
// the stock build for hosts whose kinds are plain data.
package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	riverhelpers "github.com/open-rails/helpers/river"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/image"
	"github.com/open-rails/contentkit/media/video"
	"github.com/open-rails/contentkit/media/workqueue"
)

// Config configures the worker.
type Config struct {
	Pool  *pgxpool.Pool // the host database: workqueue.Schema, manifest locks, progress
	Store media.Store
	// Kinds, Specs and Hooks are the host's: build them with the code the
	// host's media setup uses. Hooks.Failed and Hooks.SlotEncoded run here.
	Kinds *media.Registry
	Specs image.SpecChooser
	Hooks media.Hooks
	// HostSchema and HostQueue are the host's River schema ("" is the
	// connection's search path) and media queue (default media.DefaultQueue):
	// video publishes and folder sweeps after edits run there. Grace is the
	// host's JobsConfig.Grace (default 24 h).
	HostSchema string
	HostQueue  string
	Grace      time.Duration

	TempDir string // scratch for video sources and outputs; default os.TempDir()
	Threads int    // ffmpeg threads; default GOMAXPROCS
	// Preset, TopPreset and VideoEncoder are video.Config's.
	Preset, TopPreset, VideoEncoder string
	// VideoWorkers and ImageWorkers are concurrent jobs per process
	// (defaults 1 and 2); ImageSources bounds sources decoded at once per
	// image job (default 2).
	VideoWorkers int
	ImageWorkers int
	ImageSources int
	// MaxPixels, MaxFrames and MaxAnimationSeconds bound decoded images
	// (image.Config's defaults when zero).
	MaxPixels           int
	MaxFrames           int
	MaxAnimationSeconds float64
	// VideoTimeout bounds one encode (default 48 h; a 2 h 4K ladder on 2 CPU
	// runs for many hours); ImageTimeout one image job (default 1 h).
	VideoTimeout time.Duration
	ImageTimeout time.Duration
	// ShutdownGrace is how long running jobs get to finish on shutdown
	// before they are cancelled and retried elsewhere; default 30 s.
	ShutdownGrace time.Duration
	Logger        *slog.Logger
	// RiverHooks are added to the worker's River client (observability).
	RiverHooks []rivertype.Hook
}

func (c *Config) defaults() error {
	if c.Pool == nil || c.Store == nil || c.Kinds == nil {
		return errors.New("media/worker: Config needs a Pool, a Store and the host's Kinds")
	}
	if c.VideoWorkers <= 0 {
		c.VideoWorkers = 1
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

// Worker is a configured worker; Run drains its jobs.
type Worker struct {
	c      Config
	client *river.Client[pgx.Tx]
}

// New migrates workqueue.Schema, clears scratch left by a killed worker and
// builds the River client with the image and video workers.
func New(ctx context.Context, c Config) (*Worker, error) {
	if err := c.defaults(); err != nil {
		return nil, err
	}
	if err := workqueue.Migrate(ctx, c.Pool); err != nil {
		return nil, err
	}
	if err := video.SweepTemp(c.TempDir); err != nil {
		return nil, fmt.Errorf("media/worker: sweep scratch: %w", err)
	}
	host, err := media.NewHostQueue(c.Pool, c.Kinds, c.HostSchema, c.HostQueue, c.Grace)
	if err != nil {
		return nil, err
	}
	manifests, err := media.NewManifests(c.Store, c.Kinds, media.ManifestOptions{Locker: media.PGLocker(c.Pool), Sweeps: host})
	if err != nil {
		return nil, err
	}
	queue, err := workqueue.New(c.Pool, c.Kinds)
	if err != nil {
		return nil, err
	}
	images, err := image.New(image.Config{Store: c.Store, Kinds: c.Kinds, Manifests: manifests, Specs: c.Specs, Hooks: c.Hooks,
		Workers: c.ImageSources, MaxPixels: c.MaxPixels, MaxFrames: c.MaxFrames, MaxAnimationSeconds: c.MaxAnimationSeconds})
	if err != nil {
		return nil, err
	}
	enc, err := video.New(video.Config{Store: c.Store, Locker: media.PGLocker(c.Pool), Sweeps: host, TempDir: c.TempDir,
		Threads: c.Threads, Preset: c.Preset, TopPreset: c.TopPreset, Encoder: c.VideoEncoder, Hooks: c.Hooks, Logger: c.Logger, Slots: queue})
	if err != nil {
		return nil, err
	}
	videos, err := video.Contribution(video.WorkerConfig{Encoder: enc, Pool: c.Pool, Kinds: c.Kinds, Timeout: c.VideoTimeout,
		MaxWorkers: c.VideoWorkers, Logger: c.Logger})
	if err != nil {
		return nil, err
	}
	imageJobs := riverhelpers.NewContribution("contentkit-media-image", func(_ context.Context, cfg *river.Config) error {
		if _, ok := cfg.Queues[workqueue.ImageQueue]; ok {
			return fmt.Errorf("media/worker: queue %q already registered", workqueue.ImageQueue)
		}
		cfg.Queues[workqueue.ImageQueue] = river.QueueConfig{MaxWorkers: c.ImageWorkers}
		return river.AddWorkerSafely(cfg.Workers, &imageWorker{c: c, images: images, host: host})
	}, nil, nil)
	client, err := riverhelpers.New(ctx, c.Pool, &river.Config{Schema: workqueue.Schema,
		JobTimeout: max(c.VideoTimeout, c.ImageTimeout), Logger: c.Logger, Hooks: c.RiverHooks}, videos, imageJobs)
	if err != nil {
		return nil, err
	}
	return &Worker{c: c, client: client}, nil
}

// Client is the worker's River client (tests subscribe to its events).
func (w *Worker) Client() *river.Client[pgx.Tx] { return w.client }

// Run works jobs until ctx is done, then lets running jobs finish within
// ShutdownGrace before cancelling them (ffmpeg is killed, scratch removed;
// the job retries from scratch elsewhere).
func (w *Worker) Run(ctx context.Context) error {
	if err := w.client.Start(ctx); err != nil {
		return err
	}
	w.c.Logger.Info("media-worker: started", "schema", workqueue.Schema, "video_workers", w.c.VideoWorkers, "image_workers", w.c.ImageWorkers)
	<-ctx.Done()
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

type imageWorker struct {
	river.WorkerDefaults[workqueue.ImageArgs]
	c      Config
	images *image.Processor
	host   *media.HostQueue
}

func (w *imageWorker) Timeout(*river.Job[workqueue.ImageArgs]) time.Duration { return w.c.ImageTimeout }

// Work runs one image job, after any equal job it follows. A video item's
// poster is then handed to the host's Publish, which copies
// what its Exposure allows to public/.
func (w *imageWorker) Work(ctx context.Context, job *river.Job[workqueue.ImageArgs]) error {
	pj := media.ProcessJob{Ref: job.Args.Ref, Slot: job.Args.Slot}
	item, err := w.c.Kinds.Item(pj.Ref)
	if err != nil {
		return river.JobCancel(err)
	}
	if err := media.WaitFor(ctx, river.ClientFromContext[pgx.Tx](ctx), job.Args.After); err != nil {
		return err
	}
	err = w.images.Process(ctx, pj)
	if item.Kind().Video != nil && pj.Slot == media.PosterSlot {
		err = errors.Join(err, w.host.Publish(ctx, pj.Ref))
	}
	return err
}
