package mediastore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"

	"github.com/open-rails/contentkit/contentref"
	media "github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/layout"
)

// errFenceConflict: the fence kept changing while erasure replaced it.
var errFenceConflict = errors.New("media: gateway fence kept changing")

// fence reads item's gateway fence: "" when it has none, else its kind. A
// fence without the deletion mark is permanent erasure.
func (j *Jobs) fence(ctx context.Context, item Item) (media.Object, string, error) {
	obj, err := j.cfg.Store.Head(ctx, item.PublicPrefix()+layout.FenceName)
	switch {
	case errors.Is(err, ErrNotFound):
		return obj, "", nil
	case err != nil:
		return obj, "", err
	case obj.Metadata[layout.FenceMeta] == layout.FenceDeleted:
		return obj, layout.FenceDeleted, nil
	}
	return obj, layout.FenceErased, nil
}

// putFence creates item's fence of kind, or replaces the one whose ETag is match.
func (j *Jobs) putFence(ctx context.Context, item Item, kind, match string) error {
	if err := RequireConditionalPut(ctx, j.cfg.Store); err != nil {
		return err
	}
	sum := sha256.Sum256(nil)
	o := media.PutOptions{IfMatch: match, ContentType: "application/octet-stream", CacheControl: "no-store",
		ChecksumSHA256: sum[:], Metadata: map[string]string{layout.FenceMeta: kind}}
	if match == "" {
		o.IfNoneMatch = "*"
	}
	_, err := j.cfg.Store.Put(ctx, item.PublicPrefix()+layout.FenceName, bytes.NewReader(nil), 0, o)
	return err
}

// FenceDeleted makes ref's gateway fence follow its soft deletion: deleted
// fences the item; otherwise only a deletion fence is removed, never an
// erasure. The caller holds the rows that decide deleted (a version's catalog
// rows, an entity's row), which an erasure or retirement claim must lock too,
// so neither changes meanwhile.
func (j *Jobs) FenceDeleted(ctx context.Context, ref contentref.ContentRef, deleted bool) error {
	item, err := j.cfg.Registry.Item(ref)
	if err != nil {
		return err
	}
	_, kind, err := j.fence(ctx, item)
	switch {
	case err != nil:
		return err
	case deleted && kind == "":
		if err := j.putFence(ctx, item, layout.FenceDeleted, ""); !errors.Is(err, ErrPreconditionFailed) {
			return err
		}
	case !deleted && kind == layout.FenceDeleted:
		if err := j.cfg.Store.Delete(ctx, item.PublicPrefix()+layout.FenceName); !errors.Is(err, ErrNotFound) {
			return err
		}
	}
	return nil
}

// fenceErasure makes item's fence permanent, replacing a deletion fence. Erase
// and the retirement of a deleted item call it.
func (j *Jobs) fenceErasure(ctx context.Context, item Item) error {
	for range 5 {
		obj, kind, err := j.fence(ctx, item)
		if err != nil || kind == layout.FenceErased {
			return err
		}
		match := ""
		if kind == layout.FenceDeleted {
			match = obj.ETag
		}
		if err := j.putFence(ctx, item, layout.FenceErased, match); !errors.Is(err, ErrPreconditionFailed) {
			return err
		}
	}
	return errFenceConflict
}
