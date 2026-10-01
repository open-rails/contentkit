package media

import (
	"bytes"
	"cmp"
	"container/list"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"reflect"
	"slices"
	"strconv"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/open-rails/contentkit/contentref"
)

// Locker serializes manifest edits across every process sharing the bucket.
type Locker interface {
	Lock(ctx context.Context, key string) (unlock func(), err error)
}

// SweepScheduler schedules a folder's sweep after an edit: the host's
// *Jobs, or in the media worker a *HostQueue.
type SweepScheduler interface {
	ScheduleSweep(ctx context.Context, ref contentref.ContentRef) error
}

// ManifestOptions configure Manifests.
type ManifestOptions struct {
	// Locker is required: every edit runs under it, and also writes with
	// If-Match once the store reports ConditionalPut. See PGLocker.
	Locker Locker
	// CacheBytes bounds the decoded manifests kept in process, revalidated
	// by ETag. A manifest costs about three times its JSON, so the largest
	// (MaxManifestBytes) costs 24 MiB; default 128 MiB, at least two of them.
	CacheBytes int64
	MaxRetries int // conditional-write attempts per edit; default 16
	// Sweeps schedules the folder's sweep after every written edit; best
	// effort (the periodic pass backs it up).
	Sweeps SweepScheduler
}

// Manifests reads and edits item manifests.
type Manifests struct {
	store   Store
	reg     *Registry
	locker  Locker
	sweeps  SweepScheduler
	retries int
	cache   *manifestCache
}

var ErrManifestConflict = errors.New("media: manifest edit kept conflicting")

func NewManifests(store Store, reg *Registry, o ManifestOptions) (*Manifests, error) {
	if store == nil || reg == nil || o.Locker == nil {
		return nil, errors.New("media: Manifests needs a Store, a Registry and a Locker")
	}
	if o.CacheBytes <= 0 {
		o.CacheBytes = 128 << 20
	}
	o.CacheBytes = max(o.CacheBytes, 2*MaxManifestBytes*decodeFactor)
	if o.MaxRetries <= 0 {
		o.MaxRetries = 16
	}
	return &Manifests{store: store, reg: reg, locker: o.Locker, sweeps: o.Sweeps, retries: o.MaxRetries,
		cache: newManifestCache(o.CacheBytes)}, nil
}

// Registry is the registry the manifests are read under.
func (m *Manifests) Registry() *Registry { return m.reg }

// Store is the bucket.
func (m *Manifests) Store() Store { return m.store }

// Get returns ref's manifest and ETag, or ErrNotFound. Cached copies are
// revalidated with a conditional GET, so a read is never stale. The
// manifest is shared: never modify it (Clone it).
func (m *Manifests) Get(ctx context.Context, ref contentref.ContentRef) (*Manifest, string, error) {
	item, err := m.reg.Item(ref)
	if err != nil {
		return nil, "", err
	}
	return m.get(ctx, item.ManifestKey())
}

// Edit applies fn to ref's manifest (empty if none) and writes it with
// If-Match on the ETag it read (If-None-Match for a new one), re-reading
// and re-applying fn on conflict. fn must be safe to run more than once; an
// error from fn aborts the edit. The result is normalized (Kind.Normalize)
// and validated; an unchanged manifest is not written. A folder's first
// manifest is refused (ErrFolderNotEmpty) over a previous item's public
// files, and one growing to within editHeadroom of MaxManifestBytes with
// ErrManifestTooLarge.
func (m *Manifests) Edit(ctx context.Context, ref contentref.ContentRef, fn func(*Manifest) error) (*Manifest, error) {
	return m.edit(ctx, ref, false, bound{}, fn)
}

// EditExisting is Edit while the manifest exists (ErrNotFound otherwise):
// workers use it after processing, so a concurrent deletion stays deleted.
func (m *Manifests) EditExisting(ctx context.Context, ref contentref.ContentRef, fn func(*Manifest) error) (*Manifest, error) {
	return m.edit(ctx, ref, true, bound{}, fn)
}

// editLimit is where commits and the workers' records stop; an unhide, whose
// pending names commits projected, may run to flagBytes short of the bound;
// the flags (Hidden, Full and its Deficit) may use it all, so they always fit.
const (
	editLimit = MaxManifestBytes - editHeadroom
	flagBytes = 64
)

// bound refuses an edit (ErrManifestTooLarge) that grows a manifest past
// limit (default editLimit), counting what project adds; an edit that does
// not grow it passes, so a full item still shrinks. Nothing over
// MaxManifestBytes is ever written. A commit (project set) that shrinks a
// Full manifest clears Full: processing resumes.
type bound struct {
	limit   int64
	project func(*Manifest) int64
}

func (b bound) check(cur, next *Manifest) error {
	limit, grown, was := cmp.Or(b.limit, editLimit), next.size, cur.size
	if b.project != nil {
		grown, was = grown+b.project(next), was+b.project(cur)
	}
	if next.size > MaxManifestBytes || grown > limit && grown > was {
		return &tooLargeError{size: next.size, grown: grown, limit: limit}
	}
	return nil
}

// tooLargeError is ErrManifestTooLarge with how far past its limit the edit
// went, which SetFull records as the item's Deficit.
type tooLargeError struct{ size, grown, limit int64 }

func (e *tooLargeError) Error() string {
	return fmt.Sprintf("%v: %d bytes (%d when processed), at most %d", ErrManifestTooLarge, e.size, e.grown, e.limit)
}
func (e *tooLargeError) Is(target error) bool { return target == ErrManifestTooLarge }

// SyncPublic deletes public names no attached upload currently uses, under
// the manifest lock. Cleanup is bounded to one minute; deleted keys are
// returned for cache purging, including partial success on failure.
func (m *Manifests) SyncPublic(ctx context.Context, ref contentref.ContentRef) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	item, err := m.reg.Item(ref)
	if err != nil {
		return nil, err
	}
	unlock, err := m.locker.Lock(ctx, item.ManifestKey())
	if err != nil {
		return nil, err
	}
	defer unlock()
	cur, _, err := m.get(ctx, item.ManifestKey())
	if errors.Is(err, ErrNotFound) {
		cur = &Manifest{}
	} else if err != nil {
		return nil, err
	}
	want := map[string]bool{}
	if !cur.Hidden {
		for _, f := range cur.Files {
			if !f.IsUpload() || f.Unattached {
				continue
			}
			for _, p := range item.Kind().PublicFor(f.Path) {
				for _, name := range item.Kind().PublicNames(p, f.Path) {
					key, err := item.Public(name)
					if err != nil {
						return nil, err
					}
					want[key] = true
				}
			}
		}
	}
	var keys []string
	for obj, err := range m.store.List(ctx, item.PublicPrefix()) {
		if err != nil {
			return nil, err
		}
		if !want[obj.Key] {
			keys = append(keys, obj.Key)
		}
	}
	g, deleteCtx := errgroup.WithContext(ctx)
	g.SetLimit(8)
	var mu sync.Mutex
	var gone []string
	for _, key := range keys {
		if deleteCtx.Err() != nil {
			break
		}
		g.Go(func() error {
			if err := m.store.Delete(deleteCtx, key); err != nil && !errors.Is(err, ErrNotFound) {
				return err
			}
			mu.Lock()
			gone = append(gone, key)
			mu.Unlock()
			return nil
		})
	}
	err = errors.Join(g.Wait(), ctx.Err())
	return gone, err
}

// Unreferenced selects what DropUnreferenced deletes.
type Unreferenced struct {
	// All is every private blob in the folder, editor views included: a
	// worker whose unrecorded outputs go with them finds them missing when
	// it records, and produces them again.
	All bool
	// Blobs are candidates: each goes unless it is an editor view of a
	// current upload.
	Blobs []string
	// Staged are staged uploads' names.
	Staged []string
}

// DropUnreferenced deletes at once, under the manifest lock, what u selects
// and ref's manifest does not reference: a takedown's sweep, without the
// grace period.
func (m *Manifests) DropUnreferenced(ctx context.Context, ref contentref.ContentRef, u Unreferenced) error {
	item, err := m.reg.Item(ref)
	if err != nil {
		return err
	}
	unlock, err := m.locker.Lock(ctx, item.ManifestKey())
	if err != nil {
		return err
	}
	defer unlock()
	cur, _, err := m.get(ctx, item.ManifestKey())
	if errors.Is(err, ErrNotFound) {
		return nil // deleted: the folder deletion takes everything
	} else if err != nil {
		return err
	}
	var keys []string
	for _, s := range u.Staged {
		if key, err := item.Staged(s); err == nil && !slices.Contains(cur.StagedNames(), s) {
			keys = append(keys, key)
		}
	}
	if u.All {
		keep := map[string]bool{}
		for _, b := range cur.Blobs() {
			key, _ := item.Blob(b)
			keep[key] = true
		}
		for obj, err := range m.store.List(ctx, item.PrivatePrefix()) {
			if err != nil {
				return err
			}
			if !keep[obj.Key] {
				keys = append(keys, obj.Key)
			}
		}
	} else {
		keep := m.reg.editorViews(cur)
		for _, b := range cur.Blobs() {
			keep[b] = true
		}
		for _, b := range u.Blobs {
			if key, err := item.Blob(b); err == nil && !keep[b] {
				keys = append(keys, key)
			}
		}
	}
	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(8)
	for _, key := range keys {
		g.Go(func() error {
			if err := m.store.Delete(ctx, key); err != nil && !errors.Is(err, ErrNotFound) {
				return err
			}
			return nil
		})
	}
	return g.Wait()
}

// DeleteUnreferenced deletes the blobs among blobs that cur does not
// reference. A job calls it from its closing edit (the manifest lock held)
// for what it wrote for a source that is gone, so a taken-down upload's
// outputs do not come back under their old names.
func (m *Manifests) DeleteUnreferenced(ctx context.Context, item Item, cur *Manifest, blobs []string) error {
	if len(blobs) == 0 {
		return nil
	}
	keep := map[string]bool{}
	for _, b := range cur.Blobs() {
		keep[b] = true
	}
	var errs []error
	for _, b := range blobs {
		if key, err := item.Blob(b); err == nil && !keep[b] {
			if err := m.store.Delete(ctx, key); err != nil && !errors.Is(err, ErrNotFound) {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

func (m *Manifests) edit(ctx context.Context, ref contentref.ContentRef, existing bool, b bound, fn func(*Manifest) error) (*Manifest, error) {
	item, err := m.reg.Item(ref)
	if err != nil {
		return nil, err
	}
	key := item.ManifestKey()
	unlock, err := m.locker.Lock(ctx, key)
	if err != nil {
		return nil, err
	}
	defer unlock()
	conditional := m.store.Capabilities().ConditionalPut
	for attempt := 0; attempt < m.retries; attempt++ {
		out, written, conflict, err := m.try(ctx, item, existing, conditional, b, fn)
		if conflict {
			backoff := time.Duration(1<<min(attempt, 6)) * 5 * time.Millisecond
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff/2 + rand.N(backoff)):
			}
			continue
		}
		if err == nil && written && m.sweeps != nil {
			if serr := m.sweeps.ScheduleSweep(ctx, ref); serr != nil {
				slog.WarnContext(ctx, "media: schedule sweep", "key", key, "error", serr)
			}
		}
		return out, err
	}
	return nil, fmt.Errorf("%w: %s", ErrManifestConflict, key)
}

func (m *Manifests) try(ctx context.Context, item Item, existing, conditional bool, b bound, fn func(*Manifest) error) (out *Manifest, written, conflict bool, err error) {
	key := item.ManifestKey()
	cur, etag, err := m.get(ctx, key)
	switch {
	case errors.Is(err, ErrNotFound) && existing:
		return nil, false, false, err
	case errors.Is(err, ErrNotFound):
		cur, etag = &Manifest{V: ManifestVersion, Files: []File{}}, ""
	case err != nil:
		return nil, false, false, err
	}
	next := cur.Clone()
	if err := fn(next); err != nil {
		return nil, false, false, err
	}
	item.kind.Normalize(next)
	if err := next.Validate(); err != nil {
		return nil, false, false, err
	}
	if etag != "" && next.Hidden == cur.Hidden && next.Full == cur.Full && next.Deficit == cur.Deficit && reflect.DeepEqual(next.Meta, cur.Meta) && reflect.DeepEqual(next.Files, cur.Files) {
		return cur, false, false, nil
	}
	if etag == "" {
		if err := m.requireFresh(ctx, item); err != nil {
			return nil, false, false, err
		}
	}
	body, err := encodeManifest(next)
	if err != nil {
		return nil, false, false, err
	}
	if err := b.check(cur, next); err != nil {
		return nil, false, false, err
	}
	// A commit clears Full only when it really makes room: it frees, in the
	// manifest and in what its uploads will still add, the bytes the refused
	// record was short of. Less (a small shrink, one upload of many) would
	// only re-run the work to the same refusal.
	if cur.Full && b.project != nil && cur.size+b.project(cur)-next.size-b.project(next) >= max(cur.Deficit, 1) {
		next.Full, next.Deficit = false, 0
		if body, err = encodeManifest(next); err != nil {
			return nil, false, false, err
		}
	}
	// The charged quota rides on the object, so releasing it never needs
	// the manifest to decode.
	opts := PutOptions{ContentType: "application/gzip", CacheControl: "no-store",
		Metadata: map[string]string{uploadBytesMeta: strconv.FormatInt(next.uploadBytes(), 10)}}
	if conditional {
		if etag == "" {
			opts.IfNoneMatch = "*"
		} else {
			opts.IfMatch = etag
		}
	}
	obj, err := m.store.Put(ctx, key, bytes.NewReader(body), int64(len(body)), opts)
	if errors.Is(err, ErrPreconditionFailed) {
		m.cache.remove(key)
		return nil, false, true, nil
	}
	if err != nil {
		return nil, false, false, err
	}
	next.reindex()
	m.cache.put(key, obj.ETag, next)
	return next, true, false, nil
}

// get reads and decodes a manifest through the ETag-revalidated cache.
func (m *Manifests) get(ctx context.Context, key string) (*Manifest, string, error) {
	cachedETag, cached := m.cache.get(key)
	rc, obj, err := m.store.Get(ctx, key, GetOptions{IfNoneMatch: cachedETag})
	switch {
	case errors.Is(err, ErrNotModified) && cached != nil:
		return cached, cachedETag, nil
	case errors.Is(err, ErrNotFound):
		m.cache.remove(key)
		return nil, "", err
	case err != nil:
		return nil, "", err
	}
	body, err := io.ReadAll(io.LimitReader(rc, MaxManifestBytes+1))
	rc.Close()
	if err != nil {
		return nil, "", err
	}
	man, err := decodeManifest(body)
	if err != nil {
		return nil, "", fmt.Errorf("media: decode manifest %s: %w", key, err)
	}
	m.cache.put(key, obj.ETag, man)
	return man, obj.ETag, nil
}

// SetFull marks ref's manifest Full: a producer could not record its
// outputs (cause, an ErrManifestTooLarge, whose overrun becomes the
// Deficit). The flags fit in the headroom every other edit leaves.
// Producers write no private output for a Full item until a commit frees
// the deficit.
func (m *Manifests) SetFull(ctx context.Context, ref contentref.ContentRef, cause error) error {
	deficit := int64(1)
	if tl := (*tooLargeError)(nil); errors.As(cause, &tl) {
		deficit = max(tl.grown-tl.limit, 1)
	}
	_, err := m.edit(ctx, ref, true, bound{limit: MaxManifestBytes}, func(cur *Manifest) error {
		cur.Full, cur.Deficit = true, max(cur.Deficit, deficit)
		return nil
	})
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// DropIfDeleted deletes blobs a worker wrote for ref only if its manifest is
// gone: the folder lock orders the check and the deletes with edits and
// folder deletion.
func (m *Manifests) DropIfDeleted(ctx context.Context, ref contentref.ContentRef, blobs []string) error {
	if len(blobs) == 0 {
		return nil
	}
	item, err := m.reg.Item(ref)
	if err != nil {
		return err
	}
	unlock, err := m.locker.Lock(ctx, item.ManifestKey())
	if err != nil {
		return err
	}
	defer unlock()
	if _, _, err := m.get(ctx, item.ManifestKey()); err == nil {
		return nil
	} else if !errors.Is(err, ErrNotFound) {
		return err
	}
	var errs []error
	for _, b := range blobs {
		key, err := item.Blob(b)
		if err != nil {
			return err
		}
		if err := m.store.Delete(ctx, key); err != nil && !errors.Is(err, ErrNotFound) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// manifestCache keeps decoded manifests by key, bounded by their JSON size
// times a decode factor.
type manifestCache struct {
	mu    sync.Mutex
	max   int64
	used  int64
	order *list.List
	items map[string]*list.Element
}

type cacheEntry struct {
	key, etag string
	m         *Manifest
	cost      int64
}

// decodeFactor estimates a decoded manifest's memory from its JSON size.
const decodeFactor = 3

func newManifestCache(max int64) *manifestCache {
	return &manifestCache{max: max, order: list.New(), items: map[string]*list.Element{}}
}

func (c *manifestCache) get(key string) (string, *Manifest) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[key]; ok {
		c.order.MoveToFront(e)
		v := e.Value.(*cacheEntry)
		return v.etag, v.m
	}
	return "", nil
}

func (c *manifestCache) put(key, etag string, m *Manifest) {
	if etag == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.removeLocked(key)
	e := &cacheEntry{key: key, etag: etag, m: m, cost: m.size * decodeFactor}
	if e.cost > c.max {
		return
	}
	c.items[key] = c.order.PushFront(e)
	c.used += e.cost
	for c.used > c.max {
		c.removeLocked(c.order.Back().Value.(*cacheEntry).key)
	}
}

func (c *manifestCache) remove(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.removeLocked(key)
}

func (c *manifestCache) removeLocked(key string) {
	if e, ok := c.items[key]; ok {
		c.used -= e.Value.(*cacheEntry).cost
		c.order.Remove(e)
		delete(c.items, key)
	}
}
