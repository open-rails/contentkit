package mediastore

import (
	"bytes"
	"container/list"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"math/rand/v2"
	"reflect"
	"slices"
	"sync"
	"time"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/uuidv7"
	media "github.com/open-rails/contentkit/media"
)

// ManifestOptions configure Manifests.
type ManifestOptions struct {
	// CacheBytes bounds the decoded manifests kept in process, revalidated
	// by ETag. A manifest costs about three times its JSON; default 128 MiB.
	CacheBytes int64
	// MaxRetries bounds an edit's re-reads after a 412 or an ambiguous
	// answer; default 16.
	MaxRetries int
	// Grace is how long a file an edit stops referencing stays retired
	// before GC may delete it: the undo window. Default 7 days.
	Grace time.Duration
	// Settle bounds how late a DELETE may still execute after GC sent it
	// (each DELETE is one attempt; a proxy may still be running one whose
	// answer timed out): a hash being deleted is not referenced again until
	// it has passed. Default 2 minutes.
	Settle time.Duration
	// Written runs after every write that lands, and after an edit finds its
	// own write landed or nothing to change, with the stored manifest: the
	// catalog cache moves forward by Rev there. An error fails the edit;
	// repeating the edit repeats the call.
	Written func(ctx context.Context, item Item, m *Manifest, etag string) error
	// Current is the ETag the item cache last recorded for item ("" for
	// none). Get serves a decoded copy of that revision from memory without
	// asking the bucket; writes always read the bucket.
	Current func(ctx context.Context, item Item) (string, error)
	Now     func() time.Time
}

// Manifests reads and edits item manifests. The bucket is the only record
// of an item's files: an edit reads the manifest with its ETag, applies its
// change, and writes it back with If-Match (If-None-Match: * to create). A
// 412 or an ambiguous answer re-reads and re-applies, so a change must be a
// re-appliable delta: a function of the manifest it is given that checks the
// state it changes. Safety needs a bucket whose conditional PUT compares
// atomically with the write: one that never had versioning (Probe).
type Manifests struct {
	store   media.Store
	reg     *Registry
	retries int
	grace   time.Duration
	settle  time.Duration
	written func(ctx context.Context, item Item, m *Manifest, etag string) error
	current func(ctx context.Context, item Item) (string, error)
	now     func() time.Time
	cache   *manifestCache
}

var (
	// ErrManifestConflict: an edit kept meeting newer writes past MaxRetries.
	ErrManifestConflict = errors.New("media: manifest edit kept conflicting")
	// ErrOpReused refuses an operation id a recent edit used for another
	// request.
	ErrOpReused = errors.New("media: operation id reused for a different request")
	// ErrUnchanged lets an edit function say it changes nothing.
	ErrUnchanged = errors.New("media: manifest unchanged")
	// ErrBucketVersioned refuses media writes on a bucket that has had
	// versioning: its conditional PUT is not atomic with the write.
	ErrBucketVersioned = errors.New("media: manifest writes need a bucket that never had versioning")
	// ErrConditionalPutRequired refuses media writes on a backend without
	// conditional PUT.
	ErrConditionalPutRequired = errors.New("media: manifest writes require conditional PUT support")
)

// SettlingError refuses an edit that references a hash whose DELETE may
// still be in flight. Write the bytes again after Until and retry.
type SettlingError struct {
	Blob  string
	Until time.Time
}

func (e *SettlingError) Error() string {
	return fmt.Sprintf("media: %s is being deleted until %s", e.Blob, e.Until.Format(time.RFC3339))
}

// MissingObjectError refuses an edit that references bytes the bucket does
// not hold. Write them again and retry.
type MissingObjectError struct{ Key string }

func (e *MissingObjectError) Error() string { return "media: referenced object is missing: " + e.Key }

// RequireWritable is nil when store can hold manifests: conditional PUT
// works and the bucket never had versioning. ErrUnavailable while its
// capabilities are unknown.
func RequireWritable(ctx context.Context, store media.Store) error {
	c, err := store.Capabilities(ctx)
	if err != nil {
		return err
	}
	switch {
	case !c.ConditionalPut:
		return ErrConditionalPutRequired
	case !c.NeverVersioned:
		return ErrBucketVersioned
	}
	return nil
}

func NewManifests(store media.Store, reg *Registry, o ManifestOptions) (*Manifests, error) {
	if store == nil || reg == nil {
		return nil, errors.New("media: Manifests needs a Store and a Registry")
	}
	if o.CacheBytes <= 0 {
		o.CacheBytes = 128 << 20
	}
	o.CacheBytes = max(o.CacheBytes, 2*media.MaxManifestBytes*decodeFactor)
	if o.MaxRetries <= 0 {
		o.MaxRetries = 16
	}
	if o.Grace <= 0 {
		o.Grace = 7 * 24 * time.Hour
	}
	if o.Settle <= 0 {
		o.Settle = DefaultSettle
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	m := &Manifests{store: store, reg: reg, retries: o.MaxRetries, grace: o.Grace, settle: o.Settle,
		written: o.Written, current: o.Current, now: o.Now, cache: newManifestCache(o.CacheBytes)}
	reg.defaults = m.defaultImage
	return m, nil
}

// Registry is the registry the manifests are read under.
func (m *Manifests) Registry() *Registry { return m.reg }

// Store is the bucket.
func (m *Manifests) Store() media.Store { return m.store }

// DefaultSettle is ManifestOptions.Settle's default.
const DefaultSettle = 2 * time.Minute

// Settle is how long a hash being deleted stays fenced.
func (m *Manifests) Settle() time.Duration { return m.settle }

// Get returns ref's manifest and ETag, or ErrNotFound (also for a deleted
// item). With ManifestOptions.Current, a decoded copy of the revision the
// item cache records is served from memory; otherwise, or for another
// revision, the bucket is asked (a conditional GET for a cached copy). The
// manifest is shared: never modify it (Clone it).
func (m *Manifests) Get(ctx context.Context, ref contentref.ContentRef) (*Manifest, string, error) {
	if m.current != nil {
		if item, err := m.reg.Item(ref); err == nil {
			if recorded, err := m.current(ctx, item); err == nil && recorded != "" {
				if etag, cached := m.cache.get(item.ManifestKey()); cached != nil && etag == recorded {
					if cached.Deleted != nil {
						return nil, etag, ErrNotFound
					}
					return cached, etag, nil
				}
			}
		}
	}
	cur, etag, err := m.GetAny(ctx, ref)
	if err == nil && cur.Deleted != nil {
		return nil, etag, ErrNotFound
	}
	return cur, etag, err
}

// GetAny is Get including a deleted item's manifest.
func (m *Manifests) GetAny(ctx context.Context, ref contentref.ContentRef) (*Manifest, string, error) {
	item, err := m.reg.Item(ref)
	if err != nil {
		return nil, "", err
	}
	return m.get(ctx, item.ManifestKey())
}

// Settling is when blob, whose delete m records as possibly still in
// flight, may be referenced again; false when nothing holds it back.
func (ms *Manifests) Settling(m *Manifest, blob string, now time.Time) (time.Time, bool) {
	r, retired := m.Retiring(blob)
	if !retired || r.Deleting == nil {
		return time.Time{}, false
	}
	until := r.Deleting.Add(ms.settle)
	return until, now.Before(until)
}

// Sync records ref's stored manifest (ManifestOptions.Written): a write
// whose record failed is recorded by the next job of its item. An item
// without a manifest records nothing.
func (m *Manifests) Sync(ctx context.Context, ref contentref.ContentRef) error {
	if m.written == nil {
		return nil
	}
	item, err := m.reg.Item(ref)
	if err != nil {
		return err
	}
	cur, etag, err := m.get(ctx, item.ManifestKey())
	if errors.Is(err, ErrNotFound) {
		return nil
	} else if err != nil {
		return err
	}
	if err := m.written(ctx, item, cur, etag); err != nil {
		return fmt.Errorf("media: record stored manifest: %w", err)
	}
	return nil
}

// EditOptions refine an edit.
type EditOptions struct {
	// Existing refuses an item without a manifest (ErrNotFound): workers
	// edit after processing, so a deleted item stays deleted.
	Existing bool
	// Lifecycle lets the edit change a deleted item and Deleted itself:
	// deletion, restore, purge and GC.
	Lifecycle bool
	// Op identifies the edit's writes, so a re-read after an ambiguous answer
	// recognises its own; default a fresh UUIDv7. A caller retrying the
	// same request passes the same Op.
	Op string
	// Sum is the request's digest: the write records it with Op
	// (Manifest.Ops), so a retry after later writes is recognised and a
	// reused Op with another Sum is ErrOpReused.
	Sum string
	// Grace overrides how long what this edit stops referencing stays
	// retired; negative is due at once.
	Grace time.Duration
}

// Edit applies fn to ref's manifest (empty if none) and writes it. fn may
// run several times, each on a fresh read: it must be a re-appliable delta.
// The result is normalized (Kind.Normalize) and validated; an unchanged
// manifest is not written.
func (m *Manifests) Edit(ctx context.Context, ref contentref.ContentRef, fn func(*Manifest) error) (*Manifest, error) {
	return m.EditWith(ctx, ref, EditOptions{}, fn)
}

// EditExisting is Edit while the manifest exists (ErrNotFound otherwise).
func (m *Manifests) EditExisting(ctx context.Context, ref contentref.ContentRef, fn func(*Manifest) error) (*Manifest, error) {
	return m.EditWith(ctx, ref, EditOptions{Existing: true}, fn)
}

// EditWith is Edit with options.
func (m *Manifests) EditWith(ctx context.Context, ref contentref.ContentRef, o EditOptions, fn func(*Manifest) error) (*Manifest, error) {
	item, err := m.reg.Item(ref)
	if err != nil {
		return nil, err
	}
	return m.EditItem(ctx, item, o, fn)
}

// EditItem is EditWith for an item already resolved, such as a kind's
// DefaultsItem.
func (m *Manifests) EditItem(ctx context.Context, item Item, o EditOptions, fn func(*Manifest) error) (*Manifest, error) {
	if err := RequireWritable(ctx, m.store); err != nil {
		return nil, err
	}
	if o.Op == "" {
		o.Op = uuidv7.NewString()
	}
	var last error // the store's answer to the last attempt, when it had one
	for attempt := 0; attempt < m.retries; attempt++ {
		if attempt > 0 {
			if err := sleep(ctx, backoff(attempt)); err != nil {
				return nil, err
			}
		}
		out, etag, retry, err := m.try(ctx, item, o, fn)
		if retry {
			last = err
			continue
		}
		if err != nil {
			return nil, err
		}
		if m.written != nil {
			if err := m.written(ctx, item, out, etag); err != nil {
				return nil, fmt.Errorf("media: record written manifest: %w", err)
			}
		}
		return out, nil
	}
	if last != nil {
		return nil, last // the store kept failing, not other writers
	}
	return nil, ErrManifestConflict
}

// try makes one attempt; retry asks for a fresh read.
func (m *Manifests) try(ctx context.Context, item Item, o EditOptions, fn func(*Manifest) error) (out *Manifest, etag string, retry bool, err error) {
	key := item.ManifestKey()
	cur, etag, err := m.get(ctx, key)
	switch {
	case errors.Is(err, ErrNotFound) && o.Existing:
		return nil, "", false, err
	case errors.Is(err, ErrNotFound):
		cur, etag = &Manifest{V: media.ManifestVersion, Files: []media.File{}}, ""
	case err != nil:
		return nil, "", false, err
	}
	if o.Sum != "" {
		for _, r := range cur.Ops {
			if r.ID == o.Op && r.Sum == o.Sum {
				return cur, etag, false, nil // an earlier attempt landed
			} else if r.ID == o.Op {
				return nil, "", false, ErrOpReused
			}
		}
	} else if cur.Op == o.Op {
		return cur, etag, false, nil // an earlier attempt landed
	}
	if cur.Deleted != nil && !o.Lifecycle {
		return nil, "", false, ErrNotFound
	}
	next := cur.Clone()
	if err := fn(next); errors.Is(err, ErrUnchanged) {
		return cur, etag, false, nil
	} else if err != nil {
		return nil, "", false, err
	}
	if !o.Lifecycle && !reflect.DeepEqual(next.Deleted, cur.Deleted) {
		return nil, "", false, errors.New("media: only lifecycle edits change deletion")
	}
	if next.Deleted == nil || !next.Deleted.Erased {
		item.kind.Normalize(next)
	}
	now := m.now().UTC().Truncate(time.Millisecond)
	fresh, err := m.retire(cur, next, o, now)
	if err != nil {
		return nil, "", false, err
	}
	if etag != "" && unchanged(cur, next) {
		return cur, etag, false, nil
	}
	if err := next.Validate(); err != nil {
		return nil, "", false, err
	}
	next.Rev, next.Op, next.WrittenAt = cur.Rev+1, o.Op, now
	if o.Sum != "" {
		next.Ops = append(next.Ops, OpReceipt{ID: o.Op, Sum: o.Sum})
		if n := len(next.Ops); n > maxReceipts {
			next.Ops = next.Ops[n-maxReceipts:]
		}
	}
	next.Item = item.identity()
	body, err := encodeManifest(next)
	if err != nil {
		return nil, "", false, err
	}
	if next.size > media.MaxManifestBytes {
		return nil, "", false, fmt.Errorf("%w: %d bytes, at most %d", ErrManifestTooLarge, next.size, media.MaxManifestBytes)
	}
	if err := m.present(ctx, item, fresh); err != nil {
		return nil, "", false, err
	}
	opts := media.PutOptions{ContentType: "application/gzip", CacheControl: "no-store"}
	if etag == "" {
		opts.IfNoneMatch = "*"
	} else {
		opts.IfMatch = etag
	}
	obj, err := m.store.Put(ctx, key, bytes.NewReader(body), int64(len(body)), opts)
	switch {
	case errors.Is(err, ErrPreconditionFailed):
		m.cache.remove(key)
		return nil, "", true, nil
	case err != nil && ambiguous(ctx, err):
		// The write may land later; it carries the old precondition, so it
		// can only land before anything newer. The re-read decides.
		m.cache.remove(key)
		return nil, "", true, err
	case err != nil:
		return nil, "", false, err
	}
	next.reindex()
	m.cache.put(key, obj.ETag, next)
	return next, obj.ETag, false, nil
}

// retire records what next stops referencing in Retired and un-retires
// what it references again, refusing a hash whose DELETE may still land.
// It returns the blobs next references that cur did not, which must exist.
func (m *Manifests) retire(cur, next *Manifest, o EditOptions, now time.Time) ([]string, error) {
	before, after := cur.referenced(), next.referenced()
	paths := map[string]string{}
	for _, f := range cur.Files {
		paths[f.Blob] = f.Path
	}
	var fresh []string
	for _, blob := range slices.Sorted(maps.Keys(after)) {
		r, retired := next.Retiring(blob)
		if retired && r.Deleting != nil {
			if until := r.Deleting.Add(m.settle); now.Before(until) {
				return nil, &SettlingError{Blob: blob, Until: until}
			}
		}
		if retired {
			next.Retired = slices.DeleteFunc(next.Retired, func(x Retired) bool { return x.Blob == blob })
		}
		if !before[blob] || retired && r.Deleting != nil {
			fresh = append(fresh, blob)
		}
	}
	grace := cmpDuration(o.Grace, m.grace)
	for _, blob := range slices.Sorted(maps.Keys(before)) {
		if after[blob] {
			continue
		}
		if _, ok := next.Retiring(blob); ok {
			continue
		}
		next.Retired = append(next.Retired, Retired{Blob: blob, Path: paths[blob], At: now, Due: now.Add(max(grace, 0))})
	}
	return fresh, nil
}

func cmpDuration(a, b time.Duration) time.Duration {
	if a != 0 {
		return a
	}
	return b
}

// present HEADs newly referenced objects: a manifest never names bytes the
// bucket does not hold.
func (m *Manifests) present(ctx context.Context, item Item, blobs []string) error {
	errs := make([]error, len(blobs))
	var wg sync.WaitGroup
	for w := 0; w < min(8, len(blobs)); w++ {
		wg.Go(func() {
			for i := w; i < len(blobs); i += 8 {
				key, err := item.Blob(blobs[i])
				if err == nil {
					_, err = m.store.Head(ctx, key)
					if errors.Is(err, ErrNotFound) {
						err = &MissingObjectError{Key: key}
					}
				}
				errs[i] = err
			}
		})
	}
	wg.Wait()
	return errors.Join(errs...)
}

// unchanged reports an edit that changed nothing stored.
func unchanged(cur, next *Manifest) bool {
	return reflect.DeepEqual(cur.Deleted, next.Deleted) && reflect.DeepEqual(cur.Meta, next.Meta) &&
		reflect.DeepEqual(cur.Files, next.Files) && reflect.DeepEqual(cur.Retired, next.Retired)
}

// ambiguous reports a PUT whose outcome is unknown: a timeout, a dropped
// connection or a 5xx. The caller's own cancellation is not retried.
func ambiguous(ctx context.Context, err error) bool {
	return ctx.Err() == nil && (errors.Is(err, media.ErrUnavailable) || errors.Is(err, context.DeadlineExceeded))
}

func backoff(attempt int) time.Duration {
	d := min(10*time.Millisecond<<min(attempt, 7), time.Second)
	return d/2 + rand.N(d/2+1)
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// get reads and decodes a manifest through the ETag-revalidated cache.
func (m *Manifests) get(ctx context.Context, key string) (*Manifest, string, error) {
	cachedETag, cached := m.cache.get(key)
	rc, obj, err := m.store.Get(ctx, key, media.GetOptions{IfNoneMatch: cachedETag})
	switch {
	case errors.Is(err, ErrNotModified) && cached != nil:
		return cached, cachedETag, nil
	case errors.Is(err, ErrNotFound):
		m.cache.remove(key)
		return nil, "", err
	case err != nil:
		return nil, "", err
	}
	body, err := io.ReadAll(io.LimitReader(rc, media.MaxManifestBytes+1))
	rc.Close()
	if err != nil {
		return nil, "", err
	}
	man, err := decodeManifest(body)
	if err == nil {
		// Every write validates; a manifest written by hand fails closed.
		err = man.Validate()
	}
	if err != nil {
		return nil, obj.ETag, fmt.Errorf("media: decode manifest %s: %w", key, err)
	}
	m.cache.put(key, obj.ETag, man)
	return man, obj.ETag, nil
}

// identity names item in its manifest.
func (i Item) identity() *ItemIdentity {
	return &ItemIdentity{Kind: i.ref.ContentKind, Content: i.ref.ContentID, Version: i.ref.Version()}
}

// Ref rebuilds the reference a manifest names, in the registry's namespace.
func (r *Registry) RefOf(id *ItemIdentity) (contentref.ContentRef, error) {
	if id == nil {
		return contentref.ContentRef{}, errors.New("media: manifest names no item")
	}
	if id.Version != "" {
		return r.VersionRef(id.Kind, id.Content, id.Version)
	}
	return r.Ref(id.Kind, id.Content)
}

// keyOf is a blob's object key in item.
func keyOf(item Item, blob string) string {
	key, _ := item.Blob(blob)
	return key
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
