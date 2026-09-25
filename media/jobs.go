package media

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	riverhelpers "github.com/open-rails/helpers/river"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertype"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media/layout"
)

// UserKind is the kind of per-user folders ({tenant}/user/{id}/), erased by EraseUserTx.
const UserKind = "user"

// JobsConfig configures media's River jobs.
type JobsConfig struct {
	Store Store
	Kinds *Registry
	// Tenants are the folders the periodic sweep pass covers; folders of
	// kinds missing from Kinds are skipped.
	Tenants []string
	// Grace protects in-flight uploads, jobs and mid-stream viewers: a folder
	// is swept only when its manifests are this old, and only objects this old
	// are deleted. Default 24 h.
	Grace time.Duration
	// SweepInterval is the periodic pass interval. Default 24 h.
	SweepInterval time.Duration
	// LateUploadWindow delays the second pass of a folder deletion, which
	// removes PUTs and multipart completions that land after the first. It
	// must exceed the longest upload presign TTL and the 1-day multipart
	// abort rule. Default 25 h.
	LateUploadWindow time.Duration
	Limiter          UploadLimiter // releases a deleted item's quota; optional
	// Resolver decides, with an anonymous actor, what of a video item is
	// public (Publish); without it no poster or hover preview is published.
	Resolver access.ContentResolver
	// Exposure maps that anonymous resolution to what is published; default
	// DefaultExposure (drafts nothing, free items all, others the poster).
	Exposure   ExposurePolicy
	Queue      string // default "contentkit_media"
	MaxWorkers int    // default 2
	Logger     *slog.Logger
	Now        func() time.Time // clock for grace decisions; default time.Now
}

// Jobs is media's River contribution to the host: sweep, folder deletion and
// publishing. Processing runs in the media worker (media/worker). Compose
// RiverJobs once into the host client.
type Jobs struct {
	cfg JobsConfig

	mu       sync.Mutex
	regs     []func(*river.Config) error
	composed bool
	client   *river.Client[pgx.Tx]
}

var ErrJobsNotBound = errors.New("media: River jobs are not composed into a client")

func NewJobs(cfg JobsConfig) (*Jobs, error) {
	if cfg.Store == nil || cfg.Kinds == nil {
		return nil, errors.New("media: Jobs needs a Store and a Registry")
	}
	for _, t := range cfg.Tenants {
		if !layout.ValidSegment(t) {
			return nil, fmt.Errorf("media: invalid tenant %q", t)
		}
	}
	if cfg.Grace <= 0 {
		cfg.Grace = 24 * time.Hour
	}
	if cfg.SweepInterval <= 0 {
		cfg.SweepInterval = 24 * time.Hour
	}
	if cfg.LateUploadWindow <= 0 {
		cfg.LateUploadWindow = 25 * time.Hour
	}
	if cfg.Queue == "" {
		cfg.Queue = DefaultQueue
	}
	if cfg.MaxWorkers <= 0 {
		cfg.MaxWorkers = 2
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Exposure == nil {
		cfg.Exposure = DefaultExposure
	}
	if cfg.Resolver == nil && cfg.Kinds.hasVideo() {
		cfg.Logger.Warn("media: JobsConfig.Resolver is nil; video posters and hover previews are never published")
	}
	return &Jobs{cfg: cfg}, nil
}

// Queue is the shared media queue; registered workers may use it or add their own.
func (j *Jobs) Queue() string { return j.cfg.Queue }

// Register adds workers, queues or periodic jobs to the contribution. Media
// packages (image variants, video) call it before RiverJobs is composed.
func (j *Jobs) Register(fn func(*river.Config) error) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.composed {
		return errors.New("media: Register after RiverJobs was composed")
	}
	j.regs = append(j.regs, fn)
	return nil
}

// RiverJobs contributes media's workers, queue and periodic sweep to the
// host's helpers/river composition. It composes once.
func (j *Jobs) RiverJobs() riverhelpers.Contribution {
	claimed := false
	return riverhelpers.NewContribution("contentkit-media", func(_ context.Context, cfg *river.Config) error {
		j.mu.Lock()
		defer j.mu.Unlock()
		if j.composed {
			return errors.New("media: River jobs already composed")
		}
		j.composed, claimed = true, true
		if q, ok := cfg.Queues[j.cfg.Queue]; ok && q.MaxWorkers < 1 {
			return fmt.Errorf("media: queue %q needs workers", j.cfg.Queue)
		} else if !ok {
			cfg.Queues[j.cfg.Queue] = river.QueueConfig{MaxWorkers: j.cfg.MaxWorkers}
		}
		for _, w := range []func() error{
			func() error { return river.AddWorkerSafely(cfg.Workers, &sweepWorker{j: j}) },
			func() error { return river.AddWorkerSafely(cfg.Workers, &sweepPassWorker{j: j}) },
			func() error { return river.AddWorkerSafely(cfg.Workers, &deleteFolderWorker{j: j}) },
			func() error { return river.AddWorkerSafely(cfg.Workers, &publishWorker{j: j}) },
		} {
			if err := w(); err != nil {
				return err
			}
		}
		interval := j.cfg.SweepInterval
		cfg.PeriodicJobs = append(cfg.PeriodicJobs, river.NewPeriodicJob(river.PeriodicInterval(interval),
			func() (river.JobArgs, *river.InsertOpts) {
				return sweepPassArgs{}, &river.InsertOpts{Queue: j.cfg.Queue, MaxAttempts: 3,
					UniqueOpts: river.UniqueOpts{ByPeriod: interval}}
			}, &river.PeriodicJobOpts{ID: "contentkit_media_sweep_pass"}))
		for _, reg := range j.regs {
			if err := reg(cfg); err != nil {
				return err
			}
		}
		return nil
	}, func(_ context.Context, b riverhelpers.Binding) error {
		j.mu.Lock()
		j.client = b.Client
		j.mu.Unlock()
		return nil
	}, func() error {
		if claimed {
			j.mu.Lock()
			j.client = nil
			j.mu.Unlock()
		}
		return nil
	})
}

func (j *Jobs) bound() (*river.Client[pgx.Tx], error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.client == nil {
		return nil, ErrJobsNotBound
	}
	return j.client, nil
}

func (j *Jobs) opts(o *river.InsertOpts) *river.InsertOpts {
	if o == nil {
		o = &river.InsertOpts{}
	}
	if o.Queue == "" {
		o.Queue = j.cfg.Queue
	}
	return o
}

// Insert enqueues a job on the bound client; an empty queue means Queue().
func (j *Jobs) Insert(ctx context.Context, args river.JobArgs, o *river.InsertOpts) (*rivertype.JobInsertResult, error) {
	c, err := j.bound()
	if err != nil {
		return nil, err
	}
	return c.Insert(ctx, args, j.opts(o))
}

// InsertTx enqueues a job in the host's transaction.
func (j *Jobs) InsertTx(ctx context.Context, tx pgx.Tx, args river.JobArgs, o *river.InsertOpts) (*rivertype.JobInsertResult, error) {
	c, err := j.bound()
	if err != nil {
		return nil, err
	}
	return c.InsertTx(ctx, tx, args, j.opts(o))
}

// PendingOnce dedupes a job per args while one is waiting or running.
var PendingOnce = river.UniqueOpts{ByArgs: true, ByState: []rivertype.JobState{rivertype.JobStateAvailable,
	rivertype.JobStatePending, rivertype.JobStateRunning, rivertype.JobStateRetryable, rivertype.JobStateScheduled}}

// RerunArgs are job args that can name the running job they follow.
type RerunArgs interface {
	river.JobArgs
	FollowUp(id int64) river.JobArgs
}

// InsertFunc inserts one job (a River client's Insert, or InsertTx bound to a transaction).
type InsertFunc func(ctx context.Context, args river.JobArgs, o *river.InsertOpts) (*rivertype.JobInsertResult, error)

// InsertOnce enqueues args after the caller's change to their inputs. An
// equal job still waiting to run absorbs it. River's uniqueness also covers
// running jobs, which may have read the inputs before the change, so one
// follow-up is queued behind a running equal job; a burst shares it. The
// follow-up's worker calls WaitFor first.
func InsertOnce(ctx context.Context, insert InsertFunc, args RerunArgs, o river.InsertOpts) error {
	o.UniqueOpts = PendingOnce
	var next river.JobArgs = args
	for {
		res, err := insert(ctx, next, &o)
		if err != nil || !res.UniqueSkippedAsDuplicate || res.Job.State != rivertype.JobStateRunning {
			return err
		}
		next = args.FollowUp(res.Job.ID)
	}
}

// WaitFor snoozes a follow-up (InsertOnce) while the job it follows still
// runs, so jobs for the same inputs do not overlap. The client is the one
// running the job (river.ClientFromContext).
func WaitFor(ctx context.Context, c *river.Client[pgx.Tx], id int64) error {
	if id == 0 {
		return nil
	}
	prev, err := c.JobGet(ctx, id)
	if errors.Is(err, river.ErrNotFound) {
		return nil
	} else if err != nil {
		return err
	}
	if prev.State == rivertype.JobStateRunning {
		return river.JobSnooze(time.Second)
	}
	return nil
}

func (j *Jobs) insertOnce(ctx context.Context, args RerunArgs, o river.InsertOpts) error {
	return InsertOnce(ctx, j.Insert, args, o)
}

func (j *Jobs) waitFor(ctx context.Context, id int64) error {
	if id == 0 {
		return nil
	}
	c, err := j.bound()
	if err != nil {
		return err
	}
	return WaitFor(ctx, c, id)
}

// ScheduleSweep sweeps the item's folder after the grace period; a sweep
// already waiting for the folder absorbs it. Manifests calls it on every edit.
func (j *Jobs) ScheduleSweep(ctx context.Context, ref contentref.ContentRef) error {
	item, err := j.cfg.Kinds.Item(ref.Content())
	if err != nil {
		return err
	}
	return j.insertOnce(ctx, sweepArgs{Prefix: item.Prefix()}, river.InsertOpts{ScheduledAt: j.cfg.Now().Add(j.cfg.Grace)})
}

// Deletion is one item to delete. Owner is its quota owner (UploadGrant.Owner),
// "" for none; with a Limiter configured the owner's usage is released.
type Deletion struct {
	Ref   contentref.ContentRef
	Owner string
}

// DeleteItemsTx deletes each item's whole folder (every version) through a
// job enqueued in the host's delete transaction. A second pass after
// LateUploadWindow removes uploads that land after the first. The quota to
// release is measured here from the item's manifests, so a retried job
// settles it once.
func (j *Jobs) DeleteItemsTx(ctx context.Context, tx pgx.Tx, items ...Deletion) error {
	c, err := j.bound()
	if err != nil {
		return err
	}
	params := make([]river.InsertManyParams, 0, len(items))
	for _, d := range items {
		ref := d.Ref
		if ref.Version() != "" {
			return fmt.Errorf("media: delete %s: folders hold every version; pass the work ref", ref)
		}
		prefix, err := folderPrefix(ref.TenantID, ref.ContentKind, ref.ContentID)
		if err != nil {
			return err
		}
		args := deleteFolderArgs{Prefix: prefix}
		if j.cfg.Limiter != nil && d.Owner != "" {
			if args.Release, err = j.originalBytes(ctx, prefix); err != nil {
				return err
			}
			args.Owner = d.Owner
		}
		params = append(params, river.InsertManyParams{Args: args, InsertOpts: j.opts(&river.InsertOpts{UniqueOpts: PendingOnce})})
	}
	if len(params) == 0 {
		return nil
	}
	_, err = c.InsertManyTx(ctx, tx, params)
	return err
}

// EraseUserTx erases a user's media: the items the host maps to them plus
// their user folder, {tenant}/user/{id}/.
func (j *Jobs) EraseUserTx(ctx context.Context, tx pgx.Tx, tenant, userID string, items ...Deletion) error {
	return j.DeleteItemsTx(ctx, tx, append(items, Deletion{Ref: contentref.New(tenant, UserKind, userID)})...)
}

// originalBytes is the quota the folder's manifests were charged at commit.
func (j *Jobs) originalBytes(ctx context.Context, prefix string) (int64, error) {
	objs, err := j.list(ctx, prefix)
	if err != nil {
		return 0, err
	}
	var n int64
	for key := range manifestKeys(objs) {
		man, err := j.readManifest(ctx, key)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return 0, err
		}
		n += man.OriginalBytes()
	}
	return n, nil
}

func folderPrefix(tenant, kind, id string) (string, error) {
	if !layout.ValidSegment(tenant) || !layout.ValidSegment(kind) || !layout.ValidSegment(id) {
		return "", fmt.Errorf("media: invalid folder %q/%q/%q", tenant, kind, id)
	}
	return tenant + "/" + kind + "/" + id + "/", nil
}

// parseFolder guards every job's prefix: exactly "{tenant}/{kind}/{id}/".
func parseFolder(prefix string) (tenant, kind, id string, err error) {
	parts := strings.Split(prefix, "/")
	if len(parts) != 4 || parts[3] != "" {
		return "", "", "", fmt.Errorf("media: invalid folder prefix %q", prefix)
	}
	if _, err := folderPrefix(parts[0], parts[1], parts[2]); err != nil {
		return "", "", "", err
	}
	return parts[0], parts[1], parts[2], nil
}

type sweepArgs struct {
	Prefix string `json:"prefix"`
	After  int64  `json:"after,omitempty"` // the running sweep this one follows
}

func (sweepArgs) Kind() string { return "contentkit_media_sweep" }

func (a sweepArgs) FollowUp(id int64) river.JobArgs { a.After = id; return a }

type sweepWorker struct {
	river.WorkerDefaults[sweepArgs]
	j *Jobs
}

func (w *sweepWorker) Timeout(*river.Job[sweepArgs]) time.Duration { return 15 * time.Minute }

func (w *sweepWorker) Work(ctx context.Context, job *river.Job[sweepArgs]) error {
	if _, _, _, err := parseFolder(job.Args.Prefix); err != nil {
		return river.JobCancel(err)
	}
	if err := w.j.waitFor(ctx, job.Args.After); err != nil {
		return err
	}
	res, err := w.j.sweep(ctx, job.Args.Prefix, nil)
	if err != nil {
		return err
	}
	if res.Wait > 0 {
		return river.JobSnooze(res.Wait)
	}
	return nil
}

type sweepPassArgs struct{}

func (sweepPassArgs) Kind() string { return "contentkit_media_sweep_pass" }

type sweepPassWorker struct {
	river.WorkerDefaults[sweepPassArgs]
	j *Jobs
}

func (w *sweepPassWorker) Timeout(*river.Job[sweepPassArgs]) time.Duration { return 6 * time.Hour }

func (w *sweepPassWorker) Work(ctx context.Context, _ *river.Job[sweepPassArgs]) error {
	return w.j.SweepAll(ctx)
}

type deleteFolderArgs struct {
	Prefix  string `json:"prefix"`
	Final   bool   `json:"final,omitempty"`
	Owner   string `json:"owner,omitempty"`
	Release int64  `json:"release,omitempty"`
}

func (deleteFolderArgs) Kind() string { return "contentkit_media_delete_folder" }

type deleteFolderWorker struct {
	river.WorkerDefaults[deleteFolderArgs]
	j *Jobs
}

func (w *deleteFolderWorker) Timeout(*river.Job[deleteFolderArgs]) time.Duration {
	return 15 * time.Minute
}

func (w *deleteFolderWorker) Work(ctx context.Context, job *river.Job[deleteFolderArgs]) error {
	if _, _, _, err := parseFolder(job.Args.Prefix); err != nil {
		return river.JobCancel(err)
	}
	if err := w.j.deleteFolder(ctx, job.Args.Prefix); err != nil {
		return err
	}
	if job.Args.Final {
		return nil
	}
	if _, err := w.j.Insert(ctx, deleteFolderArgs{Prefix: job.Args.Prefix, Final: true}, &river.InsertOpts{
		ScheduledAt: w.j.cfg.Now().Add(w.j.cfg.LateUploadWindow), UniqueOpts: PendingOnce}); err != nil {
		return err
	}
	if w.j.cfg.Limiter != nil && job.Args.Owner != "" && job.Args.Release > 0 {
		tenant, _, _, _ := parseFolder(job.Args.Prefix)
		return w.j.cfg.Limiter.Settle(ctx, Settlement{Tenant: tenant, Owner: job.Args.Owner, Delta: -job.Args.Release})
	}
	return nil
}

// DefaultQueue is JobsConfig.Queue's default.
const DefaultQueue = "contentkit_media"

// HostQueue is the media worker's handle on the host's River schema: it
// inserts the jobs the host runs on the worker's behalf, a video item's
// Publish after its poster or hover preview changes and a folder's sweep
// after the worker edits a manifest. It inserts only.
type HostQueue struct {
	client *river.Client[pgx.Tx]
	kinds  *Registry
	queue  string
	grace  time.Duration
}

// NewHostQueue targets the host's River schema ("" is the connection's
// search path) and media queue ("" is DefaultQueue); grace is the host's
// JobsConfig.Grace (default 24 h).
func NewHostQueue(pool *pgxpool.Pool, kinds *Registry, schema, queue string, grace time.Duration) (*HostQueue, error) {
	if pool == nil || kinds == nil {
		return nil, errors.New("media: HostQueue needs a pool and a Registry")
	}
	if queue == "" {
		queue = DefaultQueue
	}
	if grace <= 0 {
		grace = 24 * time.Hour
	}
	c, err := river.NewClient(riverpgxv5.New(pool), &river.Config{Schema: schema})
	if err != nil {
		return nil, err
	}
	return &HostQueue{client: c, kinds: kinds, queue: queue, grace: grace}, nil
}

func (h *HostQueue) insert(ctx context.Context, args river.JobArgs, o *river.InsertOpts) (*rivertype.JobInsertResult, error) {
	o.Queue = h.queue
	return h.client.Insert(ctx, args, o)
}

// Publish enqueues the host's Publish of a video item.
func (h *HostQueue) Publish(ctx context.Context, ref contentref.ContentRef) error {
	if _, err := h.kinds.Item(ref.Content()); err != nil {
		return err
	}
	_, err := h.insert(ctx, publishArgs{Ref: ref.Content()}, &river.InsertOpts{})
	return err
}

// ScheduleSweep implements SweepScheduler like Jobs.ScheduleSweep.
func (h *HostQueue) ScheduleSweep(ctx context.Context, ref contentref.ContentRef) error {
	item, err := h.kinds.Item(ref.Content())
	if err != nil {
		return err
	}
	return InsertOnce(ctx, h.insert, sweepArgs{Prefix: item.Prefix()}, river.InsertOpts{ScheduledAt: time.Now().Add(h.grace)})
}
