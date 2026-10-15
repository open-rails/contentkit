package media

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/contentkit/media/layout"
	"github.com/riverqueue/river"
)

// ErrErasurePending reports bytes still present after an attempted account
// erasure. The caller must retry; queued work alone is not completion.
var ErrErasurePending = errors.New("media: erasure cleanup remains pending")

// Erase permanently tombstones an item, synchronously deletes its known
// allocations and public images, then verifies storage. Unlike Purge, it never
// resets the item to a writable lifetime. Retries repeat the same exact cleanup
// targets, and a durable delayed pass catches already-issued uploads arriving
// after the first deletion. Supply JobsConfig.ErasureQueue for recovery without
// a running host; otherwise compose the host River contribution first.
func (j *Jobs) Erase(ctx context.Context, d Deletion) error {
	item, err := j.cfg.Registry.Item(d.Ref)
	if err != nil {
		return err
	}
	if d.OperationID == "" {
		return errors.New("media: erasure needs a stable operation ID")
	}
	operation := uuid.NewSHA1(uuid.NameSpaceURL, []byte("erase:"+item.Prefix()+d.OperationID))
	fence := item.PublicPrefix() + layout.ErasureFenceName
	sum := sha256.Sum256(nil)
	if _, err = j.cfg.Store.Put(ctx, fence, bytes.NewReader(nil), 0, PutOptions{IfNoneMatch: "*", ContentType: "application/octet-stream", CacheControl: "no-store", ChecksumSHA256: sum[:]}); err != nil && !errors.Is(err, ErrPreconditionFailed) {
		return err
	}
	if err = j.deleteItem(ctx, item, d.Owner, operation, "", time.Time{}); err != nil {
		return err
	}
	if err = j.repeatDeletion(ctx, item, operation); err != nil {
		return err
	}
	args := deleteFolderArgs{Prefix: item.Prefix(), Final: true, Operation: operation.String()}
	opts := &river.InsertOpts{ScheduledAt: j.cfg.Now().Add(j.cfg.LateUploadWindow), UniqueOpts: PendingOnce, MaxAttempts: 3}
	if j.cfg.ErasureQueue != nil {
		_, err = j.cfg.ErasureQueue.insert(ctx, args, opts)
	} else {
		_, err = j.Insert(ctx, args, opts)
	}
	if err != nil {
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
