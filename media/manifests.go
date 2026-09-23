package media

import (
	"bytes"
	"container/list"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"slices"
	"sync"
	"time"

	"github.com/open-rails/contentkit/contentref"
)

// Locker serializes manifest edits on backends without conditional PUT.
type Locker interface {
	Lock(ctx context.Context, key string) (unlock func(), err error)
}

// ManifestOptions configure Manifests.
type ManifestOptions struct {
	// Locker is required when the store lacks ConditionalPut; edits then run
	// under it and write unconditionally. See PGLocker.
	Locker     Locker
	CacheSize  int // manifests kept in process, revalidated by ETag; default 4096
	MaxRetries int // CAS attempts per edit; default 16
	// Jobs, when set, schedules the folder's sweep after every written edit.
	// Scheduling is best-effort (logged); the periodic sweep pass backs it up.
	Jobs *Jobs
}

// Manifests reads and edits item manifests.
type Manifests struct {
	store   Store
	kinds   *Registry
	locker  Locker
	jobs    *Jobs
	retries int
	cache   *lru
}

var ErrManifestConflict = errors.New("media: manifest edit kept conflicting")

func NewManifests(store Store, kinds *Registry, opts ManifestOptions) (*Manifests, error) {
	if store == nil || kinds == nil {
		return nil, errors.New("media: Manifests needs a Store and a Registry")
	}
	locker := opts.Locker
	if store.Capabilities().ConditionalPut {
		locker = nil
	} else if locker == nil {
		return nil, errors.New("media: store lacks conditional PUT; ManifestOptions.Locker is required")
	}
	if opts.CacheSize <= 0 {
		opts.CacheSize = 4096
	}
	if opts.MaxRetries <= 0 {
		opts.MaxRetries = 16
	}
	return &Manifests{store: store, kinds: kinds, locker: locker, jobs: opts.Jobs, retries: opts.MaxRetries, cache: newLRU(opts.CacheSize)}, nil
}

// Get returns the manifest and its ETag, or ErrNotFound. Cached copies are
// revalidated with a conditional GET, so a read is never stale.
func (m *Manifests) Get(ctx context.Context, ref contentref.ContentRef) (*Manifest, string, error) {
	key, err := m.key(ref)
	if err != nil {
		return nil, "", err
	}
	return m.get(ctx, key)
}

// Edit applies fn to the current manifest (empty if none) and writes it with
// If-Match on the ETag it read (If-None-Match for a new one), re-reading and
// re-applying fn on conflict. fn must be safe to run more than once; an error
// from fn aborts the edit. An unchanged manifest is not written.
func (m *Manifests) Edit(ctx context.Context, ref contentref.ContentRef, fn func(*Manifest) error) (*Manifest, error) {
	key, err := m.key(ref)
	if err != nil {
		return nil, err
	}
	if m.locker != nil {
		unlock, err := m.locker.Lock(ctx, key)
		if err != nil {
			return nil, err
		}
		defer unlock()
		man, _, err := m.apply(ctx, ref, key, fn, false)
		return man, err
	}
	for attempt := 0; attempt < m.retries; attempt++ {
		man, conflict, err := m.apply(ctx, ref, key, fn, true)
		if !conflict {
			return man, err
		}
		backoff := time.Duration(1<<min(attempt, 6)) * 5 * time.Millisecond
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoff/2 + rand.N(backoff)):
		}
	}
	return nil, fmt.Errorf("%w: %s", ErrManifestConflict, key)
}

func (m *Manifests) apply(ctx context.Context, ref contentref.ContentRef, key string, fn func(*Manifest) error, conditional bool) (*Manifest, bool, error) {
	man, etag, err := m.get(ctx, key)
	if errors.Is(err, ErrNotFound) {
		man, etag = &Manifest{}, ""
	} else if err != nil {
		return nil, false, err
	}
	before, _ := json.Marshal(man)
	if err := fn(man); err != nil {
		return nil, false, err
	}
	if err := man.Validate(); err != nil {
		return nil, false, err
	}
	if man.Files == nil {
		man.Files = []File{}
	}
	body, err := json.Marshal(man)
	if err != nil {
		return nil, false, err
	}
	if etag != "" && bytes.Equal(before, body) {
		return man, false, nil
	}
	opts := PutOptions{ContentType: "application/json", CacheControl: "no-store"}
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
		return nil, true, nil
	}
	if err != nil {
		return nil, false, err
	}
	m.cache.put(key, obj.ETag, body)
	if m.jobs != nil {
		if err := m.jobs.ScheduleSweep(ctx, ref); err != nil {
			m.jobs.cfg.Logger.WarnContext(ctx, "media: schedule sweep", "key", key, "error", err)
		}
	}
	return man, false, nil
}

func (m *Manifests) get(ctx context.Context, key string) (*Manifest, string, error) {
	cachedETag, cached := m.cache.get(key)
	rc, obj, err := m.store.Get(ctx, key, GetOptions{IfNoneMatch: cachedETag})
	var body []byte
	switch {
	case errors.Is(err, ErrNotModified) && cached != nil:
		body, obj.ETag = cached, cachedETag
	case errors.Is(err, ErrNotFound):
		m.cache.remove(key)
		return nil, "", err
	case err != nil:
		return nil, "", err
	default:
		body, err = io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return nil, "", err
		}
		m.cache.put(key, obj.ETag, body)
	}
	var man Manifest
	if err := json.Unmarshal(body, &man); err != nil {
		return nil, "", fmt.Errorf("media: decode manifest %s: %w", key, err)
	}
	return &man, obj.ETag, nil
}

func (m *Manifests) key(ref contentref.ContentRef) (string, error) {
	item, err := m.kinds.Item(ref)
	if err != nil {
		return "", err
	}
	return item.ManifestKey()
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

// references reports whether any manifest in the item's folder references the
// original name.
func (m *Manifests) references(ctx context.Context, item Item, name string) (bool, error) {
	var keys []string
	if item.Kind().Versioned {
		for o, err := range m.store.List(ctx, item.ManifestsPrefix()) {
			if err != nil {
				return false, err
			}
			keys = append(keys, o.Key)
		}
	} else {
		keys = []string{item.Prefix() + "manifest.json"}
	}
	for _, key := range keys {
		man, _, err := m.get(ctx, key)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return false, err
		}
		if slices.Contains(man.Originals(), name) {
			return true, nil
		}
	}
	return false, nil
}
