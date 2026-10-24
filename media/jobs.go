package media

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media/layout"
)

// JobsConfig configures media's River jobs in the host.
type JobsConfig struct {
	Store     Store
	Registry  *Registry
	Locker    Locker       // serializes manifest edits and sweep deletion; required
	Journal   *PGJournal   // same ContentKit journal used by the uploader and media worker; required
	Processes ProcessQueue // the worker's queue (workqueue.Queue)
	// Grace bounds unfinished, recorded private allocations. Default 24 h.
	Grace time.Duration
	// LateUploadWindow retains exact-key cleanup after folder deletion or
	// allocation retirement, catching writes that land after the first pass;
	// at least the seven-day multipart lifetime plus one hour.
	LateUploadWindow time.Duration
	Limiter          *PGLimiter // optional; must share the journal's pool and schema
	Logger           *slog.Logger
	Now              func() time.Time
}

// Jobs holds synchronous storage operations used by ingest and content purge.
// It registers no workers, queues, timers, or background loops.
type Jobs struct {
	cfg       JobsConfig
	manifests *Manifests
}

func NewJobs(cfg JobsConfig) (*Jobs, error) {
	if cfg.Store == nil || cfg.Registry == nil || cfg.Locker == nil || cfg.Journal == nil {
		return nil, errors.New("media: Jobs needs a Store, a Registry, a Locker and a Journal")
	}
	if cfg.Limiter != nil && (cfg.Limiter.pool != cfg.Journal.pool || cfg.Limiter.usage != cfg.Journal.limiter.usage) {
		return nil, errors.New("media: deletion quota must use the journal's pool and ContentKit schema")
	}
	if cfg.Grace <= 0 {
		cfg.Grace = 24 * time.Hour
	}
	if cfg.LateUploadWindow < minimumLateUploadWindow {
		cfg.LateUploadWindow = minimumLateUploadWindow
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

// RetireItemsTx records folder deletion with its owning row's transaction.
// Only the existing content-purge sweep executes these durable intents.
func (j *Jobs) RetireItemsTx(ctx context.Context, tx pgx.Tx, items ...Deletion) error {
	for _, d := range items {
		item, err := j.cfg.Registry.Item(d.Ref)
		if err != nil {
			return err
		}
		m, err := j.manifests.ensureIncarnation(ctx, item)
		if err != nil {
			return err
		}
		owner := ""
		if j.cfg.Limiter != nil {
			owner = d.Owner
		}
		if err := j.cfg.Journal.retireItemTx(ctx, tx, item, owner, m.Incarnation, j.cfg.Now()); err != nil {
			return err
		}
	}
	return nil
}

func (j *PGJournal) retireItemTx(ctx context.Context, tx pgx.Tx, item Item, owner, incarnation string, due time.Time) error {
	if id, err := uuid.Parse(incarnation); err != nil || id == uuid.Nil {
		return ErrCommitIdentity
	}
	effects, err := json.Marshal(journalEffects{FolderDelete: &folderDeletion{Owner: owner, Incarnation: incarnation}})
	if err != nil {
		return err
	}
	operation := uuid.NewSHA1(uuid.NameSpaceURL, []byte("folder-retirement:"+item.Prefix()+incarnation))
	fingerprint := sha256.Sum256(effects)
	jr := j.forTenant(item.Ref().TenantID)
	if err := jr.lockAdmission(ctx, tx, item.Prefix()); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO `+jr.table+`
(tenant_id,operation_id,content_kind,content_id,content_version_id,folder_prefix,actor_id,fingerprint,lease_id,state,effects)
VALUES($1,$2,$3,$4,$5,$6,'',$7,$8,'applied',$9) ON CONFLICT(tenant_id,operation_id) DO NOTHING`,
		item.Ref().TenantID, operation, item.Ref().ContentKind, item.Ref().ContentID, item.Ref().ContentVersionID, item.Prefix(), fingerprint[:], uuid.New(), effects)
	if err != nil {
		return err
	}
	c, err := jr.load(ctx, tx, item.Ref().TenantID, operation)
	if err != nil {
		return err
	}
	if !c.Ref.Equal(item.Ref()) || c.Fingerprint != fingerprint || c.Effects.FolderDelete == nil {
		return ErrCommitIdentity
	}
	if !c.Effects.FolderDelete.Completed {
		// Initial erasure must not wait behind an older late-write deadline.
		_, err = tx.Exec(ctx, `INSERT INTO `+jr.cleanup+` AS c (tenant_id,folder_prefix,content_kind,due_at,deletion_operations)
VALUES($1,$2,$3,$4,$5) ON CONFLICT(tenant_id,folder_prefix) DO UPDATE SET due_at=least(c.due_at,EXCLUDED.due_at),
deletion_operations=ARRAY(SELECT DISTINCT op FROM unnest(c.deletion_operations || EXCLUDED.deletion_operations) op),revision=gen_random_uuid()`,
			item.Ref().TenantID, item.Prefix(), item.Ref().ContentKind, due, []uuid.UUID{operation})
		if err != nil {
			return err
		}
	}
	return nil
}

// RetirementStartedTx lets the owner refuse restore after an irreversible
// folder-retirement claim. Callers hold their ordinary owning-row locks.
func (j *Jobs) RetirementStartedTx(ctx context.Context, tx pgx.Tx, ref contentref.ContentRef) (bool, error) {
	item, err := j.cfg.Registry.Item(ref)
	if err != nil {
		return false, err
	}
	jr := j.cfg.Journal.forTenant(ref.TenantID)
	var started bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM `+jr.table+` WHERE tenant_id=$1 AND folder_prefix=$2 AND effects ? 'folder_delete')`, ref.TenantID, item.Prefix()).Scan(&started)
	return started, err
}

// RetirementComplete reports the first physical deletion of every committed
// lifetime. Exact late-write receipts remain owned by the same purge sweep.
func (j *Jobs) RetirementComplete(ctx context.Context, ref contentref.ContentRef) (bool, error) {
	item, err := j.cfg.Registry.Item(ref)
	if err != nil {
		return false, err
	}
	jr := j.cfg.Journal.forTenant(ref.TenantID)
	var complete bool
	err = jr.pool.QueryRow(ctx, `SELECT count(*)>0 AND coalesce(bool_and(coalesce(effects->'folder_delete'->>'completed'='true',false)),false)
FROM `+jr.table+` WHERE tenant_id=$1 AND folder_prefix=$2 AND effects ? 'folder_delete'`, ref.TenantID, item.Prefix()).Scan(&complete)
	return complete, err
}

// Purge clears and resets an item to a fresh incarnation. It needs a caller-
// stable OperationID, even without quota; retry a failed reset with that ID.
// Hosts deleting content commit RetireItemsTx and let content purge execute it.
func (j *Jobs) Purge(ctx context.Context, d Deletion) error {
	ctx = context.WithValue(ctx, purgeContextKey{}, true)
	item, err := j.cfg.Registry.Item(d.Ref)
	if err != nil {
		return err
	}
	if d.OperationID == "" {
		return errors.New("media: purge needs an operation id")
	}
	operation := uuid.NewSHA1(uuid.NameSpaceURL, []byte("purge:"+item.Prefix()+d.OperationID))
	if err := j.deleteItem(ctx, item, d.Owner, operation, "", time.Time{}, false); err != nil {
		return err
	}
	if err := j.repeatDeletion(ctx, item, operation); err != nil {
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
