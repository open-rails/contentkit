package media

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	riverhelpers "github.com/open-rails/helpers/river"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertype"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media/layout"
)

// JobsConfig configures media's River jobs in the host.
type JobsConfig struct {
	Store     Store
	Registry  *Registry
	Locker    Locker       // serializes manifest edits and sweep deletion; required
	Journal   *PGJournal   // same ContentKit journal used by the uploader and media worker; required
	Processes ProcessQueue // the worker's queue (workqueue.Queue): Expose and Regenerate render through it
	// Pool is the host database Hooks.ItemReady's transaction runs on;
	// required with ItemReady.
	Pool *pgxpool.Pool
	// Grace protects job outputs not recorded yet: the sweep deletes an
	// unreferenced blob once it is this old, whatever edits follow. Default
	// 24 h.
	Grace time.Duration
	// TempTTL is how long temp/ objects (in-flight server-side writes) are
	// kept; above the bucket's 1-day abort-incomplete rule. Default 48 h.
	TempTTL time.Duration
	// SweepInterval is the periodic pass over every folder. Default 24 h.
	SweepInterval time.Duration
	// LateUploadWindow delays a folder deletion's second pass, which removes
	// uploads that land after the first; above the presign TTL and the
	// 1-day multipart rule. Default 25 h.
	LateUploadWindow time.Duration
	Limiter          QuotaReleaser // releases a deleted item's quota; optional
	Queue            string        // default DefaultQueue
	MaxWorkers       int           // default 2
	Logger           *slog.Logger
	Now              func() time.Time
}

// DefaultQueue is JobsConfig.Queue's default.
const DefaultQueue = "contentkit_media"

// Jobs is media's River contribution to the host: sweep, folder deletion,
// Expose, and the worker's relays (ItemReady, PurgePublic). Processing runs
// in the media worker (media/worker).
type Jobs struct {
	cfg       JobsConfig
	manifests *Manifests

	mu       sync.Mutex
	composed bool
	client   *river.Client[pgx.Tx]
}

var ErrJobsNotBound = errors.New("media: River jobs are not composed into a client")

func NewJobs(cfg JobsConfig) (*Jobs, error) {
	if cfg.Store == nil || cfg.Registry == nil || cfg.Locker == nil || cfg.Journal == nil {
		return nil, errors.New("media: Jobs needs a Store, a Registry, a Locker and a Journal")
	}
	if cfg.Registry.cfg.Hooks.ItemReady != nil && cfg.Pool == nil {
		return nil, errors.New("media: Hooks.ItemReady needs JobsConfig.Pool")
	}
	if cfg.Grace <= 0 {
		cfg.Grace = 24 * time.Hour
	}
	if cfg.TempTTL <= 0 {
		cfg.TempTTL = 48 * time.Hour
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
	j := &Jobs{cfg: cfg}
	var err error
	if j.manifests, err = NewManifests(cfg.Store, cfg.Registry, ManifestOptions{Locker: cfg.Locker, Journal: cfg.Journal, Sweeps: j}); err != nil {
		return nil, err
	}
	return j, nil
}

// Manifests is the jobs' manifest store; hosts may share it.
func (j *Jobs) Manifests() *Manifests { return j.manifests }

// RiverJobs contributes media's workers, queue and periodic sweep pass to
// the host's helpers/river composition. It composes once.
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
		for _, add := range []func() error{
			func() error { return river.AddWorkerSafely(cfg.Workers, &sweepWorker{j: j}) },
			func() error { return river.AddWorkerSafely(cfg.Workers, &sweepPassWorker{j: j}) },
			func() error { return river.AddWorkerSafely(cfg.Workers, &deleteFolderWorker{j: j}) },
			func() error { return river.AddWorkerSafely(cfg.Workers, &exposeWorker{j: j}) },
			func() error { return river.AddWorkerSafely(cfg.Workers, &readyWorker{j: j}) },
			func() error { return river.AddWorkerSafely(cfg.Workers, &purgeWorker{j: j}) },
			func() error { return river.AddWorkerSafely(cfg.Workers, &recoverWorker{j: j}) },
		} {
			if err := add(); err != nil {
				return err
			}
		}
		interval := j.cfg.SweepInterval
		cfg.PeriodicJobs = append(cfg.PeriodicJobs, river.NewPeriodicJob(river.PeriodicInterval(interval),
			func() (river.JobArgs, *river.InsertOpts) {
				return sweepPassArgs{}, &river.InsertOpts{Queue: j.cfg.Queue, MaxAttempts: 3,
					UniqueOpts: river.UniqueOpts{ByPeriod: interval}}
			}, &river.PeriodicJobOpts{ID: "contentkit_media_sweep_pass"}))
		cfg.PeriodicJobs = append(cfg.PeriodicJobs, river.NewPeriodicJob(river.PeriodicInterval(time.Minute),
			func() (river.JobArgs, *river.InsertOpts) {
				return recoverArgs{}, &river.InsertOpts{Queue: j.cfg.Queue, MaxAttempts: 3,
					UniqueOpts: river.UniqueOpts{ByPeriod: time.Minute}}
			}, &river.PeriodicJobOpts{ID: "contentkit_media_recover", RunOnStart: true}))
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

// Insert enqueues a job on the bound client; an empty queue means the media queue.
func (j *Jobs) Insert(ctx context.Context, args river.JobArgs, o *river.InsertOpts) (*rivertype.JobInsertResult, error) {
	c, err := j.bound()
	if err != nil {
		return nil, err
	}
	return c.Insert(ctx, args, j.opts(o))
}

func (j *Jobs) waitFor(ctx context.Context, id int64) error {
	c, err := j.bound()
	if err != nil {
		return err
	}
	return WaitFor(ctx, c, id)
}

// ScheduleSweep sweeps the item's folder after the grace period; a sweep
// already waiting for the folder absorbs it. Manifests calls it on every edit.
func (j *Jobs) ScheduleSweep(ctx context.Context, ref contentref.ContentRef) error {
	item, err := j.cfg.Registry.Item(ref)
	if err != nil {
		return err
	}
	return InsertOnce(ctx, j.Insert, sweepArgs{Prefix: item.Prefix()}, river.InsertOpts{ScheduledAt: j.cfg.Now().Add(j.cfg.Grace)})
}

// Deletion is one item to delete. Owner is its quota owner (UploadGrant.Owner),
// "" for none; with a Limiter configured the owner's usage is released.
// OperationID is only for Purge; queued deletion uses its River job id.
type Deletion struct {
	Ref         contentref.ContentRef
	Owner       string
	OperationID string // required by Purge with a quota owner; stable across retries
}

// DeleteItemsTx deletes each item's folder through a job enqueued in the
// host's delete transaction: a host deletes every version item of a work,
// and erasing an account deletes its accounts/user/{id}/ item. A second pass
// after LateUploadWindow removes uploads that land after the first. The
// worker records the quota refund from the manifest before deleting.
func (j *Jobs) DeleteItemsTx(ctx context.Context, tx pgx.Tx, items ...Deletion) error {
	c, err := j.bound()
	if err != nil {
		return err
	}
	params := make([]river.InsertManyParams, 0, len(items))
	for _, d := range items {
		item, err := j.cfg.Registry.Item(d.Ref)
		if err != nil {
			return err
		}
		args := deleteFolderArgs{Prefix: item.Prefix()}
		if j.cfg.Limiter != nil {
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

// Purge deletes an item's folder now: the explicit reset before
// deliberately recreating an item, or an operator cleanup. A quota-owned
// purge needs a caller-stable OperationID; retry a failed purge with it.
// Hosts deleting content use DeleteItemsTx.
func (j *Jobs) Purge(ctx context.Context, d Deletion) error {
	item, err := j.cfg.Registry.Item(d.Ref)
	if err != nil {
		return err
	}
	if j.cfg.Limiter != nil && d.Owner != "" && d.OperationID == "" {
		return errors.New("media: purge with a quota owner needs an operation id")
	}
	return j.deleteFolderLocked(ctx, item.Prefix(), d.Owner, "purge:"+item.Prefix()+d.OperationID)
}

// deleteFolderLocked deletes prefix under its manifest lock, releasing the
// owner's quota first (once per operation).
func (j *Jobs) deleteFolderLocked(ctx context.Context, prefix, owner, operation string) error {
	unlock, err := j.cfg.Locker.Lock(ctx, prefix+layout.ManifestName)
	if err != nil {
		return err
	}
	defer unlock()
	ns, _, _, _ := parseFolder(prefix)
	if j.cfg.Limiter != nil && owner != "" {
		release, err := j.uploadBytes(ctx, prefix)
		if err != nil {
			return err
		}
		applied, err := j.cfg.Limiter.PrepareRelease(ctx, QuotaRelease{Tenant: ns, Folder: prefix, Owner: owner, Operation: operation, Bytes: release})
		if err != nil || applied {
			return err
		}
	}
	if err := j.deleteFolder(ctx, prefix); err != nil {
		return err
	}
	if j.cfg.Limiter != nil && owner != "" {
		return j.cfg.Limiter.Release(ctx, ns, operation)
	}
	return nil
}

// uploadBytesMeta is the manifest object's metadata key for the quota its
// uploads were charged.
const uploadBytesMeta = "upload-bytes"

// uploadBytes is the quota the folder's manifest was charged, read from the
// manifest object's metadata so a manifest that does not decode (over its
// bound) is still deleted. One without the record is decoded; one that does
// not decode releases nothing, and any other error is returned for a retry.
func (j *Jobs) uploadBytes(ctx context.Context, prefix string) (int64, error) {
	key := prefix + layout.ManifestName
	obj, err := j.cfg.Store.Head(ctx, key)
	if errors.Is(err, ErrNotFound) {
		return 0, nil
	} else if err != nil {
		return 0, err
	}
	if n, err := strconv.ParseInt(obj.Metadata[uploadBytesMeta], 10, 64); err == nil && n >= 0 {
		return n, nil
	}
	m, err := j.readManifest(ctx, key)
	switch {
	case errors.Is(err, ErrNotFound):
		return 0, nil
	case errors.Is(err, errBadManifest):
		j.cfg.Logger.WarnContext(ctx, "media: deleting a folder whose manifest does not decode releases no quota", "key", key, "error", err)
		return 0, nil
	case err != nil:
		return 0, err
	}
	return m.uploadBytes(), nil
}

// parseFolder guards every job's prefix: exactly "{ns}/{kind}/{id}/".
func parseFolder(prefix string) (ns, kind, id string, err error) {
	parts := strings.Split(prefix, "/")
	if len(parts) != 4 || parts[3] != "" || !layout.ValidSegment(parts[0]) || !layout.ValidSegment(parts[1]) ||
		contentref.ValidateID(parts[2]) != nil {
		return "", "", "", fmt.Errorf("media: invalid folder prefix %q", prefix)
	}
	return parts[0], parts[1], parts[2], nil
}

// Regenerate visits every item of kind (in its namespace) and asks the
// worker to redo its stale outputs of preset ("" for all): after a deploy
// changed a preset's spec or a producer's recipe. It returns the items
// visited.
func (j *Jobs) Regenerate(ctx context.Context, kind, preset string) (int, error) {
	k, err := j.cfg.Registry.Kind(kind)
	if err != nil {
		return 0, err
	}
	if j.cfg.Processes == nil {
		return 0, errors.New("media: Regenerate needs JobsConfig.Processes")
	}
	n := 0
	root := k.ns + "/" + k.Name + "/"
	last := ""
	for o, err := range j.cfg.Store.List(ctx, root) {
		if err != nil {
			return n, err
		}
		id, rest, _ := strings.Cut(strings.TrimPrefix(o.Key, root), "/")
		if id == last || rest != layout.ManifestName || contentref.ValidateID(id) != nil {
			continue
		}
		last = id
		if err := j.cfg.Processes.Enqueue(ctx, ProcessJob{Ref: contentref.New(k.ns, k.Name, id), Preset: preset, Class: VideoBackfill}); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

type sweepArgs struct {
	Prefix string `json:"prefix"`
	After  int64  `json:"after,omitempty"` // the running sweep this one follows
}

func (sweepArgs) Kind() string                      { return "contentkit_media_sweep" }
func (a sweepArgs) FollowUp(id int64) river.JobArgs { a.After = id; return a }

type sweepWorker struct {
	river.WorkerDefaults[sweepArgs]
	j *Jobs
}

func (w *sweepWorker) Timeout(*river.Job[sweepArgs]) time.Duration { return 15 * time.Minute }

func (w *sweepWorker) Work(ctx context.Context, job *river.Job[sweepArgs]) (err error) {
	defer func() { err = SnoozeUnavailable(ctx, w.j.cfg.Store, job.JobRow, err) }()
	if _, _, _, err := parseFolder(job.Args.Prefix); err != nil {
		return river.JobCancel(err)
	}
	if err := w.j.waitFor(ctx, job.Args.After); err != nil {
		return err
	}
	res, err := w.j.sweep(ctx, job.Args.Prefix)
	if err != nil {
		return err
	}
	if res.Wait > 0 {
		return river.JobSnooze(res.Wait)
	}
	return nil
}

type recoverArgs struct{}

func (recoverArgs) Kind() string { return "contentkit_media_recover" }

type recoverWorker struct {
	river.WorkerDefaults[recoverArgs]
	j *Jobs
}

func (w *recoverWorker) Timeout(*river.Job[recoverArgs]) time.Duration { return time.Minute }

func (w *recoverWorker) Work(ctx context.Context, job *river.Job[recoverArgs]) (err error) {
	defer func() { err = SnoozeUnavailable(ctx, w.j.cfg.Store, job.JobRow, err) }()
	err = w.j.manifests.RecoverPending(ctx, 100)
	return err
}

type sweepPassArgs struct{}

func (sweepPassArgs) Kind() string { return "contentkit_media_sweep_pass" }

type sweepPassWorker struct {
	river.WorkerDefaults[sweepPassArgs]
	j *Jobs
}

func (w *sweepPassWorker) Timeout(*river.Job[sweepPassArgs]) time.Duration { return 6 * time.Hour }

func (w *sweepPassWorker) Work(ctx context.Context, job *river.Job[sweepPassArgs]) (err error) {
	defer func() { err = SnoozeUnavailable(ctx, w.j.cfg.Store, job.JobRow, err) }()
	return w.j.SweepAll(ctx)
}

type deleteFolderArgs struct {
	Prefix string `json:"prefix"`
	Final  bool   `json:"final,omitempty"`
	Owner  string `json:"owner,omitempty"`
}

func (deleteFolderArgs) Kind() string { return "contentkit_media_delete_folder" }

type deleteFolderWorker struct {
	river.WorkerDefaults[deleteFolderArgs]
	j *Jobs
}

func (w *deleteFolderWorker) Timeout(*river.Job[deleteFolderArgs]) time.Duration {
	return 15 * time.Minute
}

func (w *deleteFolderWorker) Work(ctx context.Context, job *river.Job[deleteFolderArgs]) (err error) {
	defer func() { err = SnoozeUnavailable(ctx, w.j.cfg.Store, job.JobRow, err) }()
	a := job.Args
	if _, _, _, err := parseFolder(a.Prefix); err != nil {
		return river.JobCancel(err)
	}
	owner := a.Owner
	if a.Final {
		owner = ""
	}
	if err := w.j.deleteFolderLocked(ctx, a.Prefix, owner, fmt.Sprintf("folder-delete:%s:%d", a.Prefix, job.JobRow.ID)); err != nil {
		return err
	}
	if a.Final {
		return nil
	}
	_, err = w.j.Insert(ctx, deleteFolderArgs{Prefix: a.Prefix, Final: true}, &river.InsertOpts{
		ScheduledAt: w.j.cfg.Now().Add(w.j.cfg.LateUploadWindow), UniqueOpts: PendingOnce})
	return err
}

// readyArgs asks the host to report an item's settled readiness
// (Hooks.ItemReady); the media worker enqueues it after a job.
type readyArgs struct {
	Ref   contentref.ContentRef `json:"ref"`
	After int64                 `json:"after,omitempty"`
}

func (readyArgs) Kind() string                      { return "contentkit_media_ready" }
func (a readyArgs) FollowUp(id int64) river.JobArgs { a.After = id; return a }

type readyWorker struct {
	river.WorkerDefaults[readyArgs]
	j *Jobs
}

func (w *readyWorker) Work(ctx context.Context, job *river.Job[readyArgs]) (err error) {
	defer func() { err = SnoozeUnavailable(ctx, w.j.cfg.Store, job.JobRow, err) }()
	hook := w.j.cfg.Registry.cfg.Hooks.ItemReady
	if hook == nil {
		return nil
	}
	if err := w.j.waitFor(ctx, job.Args.After); err != nil {
		return err
	}
	item, err := w.j.cfg.Registry.Item(job.Args.Ref)
	if err != nil {
		return river.JobCancel(err)
	}
	m, _, err := w.j.manifests.Get(ctx, item.Ref())
	if errors.Is(err, ErrNotFound) {
		return nil // deleted meanwhile
	} else if err != nil {
		return err
	}
	r := item.Kind().Readiness(m)
	if r.State == StateProcessing {
		return nil // a later job reports it
	}
	return pgx.BeginFunc(ctx, w.j.cfg.Pool, func(tx pgx.Tx) error { return hook(ctx, tx, item.Ref(), r) })
}

// purgeArgs relays public keys the worker overwrote or deleted to
// Hooks.PurgePublic.
type purgeArgs struct {
	Keys []string `json:"keys"`
}

func (purgeArgs) Kind() string { return "contentkit_media_purge" }

type purgeWorker struct {
	river.WorkerDefaults[purgeArgs]
	j *Jobs
}

func (w *purgeWorker) Work(ctx context.Context, job *river.Job[purgeArgs]) error {
	w.j.purge(ctx, job.Args.Keys)
	return nil
}

// purge hands public keys to Hooks.PurgePublic as URLs.
func (j *Jobs) purge(ctx context.Context, keys []string) {
	purge := j.cfg.Registry.cfg.Hooks.PurgePublic
	if purge == nil || len(keys) == 0 {
		return
	}
	urls := make([]string, 0, len(keys))
	for _, k := range keys {
		urls = append(urls, strings.TrimRight(j.cfg.Registry.cfg.BaseURL, "/")+layout.URLPrefix+k)
	}
	purge(ctx, urls)
}

// HostQueue is the media worker's handle on the host's River schema: it
// inserts the jobs the host runs on the worker's behalf (a folder's sweep
// after an edit, ItemReady and PurgePublic relays). It inserts only.
type HostQueue struct {
	client *river.Client[pgx.Tx]
	reg    *Registry
	queue  string
	grace  time.Duration
}

// NewHostQueue targets the host's River schema ("" is the connection's
// search path) and media queue ("" is DefaultQueue); grace is the host's
// JobsConfig.Grace (default 24 h).
func NewHostQueue(pool *pgxpool.Pool, reg *Registry, schema, queue string, grace time.Duration) (*HostQueue, error) {
	if pool == nil || reg == nil {
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
	return &HostQueue{client: c, reg: reg, queue: queue, grace: grace}, nil
}

func (h *HostQueue) insert(ctx context.Context, args river.JobArgs, o *river.InsertOpts) (*rivertype.JobInsertResult, error) {
	o.Queue = h.queue
	return h.client.Insert(ctx, args, o)
}

// ScheduleSweep implements SweepScheduler like Jobs.ScheduleSweep.
func (h *HostQueue) ScheduleSweep(ctx context.Context, ref contentref.ContentRef) error {
	item, err := h.reg.Item(ref)
	if err != nil {
		return err
	}
	return InsertOnce(ctx, h.insert, sweepArgs{Prefix: item.Prefix()}, river.InsertOpts{ScheduledAt: time.Now().Add(h.grace)})
}

// Ready asks the host to report ref's readiness (Hooks.ItemReady) once it
// has settled.
func (h *HostQueue) Ready(ctx context.Context, ref contentref.ContentRef) error {
	if _, err := h.reg.Item(ref); err != nil {
		return err
	}
	return InsertOnce(ctx, h.insert, readyArgs{Ref: ref}, river.InsertOpts{})
}

// Purge asks the host to purge public keys from its CDN (Hooks.PurgePublic).
func (h *HostQueue) Purge(ctx context.Context, keys []string) error {
	if len(keys) == 0 {
		return nil
	}
	_, err := h.insert(ctx, purgeArgs{Keys: keys}, &river.InsertOpts{})
	return err
}

// ExposeTx enqueues the host's Expose of refs in tx, a transaction on the
// host database, like Jobs.ExposeTx.
func (h *HostQueue) ExposeTx(ctx context.Context, tx pgx.Tx, refs ...contentref.ContentRef) error {
	return exposeTx(ctx, h.reg, tx, h.client, h.queue, refs)
}
