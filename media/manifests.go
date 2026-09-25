package media

import (
	"bytes"
	"container/list"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"slices"
	"sync"
	"time"

	"github.com/open-rails/contentkit/contentref"
)

// Locker serializes manifest edits across every process sharing the bucket.
type Locker interface {
	Lock(ctx context.Context, key string) (unlock func(), err error)
}

// ManifestOptions configure Manifests.
type ManifestOptions struct {
	// Locker is required: every edit runs under it (so processes that have
	// and have not probed the store never diverge), and also writes with
	// If-Match once the store reports ConditionalPut. See PGLocker.
	Locker     Locker
	CacheSize  int // manifests kept in process, revalidated by ETag; default 4096
	MaxRetries int // CAS attempts per edit; default 16
	// Sweeps, when set, schedules the folder's sweep after every written
	// edit: the host's *Jobs, or in the media worker a *HostQueue. Scheduling
	// is best-effort (logged); the periodic sweep pass backs it up.
	Sweeps SweepScheduler
}

// SweepScheduler schedules a folder's sweep after an edit.
type SweepScheduler interface {
	ScheduleSweep(ctx context.Context, ref contentref.ContentRef) error
}

// Manifests reads and edits item manifests.
type Manifests struct {
	store   Store
	kinds   *Registry
	locker  Locker
	sweeps  SweepScheduler
	retries int
	cache   *lru
}

var ErrManifestConflict = errors.New("media: manifest edit kept conflicting")

func NewManifests(store Store, kinds *Registry, opts ManifestOptions) (*Manifests, error) {
	if store == nil || kinds == nil {
		return nil, errors.New("media: Manifests needs a Store and a Registry")
	}
	locker := opts.Locker
	if locker == nil {
		return nil, errors.New("media: ManifestOptions.Locker is required")
	}
	if opts.CacheSize <= 0 {
		opts.CacheSize = 4096
	}
	if opts.MaxRetries <= 0 {
		opts.MaxRetries = 16
	}
	return &Manifests{store: store, kinds: kinds, locker: locker, sweeps: opts.Sweeps, retries: opts.MaxRetries, cache: newLRU(opts.CacheSize)}, nil
}

// Get returns ref's files (its version's section for a versioned kind) and
// the manifest's ETag, or ErrNotFound. Cached copies are revalidated with a
// conditional GET, so a read is never stale.
func (m *Manifests) Get(ctx context.Context, ref contentref.ContentRef) (*Manifest, string, error) {
	item, err := m.kinds.Item(ref)
	if err != nil {
		return nil, "", err
	}
	v, err := item.Section()
	if err != nil {
		return nil, "", err
	}
	root, etag, err := m.root(ctx, item.ManifestKey())
	if err != nil {
		return nil, "", err
	}
	man := root.Section(v)
	if man == nil {
		return nil, "", ErrNotFound
	}
	return man, etag, nil
}

// Root returns the item's manifest.json and its ETag, or ErrNotFound.
func (m *Manifests) Root(ctx context.Context, ref contentref.ContentRef) (*Root, string, error) {
	item, err := m.kinds.Item(ref.Content())
	if err != nil {
		return nil, "", err
	}
	return m.root(ctx, item.ManifestKey())
}

// Edit applies fn to ref's files (see Get; a new version starts empty) like
// EditRoot.
func (m *Manifests) Edit(ctx context.Context, ref contentref.ContentRef, fn func(*Manifest) error) (*Manifest, error) {
	item, err := m.kinds.Item(ref)
	if err != nil {
		return nil, err
	}
	v, err := item.Section()
	if err != nil {
		return nil, err
	}
	var man *Manifest
	if _, err := m.editRoot(ctx, item, func(r *Root) error {
		man = r.section(v)
		return fn(man)
	}); err != nil {
		return nil, err
	}
	return man, nil
}

// EditRoot applies fn to the item's manifest (empty if none) and writes it
// with If-Match on the ETag it read (If-None-Match for a new one),
// re-reading and re-applying fn on conflict. fn must be safe to run more
// than once; an error from fn aborts the edit. The index is rebuilt; an
// unchanged manifest is not written. A folder's first manifest is refused
// (ErrFolderNotEmpty) over a previous item's renditions.
func (m *Manifests) EditRoot(ctx context.Context, ref contentref.ContentRef, fn func(*Root) error) (*Root, error) {
	item, err := m.kinds.Item(ref.Content())
	if err != nil {
		return nil, err
	}
	return m.editRoot(ctx, item, fn)
}

func (m *Manifests) editRoot(ctx context.Context, item Item, fn func(*Root) error) (*Root, error) {
	key := item.ManifestKey()
	var root *Root
	written, err := m.edit(ctx, key, func(body []byte) ([]byte, error) {
		root = &Root{}
		if body != nil {
			if err := json.Unmarshal(body, root); err != nil {
				return nil, fmt.Errorf("media: decode manifest %s: %w", key, err)
			}
		}
		if err := fn(root); err != nil {
			return nil, err
		}
		if err := root.validate(); err != nil {
			return nil, err
		}
		root.normalize()
		root.index()
		out, err := json.Marshal(root)
		if err != nil || (body != nil && bytes.Equal(body, out)) {
			return nil, err
		}
		if body == nil {
			if err := m.requireFresh(ctx, item); err != nil {
				return nil, err
			}
		}
		return out, nil
	})
	if err != nil {
		return nil, err
	}
	if written && m.sweeps != nil {
		if err := m.sweeps.ScheduleSweep(ctx, item.Ref().Content()); err != nil {
			slog.WarnContext(ctx, "media: schedule sweep", "key", key, "error", err)
		}
	}
	return root, nil
}

// edit writes fn's replacement of the JSON object at key (body nil when
// absent) under the Locker, with If-Match on the ETag read
// when the store has conditional PUT, re-running fn on conflict. fn returns
// nil to leave the object alone.
func (m *Manifests) edit(ctx context.Context, key string, fn func(body []byte) ([]byte, error)) (written bool, err error) {
	unlock, err := m.locker.Lock(ctx, key)
	if err != nil {
		return false, err
	}
	defer unlock()
	conditional := m.store.Capabilities().ConditionalPut
	for attempt := 0; attempt < m.retries; attempt++ {
		conflict, written, err := m.try(ctx, key, fn, conditional)
		if !conflict {
			return written, err
		}
		backoff := time.Duration(1<<min(attempt, 6)) * 5 * time.Millisecond
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(backoff/2 + rand.N(backoff)):
		}
	}
	return false, fmt.Errorf("%w: %s", ErrManifestConflict, key)
}

func (m *Manifests) try(ctx context.Context, key string, fn func([]byte) ([]byte, error), conditional bool) (conflict, written bool, err error) {
	body, etag, err := m.raw(ctx, key)
	if errors.Is(err, ErrNotFound) {
		body, etag = nil, ""
	} else if err != nil {
		return false, false, err
	}
	out, err := fn(body)
	if err != nil || out == nil {
		return false, false, err
	}
	opts := PutOptions{ContentType: "application/json", CacheControl: "no-store"}
	if conditional {
		if etag == "" {
			opts.IfNoneMatch = "*"
		} else {
			opts.IfMatch = etag
		}
	}
	obj, err := m.store.Put(ctx, key, bytes.NewReader(out), int64(len(out)), opts)
	if errors.Is(err, ErrPreconditionFailed) {
		m.cache.remove(key)
		return true, false, nil
	}
	if err != nil {
		return false, false, err
	}
	m.cache.put(key, obj.ETag, out)
	return false, true, nil
}

func (m *Manifests) root(ctx context.Context, key string) (*Root, string, error) {
	body, etag, err := m.raw(ctx, key)
	if err != nil {
		return nil, "", err
	}
	var root Root
	if err := json.Unmarshal(body, &root); err != nil {
		return nil, "", fmt.Errorf("media: decode manifest %s: %w", key, err)
	}
	return &root, etag, nil
}

// raw reads an object through the ETag-revalidated cache.
func (m *Manifests) raw(ctx context.Context, key string) ([]byte, string, error) {
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
	body, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		return nil, "", err
	}
	m.cache.put(key, obj.ETag, body)
	return body, obj.ETag, nil
}

type lru struct {
	mu    sync.Mutex
	max   int
	order *list.List
	items map[string]*list.Element
}

type lruEntry struct {
	key, etag string
	body      []byte
}

func newLRU(max int) *lru {
	return &lru{max: max, order: list.New(), items: map[string]*list.Element{}}
}

func (c *lru) get(key string) (string, []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[key]; ok {
		c.order.MoveToFront(e)
		v := e.Value.(*lruEntry)
		return v.etag, v.body
	}
	return "", nil
}

func (c *lru) put(key, etag string, body []byte) {
	if etag == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[key]; ok {
		e.Value = &lruEntry{key, etag, body}
		c.order.MoveToFront(e)
		return
	}
	c.items[key] = c.order.PushFront(&lruEntry{key, etag, body})
	if c.order.Len() > c.max {
		last := c.order.Back()
		c.order.Remove(last)
		delete(c.items, last.Value.(*lruEntry).key)
	}
}

func (c *lru) remove(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[key]; ok {
		c.order.Remove(e)
		delete(c.items, key)
	}
}

// references reports whether the item's manifest references the original
// name.
func (m *Manifests) references(ctx context.Context, item Item, name string) (bool, error) {
	root, _, err := m.root(ctx, item.ManifestKey())
	if errors.Is(err, ErrNotFound) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	if _, ok := root.Originals[name]; ok {
		return true, nil
	}
	found := false
	root.sections(func(_ string, man *Manifest) { found = found || slices.Contains(man.Sources(), name) })
	return found, nil
}
