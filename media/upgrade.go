package media

import (
	"bytes"
	"cmp"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"iter"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/helpers/deps"
	"github.com/riverqueue/river"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media/layout"
)

// The media upgrade brings items stored before v0.68 to the current format in
// place: a version 2 manifest becomes version 3 under the commit journal,
// keeping its private blobs at their names and adopting its template-named
// public files as their uploads' current (legacy) publications, so nothing is
// copied or rendered again and their URLs keep working until the worker next
// publishes a generation. It also projects every current item's
// publications. Until an item is upgraded nothing else edits or cleans it
// (ErrUpgradeRequired).

// FolderLister lists the item folders under a kind's prefix in key order,
// after one: delimiter listings, which media/s3.Store implements. Other
// stores are listed object by object.
type FolderLister interface {
	ListFolders(ctx context.Context, prefix, after string) iter.Seq2[string, error]
}

// UpgradeStatus is the media upgrade's progress.
type UpgradeStatus struct {
	Done  bool          `json:"done"` // every kind visited and no failures left
	Kinds []KindUpgrade `json:"kinds"`
	// Failures are items the upgrade could not read or convert (at most
	// 100); they stay as they were and are retried on every run.
	Failures []UpgradeFailure `json:"failures,omitempty"`
}

// KindUpgrade is one kind's pass over its folders.
type KindUpgrade struct {
	Namespace string     `json:"namespace"`
	Kind      string     `json:"kind"`
	After     string     `json:"after"`    // the last folder visited
	Visited   int64      `json:"visited"`  // folders visited
	Upgraded  int64      `json:"upgraded"` // version 2 manifests converted
	Failed    int64      `json:"failed"`   // items failing now
	Finished  *time.Time `json:"finished,omitempty"`
}

// UpgradeFailure is an item left as it was, and why.
type UpgradeFailure struct {
	Ref   contentref.ContentRef `json:"ref"`
	Error string                `json:"error"`
	At    time.Time             `json:"at"`
}

// errUnconvertible marks an item the upgrade cannot read or convert: recorded
// as a failure and left intact rather than retried at once.
type errUnconvertible struct{ err error }

func (e errUnconvertible) Error() string { return e.err.Error() }
func (e errUnconvertible) Unwrap() error { return e.err }

// itemFailure reports whether err, from upgrading one item, is that item's:
// recorded, and the pass goes on. A cancelled run or an unreachable store or
// database is the job's: it fails and retries.
func itemFailure(ctx context.Context, err error) bool {
	switch {
	case err == nil || ctx.Err() != nil:
		return false
	case errors.As(err, new(errUnconvertible)):
		return true
	}
	return !errors.Is(err, ErrUnavailable) && !errors.Is(err, ErrConditionalPutRequired) && !deps.PostgresUnavailable(err)
}

// Upgrade visits at most limit item folders where the last run stopped, then
// retries recorded failures, and returns the progress. It is idempotent and
// safe while the host serves; repeat it until Done. The River job
// contentkit_media_upgrade runs it on start until done.
func (j *Jobs) Upgrade(ctx context.Context, limit int) (UpgradeStatus, error) {
	if limit <= 0 {
		return UpgradeStatus{}, errors.New("media: upgrade needs a positive limit")
	}
	budget := limit
	for i := range j.cfg.Registry.cfg.Kinds {
		k := &j.cfg.Registry.cfg.Kinds[i]
		if budget == 0 {
			break
		}
		n, err := j.upgradeKind(ctx, k, budget)
		budget -= n
		if err != nil {
			return UpgradeStatus{}, err
		}
	}
	if budget > 0 {
		if err := j.retryUpgradeFailures(ctx, budget); err != nil {
			return UpgradeStatus{}, err
		}
	}
	return j.UpgradeStatus(ctx)
}

// upgradeKind continues k's pass and returns the folders it visited.
func (j *Jobs) upgradeKind(ctx context.Context, k *Kind, limit int) (int, error) {
	jr := j.cfg.Journal
	var after string
	var finished *time.Time
	err := jr.pool.QueryRow(ctx, `WITH ins AS (INSERT INTO `+jr.upgrades+` (tenant_id, content_kind) VALUES ($1, $2)
ON CONFLICT DO NOTHING RETURNING after_id, finished_at)
SELECT after_id, finished_at FROM ins UNION ALL SELECT after_id, finished_at FROM `+jr.upgrades+`
WHERE tenant_id = $1 AND content_kind = $2 LIMIT 1`, k.ns, k.Name).Scan(&after, &finished)
	if err != nil || finished != nil {
		return 0, err
	}
	visited, upgraded, last := 0, 0, after
	// advance moves the cursor past what was visited; a concurrent run that
	// moved it first wins, and this one's items are idempotent repeats.
	advance := func(finished bool) error {
		if last == after && !finished {
			return nil
		}
		_, err := jr.pool.Exec(ctx, `UPDATE `+jr.upgrades+` SET after_id = $4, visited = visited + $5, upgraded = upgraded + $6,
finished_at = CASE WHEN $7 THEN now() END, updated_at = now() WHERE tenant_id = $1 AND content_kind = $2 AND after_id = $3`,
			k.ns, k.Name, after, last, visited, upgraded, finished)
		return err
	}
	for id, err := range j.folders(ctx, k.ns+"/"+k.Name+"/", after) {
		if err != nil {
			return visited, errors.Join(err, advance(false))
		}
		if visited == limit {
			return visited, advance(false)
		}
		if id != layout.DefaultID && contentref.ValidateID(id) == nil {
			ref := contentref.New(k.ns, k.Name, id)
			outcome, err := j.manifests.upgradeItem(ctx, ref)
			if itemFailure(ctx, err) {
				outcome, err = upgradeNone, j.recordUpgradeFailure(ctx, ref, err)
			}
			if err != nil {
				return visited, errors.Join(err, advance(false))
			}
			if outcome == upgradeConverted {
				upgraded++
			}
		}
		visited++
		last = id
	}
	return visited, advance(true)
}

// folders lists the item folder names under root after one, in key order.
func (j *Jobs) folders(ctx context.Context, root, after string) iter.Seq2[string, error] {
	if l, ok := j.cfg.Store.(FolderLister); ok {
		return l.ListFolders(ctx, root, after)
	}
	return func(yield func(string, error) bool) {
		last := after
		for o, err := range j.cfg.Store.List(ctx, root) {
			if err != nil {
				yield("", err)
				return
			}
			id, _, ok := strings.Cut(strings.TrimPrefix(o.Key, root), "/")
			if !ok || id <= last {
				continue
			}
			last = id
			if !yield(id, nil) {
				return
			}
		}
	}
}

// recordUpgradeFailure logs a failure once, not on every retry that repeats it.
func (j *Jobs) recordUpgradeFailure(ctx context.Context, ref contentref.ContentRef, cause error) error {
	jr := j.cfg.Journal
	var prior *string
	err := jr.pool.QueryRow(ctx, `WITH prior AS (SELECT error FROM `+jr.upgradeFails+`
WHERE tenant_id = $1 AND content_kind = $2 AND content_id = $3),
f AS (INSERT INTO `+jr.upgradeFails+` (tenant_id, content_kind, content_id, error)
VALUES ($1, $2, $3, $4) ON CONFLICT (tenant_id, content_kind, content_id) DO UPDATE SET error = EXCLUDED.error, failed_at = now())
SELECT (SELECT error FROM prior)`, ref.TenantID, ref.ContentKind, ref.ContentID, cause.Error()).Scan(&prior)
	if err == nil && (prior == nil || *prior != cause.Error()) {
		j.cfg.Logger.WarnContext(ctx, "media: upgrade left an item as it was", "ref", ref.String(), "error", cause)
	}
	return err
}

// retryUpgradeFailures retries the oldest failures once their kind's pass ended.
func (j *Jobs) retryUpgradeFailures(ctx context.Context, limit int) error {
	jr := j.cfg.Journal
	rows, err := jr.pool.Query(ctx, `SELECT f.tenant_id, f.content_kind, f.content_id FROM `+jr.upgradeFails+` f
JOIN `+jr.upgrades+` u USING (tenant_id, content_kind) WHERE u.finished_at IS NOT NULL
AND f.tenant_id = ANY($1) ORDER BY f.failed_at LIMIT $2`, j.cfg.Registry.Namespaces(), limit)
	if err != nil {
		return err
	}
	refs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (contentref.ContentRef, error) {
		var ref contentref.ContentRef
		return ref, r.Scan(&ref.TenantID, &ref.ContentKind, &ref.ContentID)
	})
	if err != nil {
		return err
	}
	for _, ref := range refs {
		if _, err := j.cfg.Registry.Item(ref); err != nil {
			continue // another registry sharing this schema owns it
		}
		_, err := j.manifests.upgradeItem(ctx, ref)
		if itemFailure(ctx, err) {
			err = j.recordUpgradeFailure(ctx, ref, err)
		} else if err == nil {
			_, err = jr.pool.Exec(ctx, `DELETE FROM `+jr.upgradeFails+` WHERE tenant_id = $1 AND content_kind = $2 AND content_id = $3`,
				ref.TenantID, ref.ContentKind, ref.ContentID)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// UpgradeStatus reports the media upgrade's progress over the registry's kinds.
func (j *Jobs) UpgradeStatus(ctx context.Context) (UpgradeStatus, error) {
	jr := j.cfg.Journal
	st := UpgradeStatus{Done: true}
	for _, k := range j.cfg.Registry.cfg.Kinds {
		ku := KindUpgrade{Namespace: k.ns, Kind: k.Name}
		err := jr.pool.QueryRow(ctx, `SELECT u.after_id, u.visited, u.upgraded, u.finished_at,
(SELECT count(*) FROM `+jr.upgradeFails+` f WHERE f.tenant_id = u.tenant_id AND f.content_kind = u.content_kind)
FROM `+jr.upgrades+` u WHERE u.tenant_id = $1 AND u.content_kind = $2`, k.ns, k.Name).Scan(
			&ku.After, &ku.Visited, &ku.Upgraded, &ku.Finished, &ku.Failed)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return st, err
		}
		st.Done = st.Done && ku.Finished != nil && ku.Failed == 0
		st.Kinds = append(st.Kinds, ku)
	}
	rows, err := jr.pool.Query(ctx, `SELECT tenant_id, content_kind, content_id, error, failed_at FROM `+jr.upgradeFails+`
WHERE tenant_id = ANY($1) ORDER BY failed_at LIMIT 100`, j.cfg.Registry.Namespaces())
	if err != nil {
		return st, err
	}
	st.Failures, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (UpgradeFailure, error) {
		var f UpgradeFailure
		return f, r.Scan(&f.Ref.TenantID, &f.Ref.ContentKind, &f.Ref.ContentID, &f.Error, &f.At)
	})
	return st, err
}

func (st UpgradeStatus) failed() int64 {
	var n int64
	for _, k := range st.Kinds {
		n += k.Failed
	}
	return n
}

type upgradeOutcome int

const (
	upgradeNone      upgradeOutcome = iota // no manifest
	upgradeCurrent                         // already current: projected
	upgradeConverted                       // a version 2 manifest rewritten as version 3
)

// upgradeItem converts ref's version 2 manifest, or projects a current one.
// A crash leaves the root either version 2 or version 3 with a receipt that
// recovery settles; a rerun converges.
func (m *Manifests) upgradeItem(ctx context.Context, ref contentref.ContentRef) (upgradeOutcome, error) {
	item, err := m.reg.Item(ref)
	if err != nil {
		return upgradeNone, err
	}
	if !m.store.Capabilities().ConditionalPut {
		return upgradeNone, ErrConditionalPutRequired
	}
	unlock, err := m.locker.Lock(ctx, item.ManifestKey())
	if err != nil {
		return upgradeNone, err
	}
	defer unlock()
	if _, _, err := m.recoverLocked(ctx, item); err != nil {
		return upgradeNone, err
	}
	stored, etag, err := m.readStored(ctx, item)
	switch {
	case errors.Is(err, ErrNotFound):
		return upgradeNone, nil
	case errors.Is(err, ErrManifestUnreadable), errors.As(err, new(errUnconvertible)):
		return upgradeNone, errUnconvertible{err}
	case err != nil:
		return upgradeNone, err
	}
	switch stored.V {
	case ManifestVersion:
		// Recovery settled every attempt and the lock holds off the next, so
		// this manifest is what the projection must say.
		if err := m.journal.checkReceipt(ctx, item, stored.Receipt); errors.Is(err, ErrCommitPending) || errors.Is(err, ErrCommitIdentity) {
			return upgradeNone, errUnconvertible{err} // written under another journal schema
		} else if err != nil {
			return upgradeNone, err
		}
		return upgradeCurrent, m.journal.project(ctx, projectionOf(item, stored))
	case legacyManifestVersion:
	default:
		return upgradeNone, errUnconvertible{fmt.Errorf("media: manifest version %d cannot be upgraded", stored.V)}
	}
	next, effects, err := m.convert(ctx, item, stored)
	if err != nil {
		return upgradeNone, err
	}
	c, err := m.journal.begin(ctx, manifestCommit{ID: uuid.New(), Ref: ref, Folder: item.Prefix(),
		Fingerprint: sha256.Sum256([]byte("internal-media-upgrade"))})
	if err != nil {
		return upgradeNone, err
	}
	c.ETag = etag
	if err := m.journal.prepare(ctx, &c, effects, 0); err != nil {
		if c.State == "open" {
			err = errors.Join(err, m.journal.finish(context.WithoutCancel(ctx), c, false, nil))
		}
		if errors.Is(err, ErrAllocationRetired) {
			err = errUnconvertible{err}
		}
		return upgradeNone, err
	}
	next.Receipt = c.receipt()
	body, err := encodeManifest(next)
	if err != nil {
		return upgradeNone, err // prepared, never sent: recovery fences and releases it
	}
	obj, err := m.store.Put(ctx, item.ManifestKey(), bytes.NewReader(body), int64(len(body)), PutOptions{
		ContentType: "application/gzip", CacheControl: "no-store", IfMatch: etag,
		Metadata: map[string]string{uploadBytesMeta: strconv.FormatInt(next.uploadBytes(), 10)}})
	if errors.Is(err, ErrPreconditionFailed) {
		m.cache.remove(item.ManifestKey())
		if _, _, err := m.recoverAttemptLocked(ctx, item, &c.ID); err != nil {
			return upgradeNone, err
		}
		return upgradeNone, ErrManifestConflict // changed meanwhile: the next run converts it
	} else if err != nil {
		return upgradeNone, err
	}
	next.reindex()
	m.cache.put(item.ManifestKey(), obj.ETag, next)
	settleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	if err := m.finishCommit(settleCtx, item, c, true, next); err != nil {
		return upgradeConverted, err
	}
	if m.sweeps != nil {
		_ = m.sweeps.ScheduleSweep(ctx, ref) // the periodic pass backs it up
	}
	return upgradeConverted, nil
}

// readStored reads ref's manifest object as stored, of any version.
func (m *Manifests) readStored(ctx context.Context, item Item) (*Manifest, string, error) {
	rc, obj, err := m.store.Get(ctx, item.ManifestKey(), GetOptions{})
	if err != nil {
		return nil, "", err
	}
	defer rc.Close()
	body, err := io.ReadAll(io.LimitReader(rc, MaxManifestBytes+1))
	if err != nil {
		return nil, "", err
	}
	man, err := decodeStored(body)
	if err != nil && !errors.Is(err, ErrManifestUnreadable) {
		err = errUnconvertible{fmt.Errorf("media: decode manifest %s: %w", item.ManifestKey(), err)}
	}
	return man, obj.ETag, err
}

// convert is stored, a version 2 manifest, as version 3: its blobs stay where
// they are under a fresh incarnation, and each upload's template-named public
// files whose every rendition is in public/ become its current publication.
// Public files nothing adopts are retired with the write.
func (m *Manifests) convert(ctx context.Context, item Item, stored *Manifest) (*Manifest, journalEffects, error) {
	k := item.Kind()
	next := stored.Clone()
	next.V, next.Incarnation, next.Deleted, next.Receipt = ManifestVersion, uuid.NewString(), false, nil
	for i := range next.Files {
		next.Files[i].Public, next.Files[i].Editor = nil, nil
	}
	public := map[string]bool{}
	for obj, err := range m.store.List(ctx, item.PublicPrefix()) {
		if err != nil {
			return nil, journalEffects{}, err
		}
		public[obj.Key] = true
	}
	for _, f := range next.Files {
		if !f.IsUpload() || next.Hidden || f.Unattached || f.Fail() != nil || f.Source() == "" {
			continue
		}
		for _, p := range k.PublicFor(f.Path) {
			names := k.PublicNames(next, p, f.Path)
			// v0.67 kept a preview only once rendered at its position.
			if len(names) == 0 || p.First > 0 && slices.Contains(f.Pending, p.Name) {
				continue
			}
			pub, ok, err := m.legacyPublication(ctx, item, f, p, names, public)
			if err != nil {
				return nil, journalEffects{}, err
			}
			if ok {
				next.SetPublication(f.Path, pub)
			}
		}
	}
	k.Normalize(next)
	if err := next.Validate(); err != nil {
		return nil, journalEffects{}, errUnconvertible{err}
	}
	effects := journalEffects{Incarnation: next.Incarnation}
	for _, name := range next.Blobs() {
		key, _ := item.Blob(name)
		effects.Adopt = append(effects.Adopt, key)
	}
	for _, name := range next.StagedNames() {
		key, _ := item.Staged(name)
		effects.Adopt = append(effects.Adopt, key)
	}
	// v0.67 blobs are content-addressed: files with equal bytes share one.
	slices.Sort(effects.Adopt)
	effects.Adopt = slices.Compact(effects.Adopt)
	kept := k.PublicKept(next)
	for key := range public {
		if !slices.Contains(kept, strings.TrimPrefix(key, item.PublicPrefix())) {
			effects.Public = append(effects.Public, key)
		}
	}
	slices.Sort(effects.Public)
	if m.journal.queue != nil && slices.ContainsFunc(next.Files, func(f File) bool { return len(f.Pending) > 0 }) {
		effects.Process = &ProcessJob{Ref: item.Ref(), Place: len(next.StagedNames()) > 0}
	}
	return next, effects, nil
}

// legacyPublication adopts f's files of p at their logical names: their
// encoded sizes from the WebP headers, their fingerprint from the metadata
// the v0.67 worker wrote, so an unchanged preset is not rendered again.
func (m *Manifests) legacyPublication(ctx context.Context, item Item, f File, p *Public, names []string, public map[string]bool) (Publication, bool, error) {
	pub := Publication{Preset: p.Name, Source: f.Key(), Names: names, State: PublicationReady, Dims: make([]Dims, len(names))}
	for i, name := range names {
		key, err := item.Public(name)
		if err != nil || !public[key] {
			return pub, false, nil
		}
		rc, obj, err := m.store.Get(ctx, key, GetOptions{Range: "bytes=0-29"})
		if errors.Is(err, ErrNotFound) {
			return pub, false, nil
		} else if err != nil {
			return pub, false, err
		}
		head, err := io.ReadAll(io.LimitReader(rc, 30))
		rc.Close()
		if err != nil {
			return pub, false, err
		}
		w, h, ok := webpSize(head)
		if !ok {
			return pub, false, nil // the worker renders it
		}
		pub.Dims[i] = Dims{W: w, H: h}
		pub.FP = cmp.Or(pub.FP, obj.Metadata["fp"])
	}
	pub.FP = cmp.Or(pub.FP, "legacy")
	return pub, true, nil
}

// webpSize reads a WebP's canvas size from its first 30 bytes.
func webpSize(b []byte) (int, int, bool) {
	if len(b) < 30 || string(b[:4]) != "RIFF" || string(b[8:12]) != "WEBP" {
		return 0, 0, false
	}
	le24 := func(b []byte) int { return int(b[0]) | int(b[1])<<8 | int(b[2])<<16 }
	switch string(b[12:16]) {
	case "VP8 ":
		if b[23] != 0x9d || b[24] != 0x01 || b[25] != 0x2a {
			return 0, 0, false
		}
		return int(binary.LittleEndian.Uint16(b[26:]) & 0x3fff), int(binary.LittleEndian.Uint16(b[28:]) & 0x3fff), true
	case "VP8L":
		if b[20] != 0x2f {
			return 0, 0, false
		}
		bits := binary.LittleEndian.Uint32(b[21:])
		return int(bits&0x3fff) + 1, int(bits>>14&0x3fff) + 1, true
	case "VP8X":
		return le24(b[24:]) + 1, le24(b[27:]) + 1, true
	}
	return 0, 0, false
}

// fenceLegacy changes a version 2 root's ETag without changing its content: a
// fresh gzip nonce over the same JSON. A delayed upgrade PUT conditioned on
// the old ETag can then never land.
func (m *Manifests) fenceLegacy(ctx context.Context, item Item) error {
	rc, obj, err := m.store.Get(ctx, item.ManifestKey(), GetOptions{})
	if err != nil {
		return err
	}
	raw, err := io.ReadAll(io.LimitReader(rc, MaxManifestBytes+1))
	rc.Close()
	if err != nil {
		return err
	}
	if man, err := decodeStored(raw); err != nil || man.V != legacyManifestVersion {
		return ErrPreconditionFailed // replaced meanwhile: recovery reads it again
	}
	plain := raw
	if len(raw) >= 2 && raw[0] == 0x1f && raw[1] == 0x8b {
		zr, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			return err
		}
		if plain, err = io.ReadAll(io.LimitReader(zr, MaxManifestBytes+1)); err != nil {
			return err
		}
	}
	var b bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&b, gzip.BestSpeed)
	nonce := uuid.New()
	zw.Extra = nonce[:]
	if _, err := zw.Write(plain); err != nil {
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}
	_, err = m.store.Put(ctx, item.ManifestKey(), bytes.NewReader(b.Bytes()), int64(b.Len()), PutOptions{
		IfMatch: obj.ETag, ContentType: "application/gzip", CacheControl: "no-store", Metadata: obj.Metadata})
	m.cache.remove(item.ManifestKey())
	return err
}

type upgradeArgs struct {
	After int64 `json:"after,omitempty"` // the running batch this one follows
}

func (upgradeArgs) Kind() string                      { return "contentkit_media_upgrade" }
func (a upgradeArgs) FollowUp(id int64) river.JobArgs { a.After = id; return a }

type upgradeWorker struct {
	river.WorkerDefaults[upgradeArgs]
	j *Jobs
}

func (w *upgradeWorker) Timeout(*river.Job[upgradeArgs]) time.Duration { return 15 * time.Minute }

// Work runs one batch and queues the next until the upgrade is done.
func (w *upgradeWorker) Work(ctx context.Context, job *river.Job[upgradeArgs]) (err error) {
	defer func() { err = SnoozeUnavailable(ctx, w.j.cfg.Store, job.JobRow, err) }()
	if err := w.j.waitFor(ctx, job.Args.After); err != nil {
		return err
	}
	before, err := w.j.UpgradeStatus(ctx)
	if err != nil || before.Done {
		return err
	}
	st, err := w.j.Upgrade(ctx, upgradeBatch)
	if err != nil {
		return err
	}
	w.j.cfg.Logger.InfoContext(ctx, "media: upgrade batch", "done", st.Done, "failures", st.failed())
	// Go on while a pass is unfinished or retries clear failures; failures
	// left are retried by the next periodic run.
	if st.Done || !slices.ContainsFunc(st.Kinds, func(k KindUpgrade) bool { return k.Finished == nil }) && st.failed() >= before.failed() {
		return nil
	}
	return InsertOnce(ctx, w.j.Insert, upgradeArgs{}, river.InsertOpts{})
}

// upgradeBatch is the folders one upgrade job visits.
const upgradeBatch = 200
