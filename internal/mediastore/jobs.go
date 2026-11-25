package mediastore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"slices"
	"time"

	"github.com/open-rails/contentkit/contentref"
	media "github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/layout"
)

// JobsConfig configures media's lifecycle operations.
type JobsConfig struct {
	Manifests *Manifests
	Processes ProcessQueue // the worker's queue (workqueue.Queue); optional
	Logger    *slog.Logger
	Now       func() time.Time
}

// Jobs deletes, restores, erases and collects items. Every decision reads
// the item's manifest: nothing about files lives in Postgres.
type Jobs struct {
	cfg       JobsConfig
	manifests *Manifests
	store     media.Store
	reg       *Registry
}

func NewJobs(cfg JobsConfig) (*Jobs, error) {
	if cfg.Manifests == nil {
		return nil, errors.New("media: Jobs needs Manifests")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Jobs{cfg: cfg, manifests: cfg.Manifests, store: cfg.Manifests.store, reg: cfg.Manifests.reg}, nil
}

// Manifests is the jobs' manifest store.
func (j *Jobs) Manifests() *Manifests { return j.manifests }

// EnqueueProcessing brings an existing item's derived files up to date after
// a version-level selection or ordering edit.
func (j *Jobs) EnqueueProcessing(ctx context.Context, job ProcessJob) error {
	if j.cfg.Processes == nil {
		return errors.New("media: processing queue is not configured")
	}
	return j.cfg.Processes.Enqueue(ctx, job)
}

// ErrErasurePending reports bytes still present after an erasure. Retry it.
var ErrErasurePending = errors.New("media: erasure cleanup remains pending")

// SetDeleted soft-deletes ref (deleted) or restores it: the manifest records
// the deletion and the gateway fence closes the item at once; files are
// kept. An erased item stays erased: a stale sync changes nothing.
func (j *Jobs) SetDeleted(ctx context.Context, ref contentref.ContentRef, deleted bool) error {
	item, err := j.reg.Item(ref)
	if err != nil {
		return err
	}
	if deleted {
		if err := j.fenceDeleted(ctx, item); err != nil {
			return err
		}
	}
	_, err = j.manifests.EditWith(ctx, ref, EditOptions{Existing: true, Lifecycle: true}, func(m *Manifest) error {
		switch {
		case m.Deleted != nil && m.Deleted.Erased:
			return ErrUnchanged
		case deleted && m.Deleted == nil:
			m.Deleted = &Deletion{At: j.cfg.Now().UTC()}
		case !deleted && m.Deleted != nil:
			m.Deleted = nil
		default:
			return ErrUnchanged
		}
		return nil
	})
	if errors.Is(err, ErrNotFound) {
		err = nil // no media: only the fence matters
	}
	if err != nil {
		return err
	}
	if !deleted {
		return j.unfence(ctx, item)
	}
	return nil
}

// Erased reports whether ref's media was erased: its tombstone or its
// permanent fence.
func (j *Jobs) Erased(ctx context.Context, ref contentref.ContentRef) (bool, error) {
	item, err := j.reg.Item(ref)
	if err != nil {
		return false, err
	}
	cur, _, err := j.manifests.GetAny(ctx, ref)
	if err == nil && cur.Deleted != nil && cur.Deleted.Erased {
		return true, nil
	} else if err != nil && !errors.Is(err, ErrNotFound) {
		return false, err
	}
	_, kind, err := j.fence(ctx, item)
	return kind == layout.FenceErased, err
}

// Tombstone makes ref's erasure irreversible without deleting bytes yet:
// the permanent fence, then an empty manifest whose files are all retired
// and due. Restore refuses from here on; Erase deletes the bytes.
func (j *Jobs) Tombstone(ctx context.Context, ref contentref.ContentRef) error {
	item, err := j.reg.Item(ref)
	if err != nil {
		return err
	}
	if err := j.fenceErasure(ctx, item); err != nil {
		return err
	}
	_, err = j.manifests.EditWith(ctx, ref, EditOptions{Lifecycle: true, Grace: -1}, func(m *Manifest) error {
		if m.Deleted != nil && m.Deleted.Erased {
			return ErrUnchanged
		}
		now := j.cfg.Now().UTC()
		m.Deleted = &Deletion{At: now, Erased: true}
		m.Files, m.Meta = nil, nil
		for i := range m.Retired {
			if m.Retired[i].Due.After(now) {
				m.Retired[i].Due = now // erasure deletes every file at once
			}
		}
		return nil
	})
	return err
}

// Erase tombstones ref and deletes every object it holds but the manifest
// and the fence, then verifies the folder. Objects that land later (a
// worker's late write) are deleted by the item's next collection.
func (j *Jobs) Erase(ctx context.Context, ref contentref.ContentRef) error {
	if err := j.Tombstone(ctx, ref); err != nil {
		return err
	}
	item, err := j.reg.Item(ref)
	if err != nil {
		return err
	}
	if _, err := j.Collect(ctx, ref); err != nil {
		return err
	}
	for obj, err := range j.store.List(ctx, item.Prefix()) {
		if err != nil {
			return err
		}
		if obj.Key == item.ManifestKey() || obj.Key == item.PublicPrefix()+layout.FenceName {
			continue
		}
		return fmt.Errorf("%w: %s", ErrErasurePending, obj.Key)
	}
	return nil
}

// orphanAge protects objects a writer may still be about to reference. The
// orphan pass is due an hour after that past an item's last write, and runs
// in every collection for a further day, so a late job still makes it.
const (
	orphanAge    = 24 * time.Hour
	orphanWindow = orphanAge + 25*time.Hour
)

// Collect runs ref's garbage collection once and returns when it is next
// due (zero: nothing left). Retired entries past their due time are marked
// deleting in the manifest (the fence against re-reference), deleted, and
// dropped once their DELETE has settled. After a write within the last day
// it also lists the folder for orphans: objects the manifest does not know,
// older than a day, which a crashed writer left. An erased item's unknown
// objects are all orphans. Nothing is deleted without the manifest's say.
func (j *Jobs) Collect(ctx context.Context, ref contentref.ContentRef) (time.Time, error) {
	item, err := j.reg.Item(ref)
	if err != nil {
		return time.Time{}, err
	}
	cur, _, err := j.manifests.GetAny(ctx, ref)
	if errors.Is(err, ErrNotFound) {
		return time.Time{}, nil
	} else if err != nil {
		return time.Time{}, err
	}
	now := j.cfg.Now().UTC()
	erased := cur.Deleted != nil && cur.Deleted.Erased
	var tmp []string
	if erased || now.Sub(cur.WrittenAt) < orphanWindow {
		orphans, staged, err := j.orphans(ctx, item, cur, now, erased)
		if err != nil {
			return time.Time{}, err
		}
		tmp = staged
		if len(orphans) > 0 {
			if cur, err = j.manifests.EditWith(ctx, ref, EditOptions{Lifecycle: true}, func(m *Manifest) error {
				live := m.referenced()
				changed := false
				for _, blob := range orphans {
					if _, retired := m.Retiring(blob); !live[blob] && !retired {
						m.Retired = append(m.Retired, Retired{Blob: blob, At: now, Due: now})
						changed = true
					}
				}
				if !changed {
					return ErrUnchanged
				}
				return nil
			}); err != nil {
				return time.Time{}, err
			}
		}
	}
	if slices.ContainsFunc(cur.Retired, func(r Retired) bool { return r.Deleting == nil && !r.Due.After(now) }) {
		if cur, err = j.manifests.EditWith(ctx, ref, EditOptions{Lifecycle: true}, func(m *Manifest) error {
			changed := false
			for i := range m.Retired {
				if r := &m.Retired[i]; r.Deleting == nil && !r.Due.After(now) {
					r.Deleting = &now
					changed = true
				}
			}
			if !changed {
				return ErrUnchanged
			}
			return nil
		}); err != nil {
			return time.Time{}, err
		}
	}
	deleted := map[string]bool{}
	var errs []error
	for _, r := range cur.Retired {
		if r.Deleting == nil {
			continue
		}
		if err := j.delete(ctx, keyOf(item, r.Blob)); err != nil {
			errs = append(errs, err)
			continue
		}
		deleted[r.Blob] = true
	}
	for _, key := range tmp {
		if err := j.delete(ctx, key); err != nil {
			errs = append(errs, err)
		}
	}
	settled := func(r Retired) bool {
		return r.Deleting != nil && deleted[r.Blob] && !r.Deleting.Add(j.manifests.settle).After(now)
	}
	if slices.ContainsFunc(cur.Retired, settled) {
		if cur, err = j.manifests.EditWith(ctx, ref, EditOptions{Lifecycle: true}, func(m *Manifest) error {
			n := len(m.Retired)
			m.Retired = slices.DeleteFunc(m.Retired, settled)
			if len(m.Retired) == n {
				return ErrUnchanged
			}
			return nil
		}); err != nil {
			return time.Time{}, err
		}
	}
	if err := errors.Join(errs...); err != nil {
		return time.Time{}, err
	}
	return nextCollection(cur, j.manifests.settle, now), nil
}

// NextCollection is when m next needs Collect.
func (j *Jobs) NextCollection(m *Manifest) time.Time {
	return nextCollection(m, j.manifests.settle, j.cfg.Now().UTC())
}

// orphans lists item's folder for objects m does not know, and for an
// erased item its staged uploads too.
func (j *Jobs) orphans(ctx context.Context, item Item, m *Manifest, now time.Time, erased bool) (blobs, staged []string, err error) {
	known := m.referenced()
	for _, r := range m.Retired {
		known[r.Blob] = true
	}
	for obj, err := range j.store.List(ctx, item.Prefix()) {
		if err != nil {
			return nil, nil, err
		}
		k, ok := layout.Parse(obj.Key)
		if !ok || k.Folder() != item.Prefix() {
			continue
		}
		switch k.Area {
		case layout.AreaInternal, layout.AreaPrivate, layout.AreaPublic:
			blob := k.Area + "/" + k.Name
			if !layout.ValidBlobRef(blob) {
				continue // the gateway fence
			}
			if !known[blob] && (erased || now.Sub(obj.LastModified) > orphanAge) {
				blobs = append(blobs, blob)
			}
		case layout.AreaTmp:
			if erased {
				staged = append(staged, obj.Key)
			}
		}
	}
	return blobs, staged, nil
}

// delete sends one DELETE; an absent object is deleted.
func (j *Jobs) delete(ctx context.Context, key string) error {
	if err := j.store.Delete(ctx, key); err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	return nil
}

// errFenceConflict: the fence kept changing while erasure replaced it.
var errFenceConflict = errors.New("media: gateway fence kept changing")

// fence reads item's gateway fence: "" when it has none, else its kind. A
// fence without the deletion mark is permanent erasure.
func (j *Jobs) fence(ctx context.Context, item Item) (media.Object, string, error) {
	obj, err := j.store.Head(ctx, item.PublicPrefix()+layout.FenceName)
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
	if err := RequireWritable(ctx, j.store); err != nil {
		return err
	}
	sum := sha256.Sum256(nil)
	o := media.PutOptions{IfMatch: match, ContentType: "application/octet-stream", CacheControl: "no-store",
		ChecksumSHA256: sum[:], Metadata: map[string]string{layout.FenceMeta: kind}}
	if match == "" {
		o.IfNoneMatch = "*"
	}
	_, err := j.store.Put(ctx, item.PublicPrefix()+layout.FenceName, bytes.NewReader(nil), 0, o)
	return err
}

// fenceDeleted closes item to the gateway unless it is already fenced.
func (j *Jobs) fenceDeleted(ctx context.Context, item Item) error {
	_, kind, err := j.fence(ctx, item)
	if err != nil || kind != "" {
		return err
	}
	if err := j.putFence(ctx, item, layout.FenceDeleted, ""); !errors.Is(err, ErrPreconditionFailed) {
		return err
	}
	return nil
}

// unfence removes a deletion fence, never an erasure's: an erasure that
// replaced the fence between the check and the DELETE is fenced again.
func (j *Jobs) unfence(ctx context.Context, item Item) error {
	_, kind, err := j.fence(ctx, item)
	if err != nil || kind != layout.FenceDeleted {
		return err
	}
	if err := j.store.Delete(ctx, item.PublicPrefix()+layout.FenceName); err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	cur, _, err := j.manifests.GetAny(ctx, item.Ref())
	if err == nil && cur.Deleted != nil && cur.Deleted.Erased {
		return j.fenceErasure(ctx, item)
	}
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// fenceErasure makes item's fence permanent, replacing a deletion fence.
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

// Items lists the items of kind k that have a manifest, in key order: one
// delimited listing of the kind's folders and one GET per item, never a
// listing of the items' files.
func (j *Jobs) Items(ctx context.Context, k *RegisteredKind) iter.Seq2[ItemManifest, error] {
	return func(yield func(ItemManifest, error) bool) {
		for folder, err := range j.store.Folders(ctx, k.root()+k.Name+"/") {
			if err != nil {
				yield(ItemManifest{}, err)
				return
			}
			key, ok := layout.ParseFolder(folder)
			if !ok || key.ID == layout.DefaultID {
				continue
			}
			m, etag, err := j.manifests.get(ctx, folder+layout.ManifestName)
			if errors.Is(err, ErrNotFound) {
				continue // files whose manifest never landed: no item
			}
			var out ItemManifest
			if err == nil {
				out = ItemManifest{Manifest: m, ETag: etag}
				out.Ref, err = j.reg.RefOf(m.Item)
			}
			if err != nil {
				err = fmt.Errorf("media: %s: %w", folder, err)
			}
			if !yield(out, err) {
				return
			}
		}
	}
}

// ItemManifest is an item's stored manifest.
type ItemManifest struct {
	Ref      contentref.ContentRef
	Manifest *Manifest
	ETag     string
}

// Rebuild records every item of kinds in the cache from the bucket: what a
// database restored from a backup, or dropped, needs. Each recorded item
// wakes its processing, release and collection again. Rows already at a
// manifest's revision are left alone. It returns the items recorded and
// stops at the first error.
func (j *Jobs) Rebuild(ctx context.Context, cache *PGCache, kinds ...string) (int, error) {
	n := 0
	for _, name := range kinds {
		k, err := j.reg.Kind(name)
		if err != nil {
			return n, err
		}
		for it, err := range j.Items(ctx, k) {
			if err != nil {
				return n, err
			}
			item, err := j.reg.Item(it.Ref)
			if err != nil {
				return n, err
			}
			if err := cache.Record(ctx, item, it.Manifest, it.ETag); err != nil {
				return n, err
			}
			n++
		}
	}
	return n, nil
}

// Regenerate visits every item of kind and asks the worker to redo its
// stale outputs of preset ("" for all): after a deploy changed a preset's
// spec or a producer's recipe. It returns the items visited.
func (j *Jobs) Regenerate(ctx context.Context, kind, preset string) (int, error) {
	k, err := j.reg.Kind(kind)
	if err != nil {
		return 0, err
	}
	if j.cfg.Processes == nil {
		return 0, errors.New("media: Regenerate needs JobsConfig.Processes")
	}
	n := 0
	for it, err := range j.Items(ctx, k) {
		if err != nil {
			return n, err
		}
		if err := j.cfg.Processes.Enqueue(ctx, ProcessJob{Ref: it.Ref, Preset: preset, Class: media.VideoBackfill}); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
