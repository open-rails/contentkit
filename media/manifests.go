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
	"log/slog"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sync/errgroup"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media/layout"
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
	// Sweeps schedules the folder's sweep after every written edit; best
	// effort (the periodic pass backs it up).
	Sweeps SweepScheduler
}

// Manifests reads and edits item manifests.
type Manifests struct {
	store   Store
	reg     *Registry
	locker  Locker
	journal *PGJournal
	sweeps  SweepScheduler
	retries int
	cache   *manifestCache
}

var ErrManifestConflict = errors.New("media: manifest edit kept conflicting")

// ErrConditionalPutRequired refuses mutations on a backend that cannot fence
// a delayed manifest PUT. Probe or declare the backend's capabilities first.
var ErrConditionalPutRequired = errors.New("media: manifest writes require conditional PUT support")

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
	return &Manifests{store: store, reg: reg, locker: o.Locker, journal: o.Journal, sweeps: o.Sweeps, retries: o.MaxRetries,
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

// SyncPublic deletes the public names the manifest does not keep
// (Kind.PublicKept), under the manifest lock. Cleanup is bounded to one
// minute; deleted keys are returned for cache purging, including partial
// success on failure.
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
	if _, _, err := m.recoverLocked(ctx, item); err != nil {
		return nil, err
	}
	cur, _, err := m.get(ctx, item.ManifestKey())
	if errors.Is(err, ErrNotFound) {
		cur = &Manifest{}
	} else if err != nil {
		return nil, err
	}
	if err := m.journal.checkReceipt(ctx, item, cur.Receipt); err != nil {
		return nil, err
	}
	want := map[string]bool{}
	for _, name := range item.Kind().PublicKept(cur) {
		key, err := item.Public(name)
		if err != nil {
			return nil, err
		}
		want[key] = true
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
	if _, _, err := m.recoverLocked(ctx, item); err != nil {
		return err
	}
	cur, _, err := m.get(ctx, item.ManifestKey())
	if errors.Is(err, ErrNotFound) {
		return nil // deleted: the folder deletion takes everything
	} else if err != nil {
		return err
	}
	if err := m.journal.checkReceipt(ctx, item, cur.Receipt); err != nil {
		return err
	}
	keys, err := m.unreferenced(ctx, item, cur, u)
	if err != nil {
		return err
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
		for obj, err := range m.store.List(ctx, item.PrivatePrefix()) {
			if err != nil {
				return nil, err
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
	return keys, nil
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
	return m.editOperation(ctx, ref, existing, b, nil, fn)
}

type manifestMutation struct {
	commit  manifestCommit
	effects journalEffects
	quota   int64
}

func (m *Manifests) editOperation(ctx context.Context, ref contentref.ContentRef, existing bool, b bound, mutation *manifestMutation, fn func(*Manifest) error) (*Manifest, error) {
	item, err := m.reg.Item(ref)
	if err != nil {
		return nil, err
	}
	if !m.store.Capabilities().ConditionalPut {
		return nil, ErrConditionalPutRequired
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
	out, written, err := m.try(ctx, item, existing, b, mutation, fn)
	if mutation.commit.State == "open" {
		err = errors.Join(err, m.journal.finish(ctx, mutation.commit, err == nil))
	}
	if err == nil && written && m.sweeps != nil {
		if serr := m.sweeps.ScheduleSweep(ctx, ref); serr != nil {
			slog.WarnContext(ctx, "media: schedule sweep", "key", key, "error", serr)
		}
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
		cur, etag = &Manifest{V: ManifestVersion, Files: []File{}}, ""
	case err != nil:
		return nil, false, err
	}
	if err := m.journal.checkReceipt(ctx, item, cur.Receipt); err != nil {
		return nil, false, err
	}
	next := cur.Clone()
	if err := fn(next); err != nil {
		return nil, false, err
	}
	item.kind.Normalize(next)
	if err := next.Validate(); err != nil {
		return nil, false, err
	}
	if etag != "" && reflect.DeepEqual(mutation.effects, journalEffects{}) && next.Hidden == cur.Hidden && next.Full == cur.Full && next.Deficit == cur.Deficit && reflect.DeepEqual(next.Meta, cur.Meta) && reflect.DeepEqual(next.Files, cur.Files) {
		return cur, false, nil
	}
	if etag == "" {
		if err := m.requireFresh(ctx, item); err != nil {
			return nil, false, err
		}
	}
	body, err := encodeManifest(next)
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
		if body, err = encodeManifest(next); err != nil {
			return nil, false, err
		}
	}
	mutation.commit.ETag = etag
	if mutation.effects.Process != nil && next.Full && !publicPending(item.Kind(), next) {
		mutation.effects.Process = nil
	}
	if err := m.journal.prepare(ctx, &mutation.commit, mutation.effects, mutation.quota); err != nil {
		return nil, false, err
	}
	next.Receipt = mutation.commit.receipt()
	body, err = encodeManifest(next)
	if err == nil {
		err = b.check(cur, next)
	}
	if err != nil {
		// Prepared but never sent: recovery still fences before releasing
		// the row, so a caller cannot mistake this for a sent attempt.
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
	if err := m.finishCommit(settleCtx, item, mutation.commit, true); err != nil {
		return nil, true, err
	}
	return next, true, nil
}

// Recover settles ref's unfinished attempt without replaying the edit. It
// freezes the database attempt before advancing S3's ETag, so a late PUT using
// the old ETag cannot land after an absent outcome has been refunded.
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
	if !m.store.Capabilities().ConditionalPut {
		return nil, false, ErrConditionalPutRequired
	}
	commit, err := m.journal.freeze(ctx, item.Ref().TenantID, item.Prefix(), operation)
	if err != nil || commit == nil {
		return nil, false, err
	}
	for range m.retries {
		cur, etag, err := m.get(ctx, item.ManifestKey())
		if errors.Is(err, ErrNotFound) {
			cur, etag = &Manifest{V: ManifestVersion, Files: []File{}}, ""
		} else if err != nil {
			return nil, false, err
		}
		applied := commit.matches(cur.Receipt)
		if !applied {
			if err := m.journal.checkReceipt(ctx, item, cur.Receipt); err != nil {
				return nil, false, err
			}
		}
		// Do not take the semantic no-op shortcut: this write is the fence.
		next := cur.Clone()
		body, err := encodeManifest(next)
		if err != nil {
			return nil, false, err
		}
		opts := PutOptions{ContentType: "application/gzip", CacheControl: "no-store",
			Metadata: map[string]string{uploadBytesMeta: strconv.FormatInt(next.uploadBytes(), 10)}}
		if etag == "" {
			opts.IfNoneMatch = "*"
		} else {
			opts.IfMatch = etag
		}
		obj, err := m.store.Put(ctx, item.ManifestKey(), bytes.NewReader(body), int64(len(body)), opts)
		if errors.Is(err, ErrPreconditionFailed) {
			m.cache.remove(item.ManifestKey())
			continue
		}
		if err != nil {
			return nil, false, err
		}
		m.cache.put(item.ManifestKey(), obj.ETag, next)
		if err := m.finishCommit(ctx, item, *commit, applied); err != nil {
			return nil, false, err
		}
		return next, applied, nil
	}
	return nil, false, ErrManifestConflict
}

// finishCommit repeats only the recorded cleanup targets, never a fresh list
// that might include another producer's outputs. Database effects settle only
// after cleanup succeeds; the pending receipt remains discoverable on failure.
func (m *Manifests) finishCommit(ctx context.Context, item Item, commit manifestCommit, applied bool) error {
	if applied {
		g, deleteCtx := errgroup.WithContext(ctx)
		g.SetLimit(8)
		for _, key := range append(slices.Clone(commit.Effects.Public), commit.Effects.Private...) {
			g.Go(func() error {
				if err := m.store.Delete(deleteCtx, key); err != nil && !errors.Is(err, ErrNotFound) {
					return err
				}
				return nil
			})
		}
		if err := g.Wait(); err != nil {
			return fmt.Errorf("media: commit cleanup: %w", err)
		}
		if purge := m.reg.cfg.Hooks.PurgePublic; purge != nil && len(commit.Effects.Public) > 0 {
			urls := make([]string, len(commit.Effects.Public))
			for i, key := range commit.Effects.Public {
				urls[i] = strings.TrimRight(m.reg.cfg.BaseURL, "/") + layout.URLPrefix + key
			}
			purge(ctx, urls)
		}
	}
	return m.journal.finish(ctx, commit, applied)
}

// RecoverPending scans the journal, including writes that never created an S3
// folder. It is bounded so the host's periodic maintenance can resume it.
func (m *Manifests) RecoverPending(ctx context.Context, limit int) error {
	if limit <= 0 {
		return errors.New("media: pending recovery requires a positive limit")
	}
	var errs []error
	for _, tenant := range m.reg.Namespaces() {
		var kinds []string
		for _, kind := range m.reg.cfg.Kinds {
			if kind.ns == tenant {
				kinds = append(kinds, kind.Name)
			}
		}
		pending, err := m.journal.pending(ctx, tenant, kinds, limit, m.reg.cfg.Hooks.ItemCommitted != nil)
		if err != nil {
			return err
		}
		for _, commit := range pending {
			item, err := m.reg.Item(commit.Ref)
			if err != nil || item.Prefix() != commit.Folder {
				continue // another registry sharing this schema owns this item
			}
			if err := m.Recover(ctx, commit.Ref); err != nil {
				errs = append(errs, err)
				continue
			}
			if err := m.notifyCommit(ctx, commit); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// notifyCommit runs outside the folder lock: host callbacks may read or edit
// media themselves. A crash between delivery and acknowledgement repeats the
// already-idempotent hook; it never removes an undelivered notification.
func (m *Manifests) notifyCommit(ctx context.Context, commit manifestCommit) error {
	hook := m.reg.cfg.Hooks.ItemCommitted
	if hook == nil {
		return nil // only the host owning the callback may acknowledge it
	}
	pending, err := m.journal.notification(ctx, commit)
	if err != nil || !pending {
		return err
	}
	if err := hook(ctx, commit.Ref); err != nil {
		return fmt.Errorf("media: ItemCommitted: %w", err)
	}
	return m.journal.acknowledgeNotification(ctx, commit)
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
