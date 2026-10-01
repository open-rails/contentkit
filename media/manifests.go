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
	"strconv"
	"sync"
	"time"

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
	// by ETag; default and least 64 MiB, which holds the largest manifest
	// (MaxManifestBytes) twice over.
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
	o.CacheBytes = max(o.CacheBytes, 64<<20)
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

// Edit limits below MaxManifestBytes: commits and the workers' records stop
// at editLimit; failures recorded for what did not fit may use the headroom
// but hideBytes, which a hide's flag needs; a hide may use it all.
const (
	hideBytes = 64
	editLimit = MaxManifestBytes - editHeadroom
	failLimit = MaxManifestBytes - hideBytes
)

// bound refuses an edit (ErrManifestTooLarge) that grows a manifest past
// limit (default editLimit), counting what project adds; an edit that does
// not grow it passes, so a full item still shrinks. Nothing over
// MaxManifestBytes is ever written.
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
		return fmt.Errorf("%w: %d bytes (%d when processed), at most %d", ErrManifestTooLarge, next.size, grown, limit)
	}
	return nil
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
	if etag != "" && next.Hidden == cur.Hidden && reflect.DeepEqual(next.Meta, cur.Meta) && reflect.DeepEqual(next.Files, cur.Files) {
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

// FailUploads records cause on each of uploads that still holds the blob and
// edit given. It may use the headroom the other edits leave, so a worker can
// always fail what the manifest could not hold (ErrManifestTooLarge, a
// too_large failure).
func (m *Manifests) FailUploads(ctx context.Context, ref contentref.ContentRef, uploads []File, cause error) error {
	_, err := m.edit(ctx, ref, true, bound{limit: failLimit}, func(cur *Manifest) error {
		for _, f := range uploads {
			if c, ok := cur.Get(f.Path); ok && c.IsUpload() && c.Key() == f.Key() {
				cur.SetFailed(f.Path, cause)
			}
		}
		return nil
	})
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
