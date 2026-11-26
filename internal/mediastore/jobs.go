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
	"sync"
	"sync/atomic"
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
// kept. An erased item stays erased: a stale sync changes nothing. The fence
// then follows the manifest as written (syncFence), so of a deletion and a
// restore racing, the fence is the one the later manifest write says.
func (j *Jobs) SetDeleted(ctx context.Context, ref contentref.ContentRef, deleted bool) error {
	item, err := j.reg.Item(ref)
	if err != nil {
		return err
	}
	none := ""
	if deleted {
		none = layout.FenceDeleted
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
	return j.syncFence(ctx, item, none)
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
	if err != nil {
		return err
	}
	return j.syncFence(ctx, item, layout.FenceErased)
}

// Erase tombstones ref and deletes every object under its folder but the
// manifest and the fence, keys outside the layout included, then verifies
// the folder. Objects that land later (a worker's late write) are deleted by
// the item's next collection.
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

// OrphanAge protects objects a writer may still be about to reference: an
// object no manifest names is collected once it is this old. Every
// collection lists the item's folder; one that finds younger unknown
// objects is due again once they are old enough, and a write is followed by
// a collection a day later.
const OrphanAge = 24 * time.Hour

const (
	// gcBatch bounds the entries one conditional write stamps deleting.
	gcBatch = 1000
	// gcParallel bounds GC's requests in flight.
	gcParallel = 8
	// stampBytes is what a deleting stamp adds to a retired entry's JSON.
	stampBytes = len(`,"deleting":"2006-01-02T15:04:05.000Z"`)
	// retiredBytes is a retired entry's JSON without its path, stamp
	// included.
	retiredBytes = 200
)

// Collect runs ref's garbage collection once and returns when it is next
// due (zero: nothing left). It lists the item's folder and retires orphans:
// objects the manifest does not know, a day old (an erased item's at once,
// and the rest of its folder, tmp/ and keys outside the layout, outright).
// Due retired entries are stamped deleting in batches, each in its own
// conditional write, and a run deletes only what its own writes stamped,
// while their fence holds (the first half of the settle period). A stamp
// past its settle period is checked with a HEAD: a gone object's entry is
// dropped, a present one stamped again. Nothing is deleted without the
// manifest's say.
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
	erased := cur.Deleted != nil && cur.Deleted.Erased
	orphans, loose, young, err := j.orphans(ctx, item, cur, erased)
	if err != nil {
		return time.Time{}, err
	}
	if len(orphans) > 0 {
		if cur, err = j.manifests.EditWith(ctx, ref, EditOptions{Lifecycle: true}, func(m *Manifest) error {
			now := j.cfg.Now().UTC().Truncate(time.Millisecond)
			known := m.referenced()
			for _, r := range m.Retired {
				known[r.Blob] = true
			}
			room := (maxStoredBytes - m.size) / int64(retiredBytes+64)
			changed := false
			for _, blob := range orphans {
				if room <= 0 {
					break // the rest next run, once these have gone
				}
				if !known[blob] {
					m.Retired = append(m.Retired, Retired{Blob: blob, At: now, Due: now})
					changed = true
					room--
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
	errs := j.deleteAll(ctx, loose, time.Time{})
	cur, err = j.drain(ctx, item, cur)
	if err != nil {
		return time.Time{}, errors.Join(append(errs, err)...)
	}
	if err := errors.Join(errs...); err != nil {
		return time.Time{}, err
	}
	next := nextCollection(cur, j.manifests.settle, j.cfg.Now().UTC())
	if !young.IsZero() {
		if at := young.Add(OrphanAge + time.Minute); next.IsZero() || at.Before(next) {
			next = at
		}
	}
	return next, nil
}

// drain stamps, deletes and drops cur's retired entries until nothing is
// due. It returns the manifest as last read or written.
func (j *Jobs) drain(ctx context.Context, item Item, cur *Manifest) (*Manifest, error) {
	settle := j.manifests.settle
	unsent := map[string]time.Time{} // this run's stamps it never sent
	sent := map[string]bool{}        // an erased item's entries this run deleted
	var errs []error
	var last time.Time
	for round := 0; round < 256 && ctx.Err() == nil; round++ {
		now := j.cfg.Now().UTC()
		var settled []Retired
		for _, r := range cur.Retired {
			if r.Deleting != nil && !r.Deleting.Add(settle).After(now) {
				settled = append(settled, r)
			}
		}
		gone, present, err := j.heads(ctx, item, settled)
		if err != nil {
			errs = append(errs, err)
		}
		wasErased := cur.Deleted != nil && cur.Deleted.Erased
		due := slices.ContainsFunc(cur.Retired, func(r Retired) bool {
			return r.Deleting == nil && !r.Due.After(now) || r.Deleting != nil && (unsent[r.Blob].Equal(*r.Deleting) || wasErased && !sent[r.Blob])
		})
		if len(gone) == 0 && len(present) == 0 && !due {
			break
		}
		stamp := now.Truncate(time.Millisecond)
		if !stamp.After(last) {
			stamp = last.Add(time.Millisecond) // each round's stamp is its own
		}
		last = stamp
		matches := func(set map[string]time.Time, r Retired) bool {
			at, ok := set[r.Blob]
			return ok && r.Deleting != nil && r.Deleting.Equal(at)
		}
		out, err := j.manifests.EditItem(ctx, item, EditOptions{Lifecycle: true}, func(m *Manifest) error {
			n := len(m.Retired)
			m.Retired = slices.DeleteFunc(m.Retired, func(r Retired) bool { return matches(gone, r) })
			changed := len(m.Retired) != n
			room := min(gcBatch, (maxStoredBytes-m.size)/int64(stampBytes))
			for i := range m.Retired {
				r := &m.Retired[i]
				if room <= 0 {
					break
				}
				if r.Deleting == nil && !r.Due.After(now) || matches(present, *r) || matches(unsent, *r) {
					r.Deleting = &stamp
					changed = true
					room--
				}
			}
			if !changed {
				return ErrUnchanged
			}
			return nil
		})
		if err != nil {
			return cur, errors.Join(append(errs, err)...)
		}
		cur = out
		// No manifest can name an erased item's hashes again: its entries are
		// deleted whoever stamped them, with no fence to respect.
		erased := cur.Deleted != nil && cur.Deleted.Erased
		lastSend := stamp.Add(settle / 2)
		if erased {
			lastSend = time.Time{}
		}
		var ours []string
		for _, r := range cur.Retired {
			if r.Deleting != nil && (r.Deleting.Equal(stamp) || erased && !sent[r.Blob]) {
				ours = append(ours, r.Blob)
			}
		}
		keys := make([]string, len(ours))
		for i, blob := range ours {
			keys[i] = keyOf(item, blob)
		}
		done, delErrs := j.send(ctx, keys, lastSend)
		errs = append(errs, delErrs...)
		clear(unsent)
		for i, blob := range ours {
			if !done[i] {
				unsent[blob] = stamp
			} else if erased {
				sent[blob] = true
			}
		}
		if len(gone) == 0 && len(unsent) == len(ours) {
			break // no progress: the fence ran out before a DELETE was sent
		}
	}
	return cur, errors.Join(errs...)
}

// heads checks the objects of entries past their settle period: gone ones
// may be dropped, present ones need deleting again. Each maps a blob to the
// stamp it was checked under.
func (j *Jobs) heads(ctx context.Context, item Item, entries []Retired) (gone, present map[string]time.Time, err error) {
	gone, present = map[string]time.Time{}, map[string]time.Time{}
	found := make([]error, len(entries))
	var wg sync.WaitGroup
	for w := 0; w < min(gcParallel, len(entries)); w++ {
		wg.Go(func() {
			for i := w; i < len(entries); i += gcParallel {
				_, found[i] = j.store.Head(ctx, keyOf(item, entries[i].Blob))
			}
		})
	}
	wg.Wait()
	var errs []error
	for i, r := range entries {
		switch err := found[i]; {
		case errors.Is(err, ErrNotFound):
			gone[r.Blob] = *r.Deleting
		case err == nil:
			present[r.Blob] = *r.Deleting
		default:
			errs = append(errs, err)
		}
	}
	return gone, present, errors.Join(errs...)
}

// send DELETEs keys, gcParallel at a time, none at or after lastSend (zero:
// no bound). It reports which it sent.
func (j *Jobs) send(ctx context.Context, keys []string, lastSend time.Time) ([]bool, []error) {
	sent := make([]bool, len(keys))
	errs := make([]error, len(keys))
	var next atomic.Int64
	var wg sync.WaitGroup
	for range min(gcParallel, len(keys)) {
		wg.Go(func() {
			for {
				i := int(next.Add(1) - 1)
				if i >= len(keys) || ctx.Err() != nil || !lastSend.IsZero() && !j.cfg.Now().Before(lastSend) {
					return
				}
				sent[i] = true
				errs[i] = j.delete(ctx, keys[i])
			}
		})
	}
	wg.Wait()
	return sent, slices.DeleteFunc(errs, func(err error) bool { return err == nil })
}

// deleteAll DELETEs keys no manifest fences (an erased item's loose keys).
func (j *Jobs) deleteAll(ctx context.Context, keys []string, lastSend time.Time) []error {
	_, errs := j.send(ctx, keys, lastSend)
	return errs
}

// NextCollection is when m next needs Collect.
func (j *Jobs) NextCollection(m *Manifest) time.Time {
	return nextCollection(m, j.manifests.settle, j.cfg.Now().UTC())
}

// orphans lists item's folder for blobs m does not know, a day old, and for
// an erased item every unknown blob and every other key but the manifest
// and the fence: tmp/ and anything outside the layout. A live item's tmp/
// expires by the bucket's lifecycle. young is the oldest unknown blob too
// young to collect.
func (j *Jobs) orphans(ctx context.Context, item Item, m *Manifest, erased bool) (blobs, loose []string, young time.Time, err error) {
	known := m.referenced()
	for _, r := range m.Retired {
		known[r.Blob] = true
	}
	now := j.cfg.Now().UTC()
	for obj, err := range j.store.List(ctx, item.Prefix()) {
		if err != nil {
			return nil, nil, time.Time{}, err
		}
		k, ok := layout.Parse(obj.Key)
		switch {
		case !ok || k.Folder() != item.Prefix() || k.Area == layout.AreaTmp:
			if erased {
				loose = append(loose, obj.Key)
			}
		case k.Area == layout.AreaInternal || k.Area == layout.AreaPrivate || k.Area == layout.AreaPublic:
			blob := k.Area + "/" + k.Name
			switch {
			case !layout.ValidBlobRef(blob), known[blob]:
				// the gateway fence, or named
			case erased || now.Sub(obj.LastModified) > OrphanAge:
				blobs = append(blobs, blob)
			case young.IsZero() || obj.LastModified.Before(young):
				young = obj.LastModified
			}
		}
	}
	return blobs, loose, young, nil
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

// syncFence makes item's fence what its manifest says: permanent once
// erased, the deletion fence while deleted, none otherwise (none is the
// fence of an item without a manifest). An erasure's fence is never
// removed. Each round re-reads the manifest after acting, so racing syncs
// settle on the fence of the last manifest write.
func (j *Jobs) syncFence(ctx context.Context, item Item, none string) error {
	for range 8 {
		want := none
		m, _, err := j.manifests.GetAny(ctx, item.Ref())
		switch {
		case errors.Is(err, ErrNotFound):
		case err != nil:
			return err
		case m.Deleted == nil:
			want = ""
		case m.Deleted.Erased:
			want = layout.FenceErased
		default:
			want = layout.FenceDeleted
		}
		_, kind, err := j.fence(ctx, item)
		switch {
		case err != nil:
			return err
		case kind == want || kind == layout.FenceErased:
			return nil
		case want == layout.FenceErased:
			err = j.fenceErasure(ctx, item)
		case want == layout.FenceDeleted:
			err = j.putFence(ctx, item, layout.FenceDeleted, "")
		default:
			err = j.store.Delete(ctx, item.PublicPrefix()+layout.FenceName)
		}
		if err != nil && !errors.Is(err, ErrPreconditionFailed) && !errors.Is(err, ErrNotFound) {
			return err
		}
	}
	return errFenceConflict
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

// Regenerate visits the items r selects and asks the worker to bring each
// up to date: masters whose recipe changed, then renditions whose master or
// spec changed (after a deploy changed a recipe, a preset's spec or a
// producer's recipe). Items already current write nothing, so it is
// idempotent, and a run cut short is resumed by running it again. Each
// write publishes a new manifest revision and retires what it replaced. It
// returns the items visited.
func (j *Jobs) Regenerate(ctx context.Context, r media.Regeneration) (int, error) {
	k, err := j.reg.Kind(r.Kind)
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
		if len(r.IDs) > 0 && !slices.Contains(r.IDs, it.Ref.ContentID) && !slices.Contains(r.IDs, it.Ref.Version()) ||
			r.Filter != nil && !r.Filter(it.Ref) {
			continue
		}
		if err := j.cfg.Processes.Enqueue(ctx, ProcessJob{Ref: it.Ref, Class: media.VideoBackfill}); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
