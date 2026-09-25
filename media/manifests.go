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
	return &Manifests{store: store, kinds: kinds, locker: locker, sweeps: opts.Sweeps, retries: opts.MaxRetries, cache: newLRU(opts.CacheSize)}, nil
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
	var man *Manifest
	written, err := m.edit(ctx, key, func(body []byte) ([]byte, error) {
		man = &Manifest{}
		if body != nil {
			if err := json.Unmarshal(body, man); err != nil {
				return nil, fmt.Errorf("media: decode manifest %s: %w", key, err)
			}
		}
		before, _ := json.Marshal(man)
		if err := fn(man); err != nil {
			return nil, err
		}
		if err := man.Validate(); err != nil {
			return nil, err
		}
		if man.Files == nil {
			man.Files = []File{}
		}
		out, err := json.Marshal(man)
		if err != nil || (body != nil && bytes.Equal(before, out)) {
			return nil, err
		}
		return out, nil
	})
	if err != nil {
		return nil, err
	}
	if written && m.sweeps != nil {
		if err := m.sweeps.ScheduleSweep(ctx, ref); err != nil {
			slog.WarnContext(ctx, "media: schedule sweep", "key", key, "error", err)
		}
	}
	return man, nil
}

// edit writes fn's replacement of the JSON object at key (body nil when
// absent) with If-Match on the ETag read, or under the Locker, re-running fn
// on conflict. fn returns nil to leave the object alone.
func (m *Manifests) edit(ctx context.Context, key string, fn func(body []byte) ([]byte, error)) (written bool, err error) {
	if m.locker != nil {
		unlock, err := m.locker.Lock(ctx, key)
		if err != nil {
			return false, err
		}
		defer unlock()
		_, written, err := m.try(ctx, key, fn, false)
		return written, err
	}
	for attempt := 0; attempt < m.retries; attempt++ {
		conflict, written, err := m.try(ctx, key, fn, true)
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

func (m *Manifests) get(ctx context.Context, key string) (*Manifest, string, error) {
	body, etag, err := m.raw(ctx, key)
	if err != nil {
		return nil, "", err
	}
	var man Manifest
	if err := json.Unmarshal(body, &man); err != nil {
		return nil, "", fmt.Errorf("media: decode manifest %s: %w", key, err)
	}
	return &man, etag, nil
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
	keys, err := m.manifestKeys(ctx, item)
	if err != nil {
		return false, err
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
