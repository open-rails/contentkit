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
	riverhelpers "github.com/open-rails/helpers/river"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

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
	Queue            string        // default "contentkit_media"
	MaxWorkers       int           // default 2
	Logger           *slog.Logger
	Now              func() time.Time // clock for grace decisions; default time.Now
}

// Jobs is media's River contribution: sweep, folder deletion, and the workers
// other media packages register. Compose RiverJobs once into the host client.
type Jobs struct {
	cfg JobsConfig

	mu         sync.Mutex
	regs       []func(*river.Config) error
	processors []Processor
	composed   bool
	client     *river.Client[pgx.Tx]
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
		cfg.Queue = "contentkit_media"
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
			func() error { return river.AddWorkerSafely(cfg.Workers, &processWorker{j: j}) },
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

// pendingOnce dedupes a job per args while one is waiting or running.
var pendingOnce = river.UniqueOpts{ByArgs: true, ByState: []rivertype.JobState{rivertype.JobStateAvailable,
	rivertype.JobStatePending, rivertype.JobStateRunning, rivertype.JobStateRetryable, rivertype.JobStateScheduled}}

// ScheduleSweep sweeps the item's folder after the grace period; a sweep
// already waiting for the folder absorbs it. Manifests calls it on every edit.
func (j *Jobs) ScheduleSweep(ctx context.Context, ref contentref.ContentRef) error {
	item, err := j.cfg.Kinds.Item(ref.Content())
	if err != nil {
		return err
	}
	_, err = j.Insert(ctx, sweepArgs{Prefix: item.Prefix()}, &river.InsertOpts{
		ScheduledAt: j.cfg.Now().Add(j.cfg.Grace), UniqueOpts: pendingOnce})
	return err
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
		params = append(params, river.InsertManyParams{Args: args, InsertOpts: j.opts(&river.InsertOpts{UniqueOpts: pendingOnce})})
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

// Processor derives an item's files after a commit (image variants, video
// encodes). It must be idempotent. An Enqueue while its job runs is absorbed:
// the job reruns the processors when its input changed during the run.
type Processor func(ctx context.Context, job ProcessJob) error

// AddProcessor registers a processor for Enqueue'd jobs, before composition.
func (j *Jobs) AddProcessor(p Processor) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.composed {
		return errors.New("media: AddProcessor after RiverJobs was composed")
	}
	j.processors = append(j.processors, p)
	return nil
}

// Enqueue implements ProcessQueue: one pending job per ref and slot, run by
// every registered processor.
func (j *Jobs) Enqueue(ctx context.Context, job ProcessJob) error {
	if _, err := j.cfg.Kinds.Item(job.Ref); err != nil {
		return err
	}
	_, err := j.Insert(ctx, processArgs{Ref: job.Ref, Slot: job.Slot}, &river.InsertOpts{UniqueOpts: pendingOnce})
	return err
}

var _ ProcessQueue = (*Jobs)(nil)

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
}

func (sweepArgs) Kind() string { return "contentkit_media_sweep" }

type sweepWorker struct {
	river.WorkerDefaults[sweepArgs]
	j *Jobs
}

func (w *sweepWorker) Timeout(*river.Job[sweepArgs]) time.Duration { return 15 * time.Minute }

func (w *sweepWorker) Work(ctx context.Context, job *river.Job[sweepArgs]) error {
	if _, _, _, err := parseFolder(job.Args.Prefix); err != nil {
		return river.JobCancel(err)
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
		ScheduledAt: w.j.cfg.Now().Add(w.j.cfg.LateUploadWindow), UniqueOpts: pendingOnce}); err != nil {
		return err
	}
	if w.j.cfg.Limiter != nil && job.Args.Owner != "" && job.Args.Release > 0 {
		tenant, _, _, _ := parseFolder(job.Args.Prefix)
		return w.j.cfg.Limiter.Settle(ctx, Settlement{Tenant: tenant, Owner: job.Args.Owner, Delta: -job.Args.Release})
	}
	return nil
}

type processArgs struct {
	Ref  contentref.ContentRef `json:"ref"`
	Slot string                `json:"slot,omitempty"`
}

func (processArgs) Kind() string { return "contentkit_media_process" }

type processWorker struct {
	river.WorkerDefaults[processArgs]
	j *Jobs
}

func (w *processWorker) Timeout(*river.Job[processArgs]) time.Duration { return time.Hour }

func (w *processWorker) Work(ctx context.Context, job *river.Job[processArgs]) error {
	pj := ProcessJob{Ref: job.Args.Ref, Slot: job.Args.Slot}
	item, err := w.j.cfg.Kinds.Item(pj.Ref)
	if err != nil {
		return river.JobCancel(err)
	}
	// The input is the manifest, or the slot original; a commit that lands
	// after the processors read it changes its version (a slot's original and record).
	key, err := item.ManifestKey()
	keys := []string{key}
	if pj.Slot != "" {
		key, err = item.SlotOriginal(pj.Slot)
		keys = []string{key}
		if rec, rerr := item.SlotRecord(pj.Slot); rerr == nil {
			keys = append(keys, rec)
		}
	}
	if err != nil {
		return river.JobCancel(err)
	}
	before, err := w.j.etag(ctx, keys...)
	if err != nil {
		return err
	}
	var errs []error
	for _, p := range w.j.processors {
		errs = append(errs, p(ctx, pj))
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}
	// A processor's own write also changes it; the rerun then finds no work
	// and leaves the input unchanged.
	after, err := w.j.etag(ctx, keys...)
	if err != nil {
		return err
	}
	if after != before {
		return river.JobSnooze(0)
	}
	return nil
}

// etag is the inputs' version: their ETags ("" when absent).
func (j *Jobs) etag(ctx context.Context, keys ...string) (string, error) {
	var v string
	for _, key := range keys {
		obj, err := j.cfg.Store.Head(ctx, key)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return "", err
		}
		v += obj.ETag + "|"
	}
	return v, nil
}
