package media

import (
	"bytes"
	"cmp"
	"container/list"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media/layout"
)

// Locker serializes manifest edits across every process sharing the bucket.
type Locker interface {
	Lock(ctx context.Context, key string) (unlock func(), err error)
}

// ManifestOptions configure Manifests.
type ManifestOptions struct {
	// Locker is required: every edit runs under it and uses conditional PUT.
	// The lock coordinates manifest edits with cleanup; it cannot replace
	// the storage precondition if a PUT outlives the caller. See PGLocker.
	Locker Locker
	// Journal is required. Every process sharing a folder must use the same
	// migrated ContentKit schema to recover uncertain writes before proceeding.
	Journal *PGJournal
	// CacheBytes bounds the decoded manifests kept in process, revalidated
	// by ETag. A manifest costs about three times its JSON, so the largest
	// (MaxManifestBytes) costs 24 MiB; default 128 MiB, at least two of them.
	CacheBytes int64
	MaxRetries int // conditional-write attempts per recovery fence; default 16
	// CleanupGrace bounds unfinished private allocations; default 24 hours.
	CleanupGrace time.Duration
	// LateUploadWindow delays one repeat of exact retired keys to catch a
	// write already in flight. Minimum seven days plus one hour.
	LateUploadWindow time.Duration
}

// Manifests reads and edits item manifests.
type Manifests struct {
	store            Store
	reg              *Registry
	locker           Locker
	journal          *PGJournal
	cleanupGrace     time.Duration
	lateUploadWindow time.Duration
	retries          int
	cache            *manifestCache
}

var ErrManifestConflict = errors.New("media: manifest edit kept conflicting")

// ErrConditionalPutRequired refuses mutations on a backend that cannot fence
// a delayed manifest PUT.
var ErrConditionalPutRequired = errors.New("media: manifest writes require conditional PUT support")

// RequireConditionalPut is nil when store fences writes with conditional
// PUT: ErrUnavailable while its capabilities are unknown, else
// ErrConditionalPutRequired when it cannot.
func RequireConditionalPut(ctx context.Context, store Store) error {
	c, err := store.Capabilities(ctx)
	if err != nil {
		return err
	}
	if !c.ConditionalPut {
		return ErrConditionalPutRequired
	}
	return nil
}

func NewManifests(store Store, reg *Registry, o ManifestOptions) (*Manifests, error) {
	if store == nil || reg == nil || o.Locker == nil || o.Journal == nil {
		return nil, errors.New("media: Manifests needs a Store, a Registry, a Locker and a Journal")
	}
	if o.CacheBytes <= 0 {
		o.CacheBytes = 128 << 20
	}
	o.CacheBytes = max(o.CacheBytes, 2*MaxManifestBytes*decodeFactor)
	if o.MaxRetries <= 0 {
		o.MaxRetries = 16
	}
	if o.CleanupGrace <= 0 {
		o.CleanupGrace = 24 * time.Hour
	}
	if o.LateUploadWindow < minimumLateUploadWindow {
		o.LateUploadWindow = minimumLateUploadWindow
	}
	return &Manifests{store: store, reg: reg, locker: o.Locker, journal: o.Journal, cleanupGrace: o.CleanupGrace, lateUploadWindow: o.LateUploadWindow, retries: o.MaxRetries,
		cache: newManifestCache(o.CacheBytes)}, nil
}

// Registry is the registry the manifests are read under.
func (m *Manifests) Registry() *Registry { return m.reg }

// Store is the bucket.
func (m *Manifests) Store() Store { return m.store }

// WithLock runs fn while holding the configured manifest lock. fn may read
// this manifest but must not mutate it (mutations acquire the same lock).
// It lets a consumer atomically check media readiness and publish its catalog
// record without a concurrent upload changing the checked source.
func (m *Manifests) WithLock(ctx context.Context, ref contentref.ContentRef, fn func() error) error {
	item, err := m.reg.Item(ref)
	if err != nil {
		return err
	}
	unlock, err := m.locker.Lock(ctx, item.ManifestKey())
	if err != nil {
		return err
	}
	defer unlock()
	return fn()
}

// WithRecoveredLock settles any admitted manifest write before fn runs under
// the same manifest lock. Catalog marker authoring uses this before acquiring
// catalog row locks, so it cannot author a marker against bytes a pending
// accepted removal is about to replace. fn must not mutate this manifest.
func (m *Manifests) WithRecoveredLock(ctx context.Context, ref contentref.ContentRef, fn func() error) error {
	item, err := m.reg.Item(ref)
	if err != nil {
		return err
	}
	unlock, err := m.locker.Lock(ctx, item.ManifestKey())
	if err != nil {
		return err
	}
	defer unlock()
	if _, _, err := m.recoverLocked(ctx, item); err != nil {
		return err
	}
	return fn()
}

// Get returns ref's manifest and ETag, or ErrNotFound. Cached copies are
// revalidated with a conditional GET, so a read is never stale. The
// manifest is shared: never modify it (Clone it).
func (m *Manifests) Get(ctx context.Context, ref contentref.ContentRef) (*Manifest, string, error) {
	return m.getRef(ctx, m.journal.pool, ref)
}

// GetTx reads current media while validating version ownership through the
// caller's held transaction. It preserves lock-then-read ordering without
// borrowing another pool connection while other writers wait on that lock.
func (m *Manifests) GetTx(ctx context.Context, tx pgx.Tx, ref contentref.ContentRef) (*Manifest, string, error) {
	if tx == nil {
		return nil, "", errors.New("media: GetTx requires a transaction")
	}
	return m.getRef(ctx, tx, ref)
}

func (m *Manifests) getRef(ctx context.Context, q folderQuerier, ref contentref.ContentRef) (*Manifest, string, error) {
	item, err := m.reg.Item(ref)
	if err != nil {
		return nil, "", err
	}
	cur, etag, err := m.get(ctx, item.ManifestKey())
	if err == nil && ref.Version() != "" {
		if ownerErr := m.journal.checkFolderOwner(ctx, q, item, true); ownerErr != nil {
			return nil, etag, ownerErr
		}
	}
	if err == nil && cur.Deleted {
		return nil, etag, ErrNotFound
	}
	return cur, etag, err
}

// Edit applies fn to ref's manifest (empty if none) and writes it with
// If-Match on the ETag it read (If-None-Match for a new one). An uncertain
// write is recovered before another edit starts; a fenced-out caller receives
// an error rather than repeating fn against a newer revision. The result is
// normalized (Kind.Normalize) and validated; an unchanged manifest is not written. A folder's first
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

// editLimit is where commits and worker records stop; lifecycle tombstones
// and the Full/Deficit flags retain headroom so they always fit.
const editLimit = MaxManifestBytes - editHeadroom

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

// SyncPublic deletes the public names the manifest does not keep
// (Kind.PublicKept), considering only recorded allocations. Unknown objects
// require an explicit integrity audit. Cleanup is bounded to one minute. Selected keys are
// returned for cache purging; failed deletes remain in the recovery journal.
func (m *Manifests) SyncPublic(ctx context.Context, ref contentref.ContentRef) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	item, err := m.reg.Item(ref)
	if err != nil {
		return nil, err
	}
	var keys []string
	err = m.cleanup(ctx, item, false, func(cur *Manifest, _ bool) (journalEffects, error) {
		want := item.Kind().PublicKept(cur)
		known, err := m.allocationKeys(ctx, item)
		if err != nil {
			return journalEffects{}, err
		}
		for _, key := range known {
			if strings.HasPrefix(key, item.PublicPrefix()) && !slices.Contains(want, strings.TrimPrefix(key, item.PublicPrefix())) {
				keys = append(keys, key)
			}
		}
		return journalEffects{Public: keys}, nil
	})
	return keys, err
}

// Unreferenced selects what DropUnreferenced deletes.
type Unreferenced struct {
	// Public are exact physical keys written by a producer. Only keys in
	// this item's public area and no longer owned by its manifest may go.
	// This internal producer API is not an HTTP deletion capability.
	Public []string
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
	return m.cleanup(ctx, item, false, func(cur *Manifest, _ bool) (journalEffects, error) {
		keys, err := m.unreferenced(ctx, item, cur, u)
		if err != nil {
			return journalEffects{}, err
		}
		kept := item.Kind().PublicKept(cur)
		var public []string
		for _, key := range u.Public {
			if blob, ok := item.BlobRef(key); ok {
				if area, _, _ := layout.SplitBlobRef(blob); area != layout.AreaPublic {
					if !slices.Contains(cur.Blobs(), blob) {
						keys = append(keys, key)
					}
					continue
				}
			}
			name, ok := strings.CutPrefix(key, item.PublicPrefix())
			canonical, err := item.Public(name)
			if !ok || err != nil || canonical != key {
				return journalEffects{}, errors.New("media: public retirement key is outside the item")
			}
			if !slices.Contains(kept, name) && !slices.Contains(public, key) {
				public = append(public, key)
			}
		}
		return journalEffects{Private: keys, Public: public}, nil
	})
}

// cleanup owns a journal lease before selecting targets. Retirement and its
// recoverable exact-key effects commit together before any DELETE is sent.
func (m *Manifests) cleanup(ctx context.Context, item Item, publicOnly bool, selectKeys func(*Manifest, bool) (journalEffects, error)) (err error) {
	unlock, err := m.locker.Lock(ctx, item.ManifestKey())
	if err != nil {
		return err
	}
	defer unlock()
	if _, _, err := m.recoverLocked(ctx, item); err != nil {
		return err
	}
	c, err := m.journal.begin(ctx, manifestCommit{ID: uuid.New(), Ref: item.Ref(), Folder: item.Prefix(),
		Fingerprint: sha256.Sum256([]byte("internal-media-cleanup")), Effects: journalEffects{Cleanup: true}})
	if err != nil {
		return err
	}
	defer func() {
		if c.State == "open" {
			err = errors.Join(err, m.journal.finish(context.WithoutCancel(ctx), c, false, nil, nil))
		}
	}()
	cur, _, err := m.get(ctx, item.ManifestKey())
	exists := err == nil
	if errors.Is(err, ErrNotFound) || publicOnly && errors.Is(err, ErrManifestUnreadable) {
		cur = &Manifest{}
	} else if err != nil {
		return err
	}
	if err := m.journal.checkReceipt(ctx, item, cur.Receipt); err != nil {
		return err
	}
	effects, err := selectKeys(cur, exists)
	if err != nil {
		return err
	}
	if publicOnly && (len(effects.Private) != 0 || len(effects.Allocate) != 0) {
		return errors.New("media: public cleanup cannot alter private allocation ownership")
	}
	if len(effects.Allocate) > 0 {
		effects.CleanupAt = time.Now().Add(m.cleanupGrace)
	}
	m.scheduleRetirement(item, &effects)
	// The projection is asserted before any DELETE: a file it names is never
	// one this cleanup retires.
	if err := m.journal.prepareCleanup(ctx, &c, effects, projectionOf(item, cur).without(item, effects.Public)); err != nil {
		return err
	}
	return m.finishCommit(ctx, item, c, true, nil)
}

// retiredPublicKeys derives physical ownership from the old manifest even
// when a deployment removed or renamed the preset. Historical generations
// without a recorded alias policy retain those exact possible alias targets.
func retiredPublicKeys(item Item, previous *Manifest, kept []string) ([]string, error) {
	var keys []string
	for _, file := range previous.Files {
		for _, publication := range file.Public {
			if len(publication.Blobs) > 0 {
				continue
			} // blob retirement owns prepared version outputs
			retired := missing(publication.NamesOnDisk(), kept)
			if len(retired) == 0 {
				continue
			}
			for _, name := range retired {
				key, err := item.Public(name)
				if err != nil {
					return nil, err
				}
				keys = append(keys, key)
			}
			if !publication.Ready() || publication.Generation == "" || publication.Stable != nil && !*publication.Stable {
				continue
			}
			for _, name := range missing(publication.Names, kept) {
				key, err := item.Public(name)
				if err != nil {
					return nil, err
				}
				keys = append(keys, key)
			}
		}
	}
	return keys, nil
}

// scheduleRetirement preserves exact permanent keys after an in-flight write
// could land. Temporary objects have their own absolute bucket expiry.
func (m *Manifests) scheduleRetirement(item Item, effects *journalEffects) {
	if len(effects.Public)+len(effects.Private) == 0 {
		return
	}
	effects.CleanupDelete = true
	if !effects.CleanupAt.IsZero() {
		return
	}
	effects.CleanupAt = time.Now()
	if len(effects.Public) > 0 || slices.ContainsFunc(effects.Private, func(key string) bool { return !strings.HasPrefix(key, item.TempPrefix()) }) {
		effects.CleanupAt = effects.CleanupAt.Add(m.lateUploadWindow)
	}
}

// unreferenced selects exact targets while the caller holds the folder lock.
func (m *Manifests) unreferenced(ctx context.Context, item Item, cur *Manifest, u Unreferenced) ([]string, error) {
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

		allocated, err := m.allocationKeys(ctx, item)
		if err != nil {
			return nil, err
		}
		for _, key := range allocated {
			if (strings.HasPrefix(key, item.PrivatePrefix()) || strings.HasPrefix(key, item.DownloadPrefix()) || strings.HasPrefix(key, item.OriginalsPrefix()) || strings.HasPrefix(key, item.InternalPrefix())) && !keep[key] && !slices.Contains(keys, key) {
				keys = append(keys, key)
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
	return keys, nil
}

// DeleteUnreferenced records cleanup in a worker's closing edit. The edit's
// journal retires and deletes these exact targets after its manifest write;
// the callback itself never deletes from a potentially stale snapshot.
func (m *Manifests) DeleteUnreferenced(ctx context.Context, item Item, cur *Manifest, blobs []string) error {
	if len(blobs) == 0 {
		return nil
	}
	keep := map[string]bool{}
	for _, b := range cur.Blobs() {
		keep[b] = true
	}
	for _, b := range blobs {
		if key, err := item.Blob(b); err == nil && !keep[b] {
			cur.cleanup = append(cur.cleanup, key)
		}
	}
	return nil
}

func (m *Manifests) edit(ctx context.Context, ref contentref.ContentRef, existing bool, b bound, fn func(*Manifest) error) (*Manifest, error) {
	return m.editOperation(ctx, ref, existing, b, nil, fn)
}

// Synchronous deletion and explicit migration operations own their durable
// journal retries themselves, without a runtime processing job.
type synchronousMutationOwnerKey struct{}

type manifestMutation struct {
	lifecycle  bool // only deletion, explicit reset and restore may edit a tombstone
	layoutCopy bool // a verified storage migration preserves the raw snapshot, including historical file order
	commit     manifestCommit
	effects    journalEffects
	quota      int64
}

// replayCommit resolves a known request before any copy or new-item policy
// work. A successful retry returns today's manifest, never replays old edits.
func (m *Manifests) replayCommit(ctx context.Context, item Item, c manifestCommit) (*Manifest, bool, error) {
	found, err := m.journal.existing(ctx, c)
	if err != nil || found == nil || found.State == "absent" {
		return nil, false, err
	}
	unlock, err := m.locker.Lock(ctx, item.ManifestKey())
	if err != nil {
		return nil, false, err
	}
	defer unlock()
	if _, _, err := m.recoverLocked(ctx, item); err != nil {
		return nil, false, err
	}
	found, err = m.journal.existing(ctx, c)
	if err != nil || found == nil || found.State != "applied" {
		return nil, false, err
	}
	cur, _, err := m.get(ctx, item.ManifestKey())
	return cur, true, err
}

func (m *Manifests) editOperation(ctx context.Context, ref contentref.ContentRef, existing bool, b bound, mutation *manifestMutation, fn func(*Manifest) error) (*Manifest, error) {
	item, err := m.reg.Item(ref)
	if err != nil {
		return nil, err
	}
	if err := RequireConditionalPut(ctx, m.store); err != nil {
		return nil, err
	}
	key := item.ManifestKey()
	unlock, err := m.locker.Lock(ctx, key)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if _, _, err := m.recoverLocked(ctx, item); err != nil {
		return nil, err
	}
	if mutation == nil {
		mutation = &manifestMutation{commit: manifestCommit{ID: uuid.New(),
			Fingerprint: sha256.Sum256([]byte("internal-manifest-edit"))}}
	}
	mutation.commit.Ref, mutation.commit.Folder = ref, item.Prefix()
	mutation.commit, err = m.journal.begin(ctx, mutation.commit)
	if err != nil {
		return nil, err
	}
	if mutation.commit.terminal() {
		if mutation.commit.State == "absent" {
			return nil, ErrManifestConflict
		}
		cur, _, err := m.get(ctx, key)
		return cur, err
	}
	out, _, err := m.try(ctx, item, existing, b, mutation, fn)
	if mutation.commit.State == "open" {
		var proj *projection
		if err == nil {
			proj = projectionOf(item, out) // unchanged and read under the lock
		}
		err = errors.Join(err, m.journal.finish(ctx, mutation.commit, err == nil, proj, m.reg.cfg.Hooks.PathsChanged))
	}
	return out, err
}

func (m *Manifests) try(ctx context.Context, item Item, existing bool, b bound, mutation *manifestMutation, fn func(*Manifest) error) (out *Manifest, written bool, err error) {
	key := item.ManifestKey()
	cur, etag, err := m.get(ctx, key)
	switch {
	case errors.Is(err, ErrNotFound) && existing:
		return nil, false, err
	case errors.Is(err, ErrNotFound):
		cur, etag = &Manifest{V: ManifestVersion, Incarnation: uuid.NewString(), Files: []File{}}, ""
		if item.Kind().Versions {
			cur.PublicationLayout = PublicationLayoutVersion
		}
	case errors.Is(err, ErrManifestUnreadable) && mutation.lifecycle:
		cur = &Manifest{V: ManifestVersion, Incarnation: uuid.NewString(), Files: []File{}}
	case err != nil:
		return nil, false, err
	}
	if cur.Deleted && !mutation.lifecycle {
		return nil, false, ErrNotFound
	}
	if err := m.journal.checkReceipt(ctx, item, cur.Receipt); err != nil {
		return nil, false, err
	}
	next := cur.Clone()
	if err := fn(next); err != nil {
		return nil, false, err
	}
	if !mutation.lifecycle && (next.Deleted != cur.Deleted || next.Incarnation != cur.Incarnation) {
		return nil, false, errors.New("media: only explicit lifecycle operations can change incarnation or deletion")
	}
	if !mutation.layoutCopy {
		item.kind.Normalize(next)
	}
	if err := next.Validate(); err != nil {
		return nil, false, err
	}
	allocations := manifestAllocations(item, next)
	if err := m.checkAllocationsIn(ctx, item, allocations, next.Incarnation); err != nil {
		return nil, false, err
	}
	for _, key := range next.cleanup {
		if !slices.Contains(allocations, key) {
			mutation.effects.Private = append(mutation.effects.Private, key)
		}
	}
	next.cleanup = nil
	// The committed manifests, not a bucket listing, determine retirement.
	// Shared allocations stay live while any current file still names them.
	for _, blob := range missing(cur.Blobs(), next.Blobs()) {
		key, _ := item.Blob(blob)
		mutation.effects.Private = append(mutation.effects.Private, key)
	}
	for _, name := range missing(cur.StagedNames(), next.StagedNames()) {
		key, _ := item.Staged(name)
		mutation.effects.Private = append(mutation.effects.Private, key)
	}
	retiredPublic, err := retiredPublicKeys(item, cur, item.Kind().PublicKept(next))
	if err != nil {
		return nil, false, err
	}
	mutation.effects.Public = append(mutation.effects.Public, retiredPublic...)
	slices.Sort(mutation.effects.Private)
	mutation.effects.Private = slices.Compact(mutation.effects.Private)
	m.scheduleRetirement(item, &mutation.effects)
	slices.Sort(mutation.effects.Public)
	mutation.effects.Public = slices.Compact(mutation.effects.Public)
	mutation.effects.Mirror = item.kind.mirrors(item, cur, next)
	if etag != "" && reflect.DeepEqual(mutation.effects, journalEffects{}) && next.PublicationLayout == cur.PublicationLayout && next.Incarnation == cur.Incarnation && next.Deleted == cur.Deleted && next.Hidden == cur.Hidden && next.Full == cur.Full && next.Deficit == cur.Deficit && reflect.DeepEqual(next.Meta, cur.Meta) && reflect.DeepEqual(next.Files, cur.Files) && reflect.DeepEqual(next.Deliveries, cur.Deliveries) {
		return cur, false, nil
	}
	if etag == "" && !mutation.lifecycle {
		if err := m.requireFresh(ctx, item); err != nil {
			return nil, false, err
		}
	}
	_, err = encodeManifest(next)
	if err != nil {
		return nil, false, err
	}
	if err := b.check(cur, next); err != nil {
		return nil, false, err
	}
	// A commit clears Full only when it really makes room: it frees, in the
	// manifest and in what its uploads will still add, the bytes the refused
	// record was short of. Less (a small shrink, one upload of many) would
	// only re-run the work to the same refusal.
	if cur.Full && b.project != nil && cur.size+b.project(cur)-next.size-b.project(next) >= max(cur.Deficit, 1) {
		next.Full, next.Deficit = false, 0
	}
	mutation.commit.ETag = etag
	if mutation.effects.Process != nil && next.Full && !publicPending(item.Kind(), next) {
		mutation.effects.Process = nil
	}
	mutation.commit.Attempt = uuid.New()
	next.Receipt = mutation.commit.receipt()
	body, err := encodeManifest(next)
	if err == nil {
		err = b.check(cur, next)
	}
	if err != nil {
		return nil, false, err
	}
	if hook := m.reg.cfg.Hooks.ValidatePathsChanged; hook != nil && (len(mutation.effects.Renames) > 0 || len(mutation.effects.RemovedPaths) > 0) {
		if err := hook(ctx, item.Ref(), mutation.effects.Renames, mutation.effects.RemovedPaths); err != nil {
			return nil, false, err
		}
	}
	if err := m.journal.prepare(ctx, &mutation.commit, mutation.effects, mutation.quota, body, next.Incarnation, mutation.lifecycle && next.Deleted); err != nil {
		return nil, false, err
	}
	// The charged quota rides on the object, so releasing it never needs
	// the manifest to decode.
	opts := PutOptions{ContentType: "application/gzip", CacheControl: "no-store",
		Metadata: map[string]string{uploadBytesMeta: strconv.FormatInt(next.uploadBytes(), 10)}}
	if etag == "" {
		opts.IfNoneMatch = "*"
	} else {
		opts.IfMatch = etag
	}
	obj, err := m.store.Put(ctx, key, bytes.NewReader(body), int64(len(body)), opts)
	if errors.Is(err, ErrPreconditionFailed) {
		m.cache.remove(key)
		if _, _, err := m.recoverAttemptLocked(ctx, item, &mutation.commit.ID); err != nil {
			return nil, false, err
		}
		state, err := m.journal.outcome(ctx, mutation.commit)
		if err != nil {
			return nil, false, err
		}
		if state != "applied" {
			// A frozen caller must not prepare another write against the
			// recovery ETag. The next authorized request starts the retry.
			return nil, false, ErrManifestConflict
		}
		cur, _, err := m.get(ctx, key)
		return cur, true, err
	}
	if err != nil {
		return nil, false, err
	}
	next.reindex()
	m.cache.put(key, obj.ETag, next)
	// S3 acknowledged the write. A disconnected caller must not prevent its
	// durable settlement or the cleanup that follows a successful edit.
	settleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	if err := m.finishCommit(settleCtx, item, mutation.commit, true, next); err != nil {
		return nil, true, err
	}
	return next, true, nil
}

// Recover resumes the exact admitted mutation inside its existing processing
// or purge owner. It never replays an edit callback against newer input.
func (m *Manifests) Recover(ctx context.Context, ref contentref.ContentRef) error {
	item, err := m.reg.Item(ref)
	if err != nil {
		return err
	}
	unlock, err := m.locker.Lock(ctx, item.ManifestKey())
	if err != nil {
		return err
	}
	defer unlock()
	_, _, err = m.recoverLocked(ctx, item)
	return err
}

func (m *Manifests) recoverLocked(ctx context.Context, item Item) (*Manifest, bool, error) {
	return m.recoverAttemptLocked(ctx, item, nil)
}

func (m *Manifests) recoverAttemptLocked(ctx context.Context, item Item, operation *uuid.UUID) (*Manifest, bool, error) {
	if item.Ref().Version() != "" {
		if err := m.journal.checkFolderOwner(ctx, m.journal.pool, item, false); err != nil {
			return nil, false, err
		}
	}
	if err := RequireConditionalPut(ctx, m.store); err != nil {
		return nil, false, err
	}
	commit, err := m.journal.freeze(ctx, item.Ref().TenantID, item.Prefix(), operation)
	if err != nil || commit == nil {
		return nil, false, err
	}
	if !commit.Ref.Equal(item.Ref()) || commit.Folder != item.Prefix() {
		return nil, false, ErrCommitIdentity
	}
	if commit.Effects.Cleanup {
		return nil, true, m.finishCommit(ctx, item, *commit, true, nil)
	}
	if commit.Attempt == uuid.Nil {
		// No prepared bytes were ever admitted. Freezing prevents the vanished
		// request from preparing later; there is no S3 attempt to fence.
		return nil, false, m.finishCommit(ctx, item, *commit, false, nil)
	}
	next, err := decodeManifest(commit.Effects.Manifest)
	if err != nil || !commit.matches(next.Receipt) {
		return nil, false, ErrCommitIdentity
	}
	// Admission preserves the intended bytes, not ownership forever. A retry
	// must not adopt retired or reassigned allocations, even when S3 accepted
	// the manifest before the original request lost its response.
	if err := m.checkAllocationsIn(ctx, item, manifestAllocations(item, next), next.Incarnation); err != nil {
		return nil, false, err
	}
	for range m.retries {
		cur, etag, err := m.get(ctx, item.ManifestKey())
		if errors.Is(err, ErrNotFound) {
			cur, etag = &Manifest{}, ""
		} else if errors.Is(err, ErrManifestUnreadable) && commit.Effects.Deletion {
			cur = &Manifest{} // the admitted deletion replaces this exact unreadable root
		} else if err != nil {
			return nil, false, err
		}
		if commit.matches(cur.Receipt) {
			return cur, true, m.finishCommit(ctx, item, *commit, true, cur)
		}
		if etag != commit.ETag {
			return nil, false, ErrManifestConflict
		}
		if err := m.journal.checkReceipt(ctx, item, cur.Receipt); err != nil {
			return nil, false, err
		}
		opts := PutOptions{ContentType: "application/gzip", CacheControl: "no-store", Metadata: map[string]string{uploadBytesMeta: strconv.FormatInt(next.uploadBytes(), 10)}}
		if etag == "" {
			opts.IfNoneMatch = "*"
		} else {
			opts.IfMatch = etag
		}
		obj, err := m.store.Put(ctx, item.ManifestKey(), bytes.NewReader(commit.Effects.Manifest), int64(len(commit.Effects.Manifest)), opts)
		if errors.Is(err, ErrPreconditionFailed) {
			m.cache.remove(item.ManifestKey())
			continue
		}
		if err != nil {
			return nil, false, err
		}
		m.cache.put(item.ManifestKey(), obj.ETag, next)
		return next, true, m.finishCommit(ctx, item, *commit, true, next)
	}
	return nil, false, ErrManifestConflict
}

// finishCommit settles the admitted mutation and records exact retired keys.
// Physical deletion belongs to content purge, which retains those keys through
// its bounded late-write window. Projection and quota follow this manifest.
func (m *Manifests) finishCommit(ctx context.Context, item Item, commit manifestCommit, applied bool, current *Manifest) error {
	proj := projectionOf(item, current)
	if applied {
		if hook := m.reg.cfg.Hooks.BeforeRetire; hook != nil && (len(commit.Effects.Public) > 0 || len(commit.Effects.Private) > 0) {
			if current == nil {
				var err error
				current, _, err = m.get(ctx, item.ManifestKey())
				if err != nil && !errors.Is(err, ErrNotFound) {
					return err
				}
				if current == nil {
					current = &Manifest{}
				}
			}
			if err := beforeRetire(ctx, hook, item.Ref(), current); err != nil {
				return err
			}
		}
		if len(commit.Effects.Public) > 0 {
			// Readers leave the retired files before they are deleted.
			if err := m.journal.project(ctx, proj); err != nil {
				return err
			}
		}
		if err := m.retireAllocations(ctx, item, commit.Effects.Private); err != nil {
			return err
		}
		for _, c := range commit.Effects.Mirror {
			// A source already retired had a successor, which mirrors itself.
			if _, err := m.store.Copy(ctx, c.From, c.To, CopyOptions{}); err != nil && !errors.Is(err, ErrNotFound) {
				return fmt.Errorf("media: commit mirror: %w", err)
			}
		}

	}
	return m.journal.finish(ctx, commit, applied, proj, m.reg.cfg.Hooks.PathsChanged)
}

// A failed integration leaves the journal pending and every source intact.
func beforeRetire(ctx context.Context, hook func(context.Context, contentref.ContentRef, *Manifest) error, ref contentref.ContentRef, current *Manifest) (err error) {
	defer func() {
		if value := recover(); value != nil {
			err = fmt.Errorf("media: before retirement panic: %v", value)
		}
	}()
	return hook(ctx, ref, current)
}

// CompleteCommit finishes the exact mutation admitted with an ingest job.
// Its operation ID survives retries; no unrelated-folder recovery scan is used.
func (m *Manifests) CompleteCommit(ctx context.Context, ref contentref.ContentRef, operation string) error {
	id, err := uuid.Parse(operation)
	if err != nil || id == uuid.Nil {
		return ErrCommitIdentity
	}
	item, err := m.reg.Item(ref)
	if err != nil {
		return err
	}
	unlock, err := m.locker.Lock(ctx, item.ManifestKey())
	if err != nil {
		return err
	}
	_, _, err = m.recoverAttemptLocked(ctx, item, &id)
	unlock()
	if err != nil {
		return err
	}
	jr := m.journal.forTenant(ref.TenantID)
	if err := pgx.BeginFunc(ctx, jr.pool, func(tx pgx.Tx) error {
		c, err := jr.load(ctx, tx, ref.TenantID, id)
		if err != nil {
			return err
		}
		if !c.Ref.Equal(ref) || c.Folder != item.Prefix() || c.State != "applied" {
			return ErrCommitIdentity
		}
		return nil
	}); err != nil {
		return err
	}
	return m.notifyCommit(ctx, manifestCommit{ID: id, Ref: ref})
}

// notifyCommit is a direct, idempotent callback owned by the request or ingest
// attempt. Stock workers publish metadata as part of processing completion.
func (m *Manifests) notifyCommit(ctx context.Context, commit manifestCommit) error {
	if hook := m.reg.cfg.Hooks.ItemCommitted; hook != nil {
		if err := hook(ctx, commit.Ref); err != nil {
			return fmt.Errorf("media: ItemCommitted: %w", err)
		}
	}
	return nil
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
		return nil, obj.ETag, fmt.Errorf("media: decode manifest %s: %w", key, err)
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
	return m.cleanup(ctx, item, false, func(cur *Manifest, exists bool) (journalEffects, error) {
		if exists && !cur.Deleted {
			return journalEffects{}, nil
		}
		keys, err := m.unreferenced(ctx, item, cur, Unreferenced{Blobs: blobs})
		return journalEffects{Private: keys}, err
	})
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
