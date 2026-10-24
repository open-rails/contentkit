package media

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/open-rails/contentkit/media/layout"
)

// versionStorageRetirement authorizes only the old, nested storage address.
// The applied target commit proves the same logical version was copied before
// its old address was retired. It never changes the version's live projection.
type versionStorageRetirement struct {
	Target          string `json:"target"`
	TargetOperation string `json:"target_operation"`
	Final           bool   `json:"final"`
}

func versionStoragePair(source, target Item) bool {
	return source.Ref().Equal(target.Ref()) && source.Ref().Version() != "" && source.Kind().Versions &&
		source.Prefix() == source.Kind().nestedPrefix(source.Ref().ContentID, source.Ref().Version()) &&
		target.Prefix() == target.Kind().prefix(target.Ref().ContentID, target.Ref().Version()) && source.Prefix() != target.Prefix()
}

func versionStorageArea(source Item, key string) (string, bool) {
	if key == source.PublicPrefix()+layout.ErasureFenceName {
		return layout.AreaPublic, true
	}
	parsed, ok := layout.Parse(key)
	return parsed.Area, ok && parsed.Folder() == source.Prefix()
}

// scheduleVersionStorageRetirement supersedes every former cleanup obligation
// for the old address in the same transaction that cancels its queued timers.
// The copy and its verification happen before this call, with writers stopped.
func (j *Jobs) scheduleVersionStorageRetirement(ctx context.Context, source, target Item, targetOperation uuid.UUID, keys []string, due time.Time, riverSchema string) error {
	if !versionStoragePair(source, target) || targetOperation == uuid.Nil || due.IsZero() {
		return ErrCommitIdentity
	}
	jr := j.cfg.Journal.forTenant(source.Ref().TenantID)
	// Include stale objects and allocations, including PUTs which have not
	// arrived yet. This one-time inventory is never repeated by the cleaner.
	for object, err := range j.cfg.Store.List(ctx, source.Prefix()) {
		if err != nil {
			return err
		}
		if _, ok := versionStorageArea(source, object.Key); !ok {
			return ErrCommitIdentity
		}
		keys = append(keys, object.Key)
	}
	allocated, err := j.manifests.allocationKeys(ctx, source)
	if err != nil {
		return err
	}
	keys = append(keys, allocated...)
	keys = append(keys, source.ManifestKey())
	slices.Sort(keys)
	keys = slices.Compact(keys)
	for _, key := range keys {
		if _, ok := versionStorageArea(source, key); !ok {
			return ErrCommitIdentity
		}
	}
	if slices.Contains(keys, source.PublicPrefix()+layout.ErasureFenceName) {
		if _, err := j.cfg.Store.Head(ctx, target.PublicPrefix()+layout.ErasureFenceName); err != nil {
			return fmt.Errorf("media: old erasure fence has no copied target: %w", err)
		}
	}
	client, err := river.NewClient(riverpgxv5.New(jr.pool), &river.Config{Schema: riverSchema})
	if err != nil {
		return err
	}
	id := uuid.NewSHA1(targetOperation, []byte("version-storage-retirement:"+source.Prefix()))
	return pgx.BeginFunc(ctx, jr.pool, func(tx pgx.Tx) error {
		targetCommit, err := jr.load(ctx, tx, target.Ref().TenantID, targetOperation)
		if err != nil {
			return err
		}
		if targetCommit.State != "applied" || !targetCommit.Ref.Equal(target.Ref()) || targetCommit.Folder != target.Prefix() {
			return ErrCommitIdentity
		}
		if existing, err := jr.load(ctx, tx, source.Ref().TenantID, id); err == nil {
			if !existing.Ref.Equal(source.Ref()) || existing.Folder != source.Prefix() || existing.Effects.VersionStorage == nil ||
				existing.Effects.VersionStorage.TargetOperation != targetOperation.String() {
				return ErrCommitIdentity
			}
			return nil // already atomically scheduled; do not restart its grace
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		var pending bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM `+jr.table+` WHERE tenant_id=$1 AND folder_prefix=$2 AND state IN('open','prepared','frozen'))`, source.Ref().TenantID, source.Prefix()).Scan(&pending); err != nil {
			return err
		}
		if pending {
			return ErrCommitPending
		}
		rows, err := tx.Query(ctx, `SELECT id,state,scheduled_at,kind,args,queue,max_attempts FROM `+legacyCleanupTable(riverSchema)+`
WHERE `+legacyCleanupWhere+` AND args->>'prefix'=$1 FOR UPDATE`, source.Prefix())
		if err != nil {
			return err
		}
		type timer struct {
			ID          int64
			State       string
			Due         time.Time
			Kind        string
			Args        []byte
			Queue       string
			MaxAttempts int
		}
		timers, err := pgx.CollectRows(rows, pgx.RowToStructByPos[timer])
		if err != nil {
			return err
		}
		for _, timer := range timers {
			if timer.State == "running" {
				return fmt.Errorf("media: stop old version cleanup before migrating running job %d", timer.ID)
			}
			due = maxTime(due, timer.Due)
		}
		var previousDue *time.Time
		var finite bool
		var oldOperations []uuid.UUID
		err = tx.QueryRow(ctx, `SELECT CASE WHEN isfinite(due_at) THEN due_at END,isfinite(due_at),deletion_operations FROM `+jr.cleanup+` WHERE tenant_id=$1 AND folder_prefix=$2 FOR UPDATE`, source.Ref().TenantID, source.Prefix()).Scan(&previousDue, &finite, &oldOperations)
		if err == nil {
			if !finite {
				return errors.New("media: initialize cleanup deadlines before moving version storage")
			}
			due = maxTime(due, *previousDue)
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		for _, timer := range timers {
			if timer.Kind != "contentkit_media_delete_folder" {
				continue
			}
			var args struct {
				Final     bool
				Operation string
			}
			if err := json.Unmarshal(timer.Args, &args); err != nil {
				return err
			}
			if args.Final {
				operation, err := uuid.Parse(args.Operation)
				if err != nil {
					return ErrCommitIdentity
				}
				oldOperations = append(oldOperations, operation)
			}
		}
		// Retired allocations and recorded public targets may not exist in S3
		// yet. Their original receipts remain the authority for late uploads.
		for _, operation := range oldOperations {
			old, err := jr.load(ctx, tx, source.Ref().TenantID, operation)
			if err != nil {
				return err
			}
			if old.State != "applied" || !old.Ref.Equal(source.Ref()) || old.Folder != source.Prefix() {
				return ErrCommitIdentity
			}
			keys = append(keys, old.Effects.Private...)
			keys = append(keys, old.Effects.Public...)
			for _, mirror := range old.Effects.Mirror {
				keys = append(keys, mirror.To)
			}
		}
		slices.Sort(keys)
		keys = slices.Compact(keys)
		effects := journalEffects{Cleanup: true, VersionStorage: &versionStorageRetirement{Target: target.Prefix(), TargetOperation: targetOperation.String()}}
		for _, key := range keys {
			area, ok := versionStorageArea(source, key)
			if !ok {
				return ErrCommitIdentity
			}
			if area == layout.AreaPublic {
				effects.Public = append(effects.Public, key)
			} else {
				effects.Private = append(effects.Private, key)
			}
		}
		body, err := json.Marshal(effects)
		if err != nil {
			return err
		}
		fingerprint := sha256.Sum256(body)
		_, err = tx.Exec(ctx, `INSERT INTO `+jr.table+`(tenant_id,operation_id,content_kind,content_id,content_version_id,folder_prefix,actor_id,fingerprint,lease_id,state,effects)
VALUES($1,$2,$3,$4,$5,$6,'migration:version-storage',$7,$8,'applied',$9)`, source.Ref().TenantID, id, source.Ref().ContentKind,
			source.Ref().ContentID, source.Ref().ContentVersionID, source.Prefix(), fingerprint[:], uuid.New(), body)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO `+jr.cleanup+` AS c (tenant_id,folder_prefix,content_kind,due_at,deletion_operations)
VALUES($1,$2,$3,$4,$5) ON CONFLICT(tenant_id,folder_prefix) DO UPDATE SET due_at=EXCLUDED.due_at,
deletion_operations=EXCLUDED.deletion_operations,revision=gen_random_uuid()`, source.Ref().TenantID, source.Prefix(), source.Ref().ContentKind, due, []uuid.UUID{id})
		if err != nil {
			return err
		}
		for _, timer := range timers {
			if timer.Kind == "contentkit_media_delete_folder" {
				var args struct {
					deleteFolderArgs
					Final bool
				}
				if err := json.Unmarshal(timer.Args, &args); err != nil {
					return err
				}
				if !args.Final {
					if _, err := uuid.Parse(args.Incarnation); err != nil {
						return ErrCommitIdentity
					}
					args.Prefix = target.Prefix()
					if _, err := client.InsertTx(ctx, tx, args.deleteFolderArgs, &river.InsertOpts{
						Queue: timer.Queue, ScheduledAt: timer.Due, MaxAttempts: timer.MaxAttempts,
					}); err != nil {
						return err
					}
				}
			}
			if _, err := client.JobCancelTx(ctx, tx, timer.ID); err != nil {
				return err
			}
		}
		return nil
	})
}

func (j *Jobs) cleanVersionStorage(ctx context.Context, source Item, operations []uuid.UUID) error {
	if len(operations) != 1 {
		return errors.New("media: retired version storage requires one canonical receipt")
	}
	jr := j.cfg.Journal.forTenant(source.Ref().TenantID)
	var receipt manifestCommit
	err := pgx.BeginFunc(ctx, jr.pool, func(tx pgx.Tx) error {
		var err error
		receipt, err = jr.load(ctx, tx, source.Ref().TenantID, operations[0])
		if err != nil {
			return err
		}
		r := receipt.Effects.VersionStorage
		if r == nil || receipt.Actor != "migration:version-storage" || receipt.State != "applied" ||
			!receipt.Ref.Equal(source.Ref()) || receipt.Folder != source.Prefix() || len(receipt.Effects.Mirror) != 0 {
			return ErrCommitIdentity
		}
		target, err := j.cfg.Registry.Item(source.Ref())
		if err != nil || !versionStoragePair(source, target) || r.Target != target.Prefix() {
			return ErrCommitIdentity
		}
		id, err := uuid.Parse(r.TargetOperation)
		if err != nil {
			return ErrCommitIdentity
		}
		c, err := jr.load(ctx, tx, source.Ref().TenantID, id)
		if err != nil {
			return err
		}
		if c.State != "applied" || !c.Ref.Equal(target.Ref()) || c.Folder != target.Prefix() {
			return ErrCommitIdentity
		}
		return nil
	})
	if err != nil {
		return err
	}
	keys := append(slices.Clone(receipt.Effects.Private), receipt.Effects.Public...)
	for _, key := range keys {
		if _, ok := versionStorageArea(source, key); !ok {
			return ErrCommitIdentity
		}
	}
	if err := j.manifests.retireAllocations(ctx, source, receipt.Effects.Private); err != nil {
		return err
	}
	for _, key := range keys {
		if err := j.cfg.Store.Delete(ctx, key); err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
	}
	if receipt.Effects.VersionStorage.Final {
		return nil
	}
	return pgx.BeginFunc(ctx, jr.pool, func(tx pgx.Tx) error {
		changed, err := tx.Exec(ctx, `UPDATE `+jr.table+` SET effects=jsonb_set(effects,'{version_storage,final}','true'),updated_at=now()
WHERE tenant_id=$1 AND operation_id=$2 AND effects->'version_storage'->>'final'='false'`, source.Ref().TenantID, receipt.ID)
		if err != nil || changed.RowsAffected() == 0 {
			return err
		}
		return jr.scheduleCleanupTx(ctx, tx, source.Ref(), source.Prefix(), j.cfg.Now().Add(j.cfg.LateUploadWindow), operations, false)
	})
}
