package media

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
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
	// Grace bounds unfinished, recorded private allocations. Default 24 h.
	Grace time.Duration
	// CleanupInterval is the interval between bounded due-cleanup passes.
	// Idle passes query PostgreSQL without listing the bucket. Default 24 h.
	CleanupInterval time.Duration
	// LateUploadWindow retains exact-key cleanup after folder deletion or
	// allocation retirement, catching writes that land after the first pass;
	// at least the seven-day multipart lifetime plus one hour.
	LateUploadWindow time.Duration
	Limiter          *PGLimiter // optional; must share the journal's pool and schema
	Queue            string     // default DefaultQueue
	MaxWorkers       int        // default 2
	Logger           *slog.Logger
	Now              func() time.Time
}

// DefaultQueue is JobsConfig.Queue's default.
const DefaultQueue = "contentkit_media"

// Jobs is media's River contribution to the host: sweep, folder deletion,
// Expose, and the worker's readiness relay. Processing runs
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
	if (cfg.Registry.cfg.Hooks.ItemReady != nil || cfg.Registry.cfg.Hooks.ItemProgressed != nil) && cfg.Pool == nil {
		return nil, errors.New("media: Hooks.ItemReady and Hooks.ItemProgressed need JobsConfig.Pool")
	}
	if cfg.Limiter != nil && (cfg.Limiter.pool != cfg.Journal.pool || cfg.Limiter.usage != cfg.Journal.limiter.usage) {
		return nil, errors.New("media: deletion quota must use the journal's pool and ContentKit schema")
	}
	if cfg.Grace <= 0 {
		cfg.Grace = 24 * time.Hour
	}
	if cfg.CleanupInterval <= 0 {
		cfg.CleanupInterval = 24 * time.Hour
	}
	if cfg.LateUploadWindow < minimumLateUploadWindow {
		cfg.LateUploadWindow = minimumLateUploadWindow
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
	if j.manifests, err = NewManifests(cfg.Store, cfg.Registry, ManifestOptions{Locker: cfg.Locker, Journal: cfg.Journal, CleanupGrace: cfg.Grace, LateUploadWindow: cfg.LateUploadWindow}); err != nil {
		return nil, err
	}
	return j, nil
}

// Manifests is the jobs' manifest store; hosts may share it.
func (j *Jobs) Manifests() *Manifests { return j.manifests }

// EnqueueProcessing brings an existing item's derived files up to date after
// a version-level selection or ordering edit.
func (j *Jobs) EnqueueProcessing(ctx context.Context, job ProcessJob) error {
	if j.cfg.Processes == nil {
		return errors.New("media: processing queue is not configured")
	}
	return j.cfg.Processes.Enqueue(ctx, job)
}

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
			func() error { return river.AddWorkerSafely(cfg.Workers, &cleanupDueWorker{j: j}) },
			func() error { return river.AddWorkerSafely(cfg.Workers, &deleteFolderWorker{j: j}) },
			func() error { return river.AddWorkerSafely(cfg.Workers, &exposeWorker{j: j}) },
			func() error { return river.AddWorkerSafely(cfg.Workers, &readyWorker{j: j}) },
			func() error { return river.AddWorkerSafely(cfg.Workers, &recoverWorker{j: j}) },
		} {
			if err := add(); err != nil {
				return err
			}
		}
		interval := j.cfg.CleanupInterval
		cfg.PeriodicJobs = append(cfg.PeriodicJobs, riverhelpers.NewPeriodicJob(cfg, interval,
			func() (river.JobArgs, *river.InsertOpts) {
				args := cleanupDueArgs{}
				opts := args.InsertOpts()
				opts.Queue, opts.MaxAttempts = j.cfg.Queue, 3
				return args, &opts
			}, &river.PeriodicJobOpts{ID: "contentkit_media_cleanup_due", RunOnStart: true}))
		cfg.PeriodicJobs = append(cfg.PeriodicJobs, riverhelpers.NewPeriodicJob(cfg, time.Minute,
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

// ScheduleSweep records one due-cleanup obligation, without creating a River
// job. Successful manifest edits already record this atomically themselves.
func (j *Jobs) ScheduleSweep(ctx context.Context, ref contentref.ContentRef) error {
	item, err := j.cfg.Registry.Item(ref)
	if err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, j.cfg.Journal.pool, func(tx pgx.Tx) error {
		return j.cfg.Journal.scheduleCleanupTx(ctx, tx, item.Ref(), item.Prefix(), j.cfg.Now().Add(j.cfg.Grace), nil, false)
	})
}

// Deletion is one item to delete. Owner is its quota owner (UploadGrant.Owner),
// "" for none; with a Limiter configured the owner's usage is released.
// OperationID is required for Purge; queued deletion uses its River job id.
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
		m, err := j.manifests.deletionIncarnation(ctx, item)
		if err != nil {
			return err
		}
		args := deleteFolderArgs{Prefix: item.Prefix(), Incarnation: m.Incarnation}
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

// Purge clears and resets an item to a fresh incarnation. It needs a caller-
// stable OperationID, even without quota; retry a failed reset with that ID.
// Hosts deleting content use DeleteItemsTx.
func (j *Jobs) Purge(ctx context.Context, d Deletion) error {
	item, err := j.cfg.Registry.Item(d.Ref)
	if err != nil {
		return err
	}
	if d.OperationID == "" {
		return errors.New("media: purge needs an operation id")
	}
	operation := uuid.NewSHA1(uuid.NameSpaceURL, []byte("purge:"+item.Prefix()+d.OperationID))
	if err := j.deleteItem(ctx, item, d.Owner, operation, "", time.Time{}); err != nil {
		return err
	}
	return j.resetItem(ctx, item, operation)
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

// parseFolder guards every job's prefix: exactly an item folder,
// "{ns}/{kind}/{id}/" or a version's "{ns}/{kind}/{id}/{version}/".
func parseFolder(prefix string) (contentref.ContentRef, error) {
	k, ok := layout.ParseFolder(prefix)
	if !ok || contentref.ValidateID(k.ID) != nil || k.Version != "" && contentref.ValidateID(k.Version) != nil {
		return contentref.ContentRef{}, fmt.Errorf("media: invalid folder prefix %q", prefix)
	}
	return contentref.New(k.Namespace, k.Kind, k.ID).WithVersion(k.Version), nil
}

// folderItem also binds the serialized path shape. A legacy-looking path
// whose namespace equals an internal scoped partition must not alias a new root.
func (r *Registry) folderItem(prefix string) (Item, error) {
	ref, err := parseFolder(prefix)
	if err != nil {
		return Item{}, err
	}
	item, err := r.Item(ref)
	if err != nil {
		return Item{}, err
	}
	if item.Prefix() != prefix {
		return Item{}, fmt.Errorf("media: folder does not match registry scope %q", prefix)
	}
	return item, nil
}

// itemFolders lists the item folders of kind k in key order: each item's,
// and each content's versions' before its own.
func (j *Jobs) itemFolders(ctx context.Context, k *Kind) iter.Seq2[contentref.ContentRef, error] {
	return func(yield func(contentref.ContentRef, error) bool) {
		last := ""
		for o, err := range j.cfg.Store.List(ctx, k.root()+k.Name+"/") {
			if err != nil {
				yield(contentref.ContentRef{}, err)
				return
			}
			key, ok := layout.Parse(o.Key)
			if !ok || key.Folder() == last {
				continue
			}
			last = key.Folder()
			if k.Versions && key.Version != "" {
				continue // retained nested source folders belong only to migration/cleanup
			}
			item, err := j.itemForFolder(ctx, last)
			if err != nil {
				if !yield(contentref.ContentRef{}, err) {
					return
				}
				continue
			}
			if !yield(item.Ref(), nil) {
				return
			}
		}
	}
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
	for ref, err := range j.itemFolders(ctx, k) {
		if err != nil {
			return n, err
		}
		if _, err := j.cfg.Registry.Item(ref); err != nil {
			continue
		}
		if err := j.cfg.Processes.Enqueue(ctx, ProcessJob{Ref: ref, Preset: preset, Class: VideoBackfill}); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
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

type deleteFolderArgs struct {
	Prefix      string `json:"prefix"`
	Owner       string `json:"owner,omitempty"`
	Incarnation string `json:"incarnation,omitempty"`
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
	item, err := w.j.itemForFolder(ctx, a.Prefix)
	if errors.Is(err, ErrLayoutChanged) {
		return err
	}
	if err != nil {
		return river.JobCancel(err)
	}
	if a.Incarnation == "" {
		return river.JobCancel(errors.New("media: queued deletion needs an incarnation"))
	}
	operation := uuid.NewSHA1(uuid.NameSpaceURL, []byte(fmt.Sprintf("folder-delete:%s:%d", a.Prefix, job.JobRow.ID)))
	if err := w.j.deleteItem(ctx, item, a.Owner, operation, a.Incarnation, time.Time{}); err != nil {
		return err
	}
	return nil
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
	hooks := w.j.cfg.Registry.cfg.Hooks
	if hooks.ItemReady == nil && hooks.ItemProgressed == nil {
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
		if hooks.ItemProgressed == nil || !item.Kind().Playable(m) {
			return nil // a later job reports it
		}
		return pgx.BeginFunc(ctx, w.j.cfg.Pool, func(tx pgx.Tx) error { return hooks.ItemProgressed(ctx, tx, item.Ref()) })
	}
	if hooks.ItemReady == nil {
		return nil
	}
	return pgx.BeginFunc(ctx, w.j.cfg.Pool, func(tx pgx.Tx) error { return hooks.ItemReady(ctx, tx, item.Ref(), r) })
}

// HostQueue is the media worker's handle on the host's River schema: it
// inserts readiness and Expose relays on the worker's behalf.
// Cleanup is recorded in the shared journal, without queueing a relay.
type HostQueue struct {
	client *river.Client[pgx.Tx]
	reg    *Registry
	queue  string
}

// NewHostQueue targets the host's River schema ("" is the connection's
// search path) and media queue ("" is DefaultQueue).
func NewHostQueue(pool *pgxpool.Pool, reg *Registry, schema, queue string) (*HostQueue, error) {
	if pool == nil || reg == nil {
		return nil, errors.New("media: HostQueue needs a pool and a Registry")
	}
	if queue == "" {
		queue = DefaultQueue
	}
	c, err := river.NewClient(riverpgxv5.New(pool), &river.Config{Schema: schema})
	if err != nil {
		return nil, err
	}
	return &HostQueue{client: c, reg: reg, queue: queue}, nil
}

func (h *HostQueue) insert(ctx context.Context, args river.JobArgs, o *river.InsertOpts) (*rivertype.JobInsertResult, error) {
	o.Queue = h.queue
	return h.client.Insert(ctx, args, o)
}

// Ready asks the host to report ref's readiness (Hooks.ItemReady) once it
// has settled, or its progress (Hooks.ItemProgressed) while it plays but
// still processes.
func (h *HostQueue) Ready(ctx context.Context, ref contentref.ContentRef) error {
	if _, err := h.reg.Item(ref); err != nil {
		return err
	}
	return InsertOnce(ctx, h.insert, readyArgs{Ref: ref}, river.InsertOpts{})
}

// ExposeTx enqueues the host's Expose of refs in tx, a transaction on the
// host database, like Jobs.ExposeTx.
func (h *HostQueue) ExposeTx(ctx context.Context, tx pgx.Tx, refs ...contentref.ContentRef) error {
	return exposeTx(ctx, h.reg, tx, h.client, h.queue, refs)
}
