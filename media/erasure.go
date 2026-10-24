package media

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media/layout"
)

// ErrErasurePending reports bytes still present after an attempted account
// erasure. The caller must retry; queued work alone is not completion.
var ErrErasurePending = errors.New("media: erasure cleanup remains pending")

// ClaimErasureTx records irreversible version erasure under the catalog's
// eligibility locks. It sends no storage writes: the caller commits this
// intent before Erase, so even an ambiguous later S3 PUT cannot race restore.
func (j *Jobs) ClaimErasureTx(ctx context.Context, tx pgx.Tx, ref contentref.ContentRef) error {
	if tx == nil || ref.Version() == "" {
		return errors.New("media: version erasure needs its catalog transaction")
	}
	item, err := j.cfg.Registry.Item(ref)
	if err != nil {
		return err
	}
	if err := j.cfg.Journal.checkFolderOwner(ctx, tx, item, false); err != nil {
		return err
	}
	return j.cfg.Journal.claimErasureTx(ctx, tx, item)
}

// ErasureStartedTx reads the durable intent before the storage fence. Restore
// holds the same catalog locks used to commit that intent. The storage check
// also recognizes erasure performed before durable catalog claims existed.
func (j *Jobs) ErasureStartedTx(ctx context.Context, tx pgx.Tx, ref contentref.ContentRef) (bool, error) {
	item, err := j.cfg.Registry.Item(ref)
	if err != nil {
		return false, err
	}
	journal := j.cfg.Journal.forTenant(ref.TenantID)
	var started bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM `+journal.table+`
 WHERE tenant_id=$1 AND content_kind=$2 AND content_id=$3 AND content_version_id IS NOT DISTINCT FROM $4::text
 AND effects->>'erasure'='true')`, ref.TenantID, ref.ContentKind, ref.ContentID, ref.ContentVersionID).Scan(&started); err != nil || started {
		return started, err
	}
	_, err = j.cfg.Store.Head(ctx, item.PublicPrefix()+layout.ErasureFenceName)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

func (j *Jobs) fenceErasure(ctx context.Context, item Item) error {
	if err := RequireConditionalPut(ctx, j.cfg.Store); err != nil {
		return err
	}
	sum := sha256.Sum256(nil)
	_, err := j.cfg.Store.Put(ctx, item.PublicPrefix()+layout.ErasureFenceName, bytes.NewReader(nil), 0,
		PutOptions{IfNoneMatch: "*", ContentType: "application/octet-stream", CacheControl: "no-store", ChecksumSHA256: sum[:]})
	if errors.Is(err, ErrPreconditionFailed) {
		return nil
	}
	return err
}

// Erase permanently tombstones an item, synchronously deletes its known
// allocations and public images, then verifies storage. Unlike Purge, it never
// resets the item to a writable lifetime. Retries repeat the same exact cleanup
// targets, and a durable delayed pass catches already-issued uploads arriving
// after the first deletion. Its deadline commits with the deletion journal,
// so erasure does not need a running or bound River client.
func (j *Jobs) Erase(ctx context.Context, d Deletion) error {
	ctx = context.WithValue(ctx, purgeContextKey{}, true)
	item, err := j.cfg.Registry.Item(d.Ref)
	if err != nil {
		return err
	}
	if d.OperationID == "" {
		return errors.New("media: erasure needs a stable operation ID")
	}
	operation := uuid.NewSHA1(uuid.NameSpaceURL, []byte("erase:"+item.Prefix()+d.OperationID))
	fence := item.PublicPrefix() + layout.ErasureFenceName
	if err = j.fenceErasure(ctx, item); err != nil {
		return err
	}
	if err = j.deleteItem(ctx, item, d.Owner, operation, "", time.Time{}, true); err != nil {
		return err
	}
	if err = j.repeatDeletion(ctx, item, operation); err != nil {
		return err
	}
	cur, _, err := j.manifests.get(ctx, item.ManifestKey())
	if err != nil {
		return err
	}
	if !cur.Deleted || len(cur.Files) != 0 {
		return ErrErasurePending
	}
	for obj, err := range j.cfg.Store.List(ctx, item.Prefix()) {
		if err != nil {
			return err
		}
		if obj.Key == item.ManifestKey() || obj.Key == fence {
			continue
		}
		if k, ok := layout.Parse(obj.Key); ok && k.Folder() != item.Prefix() {
			continue
		}
		return fmt.Errorf("%w: %s", ErrErasurePending, obj.Key)
	}
	return nil
}
