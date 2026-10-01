package media

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media/layout"
)

// SlotIndex is the index of public slot outputs (content_media_slots in the
// ContentKit schema): a row per registered slot whose renditions are in
// public/, so an unset, hidden or not yet encoded slot has none. The host's
// media jobs keep it (Jobs.IndexSlots); listings (Reader.SlotImages) and
// stable slot links (Reader.SlotLink) read it without bucket reads. Pass the
// same index to JobsConfig.Slots and ReaderOptions.Slots.
type SlotIndex struct {
	pool     *pgxpool.Pool
	table    string
	backfill string // content_media_slot_backfill
}

// NewSlotIndex reads and writes the index in ContentKit's schema.
func NewSlotIndex(pool *pgxpool.Pool, schema string) (*SlotIndex, error) {
	if pool == nil || schema == "" {
		return nil, errors.New("media: SlotIndex needs a pool and the ContentKit schema")
	}
	return &SlotIndex{pool: pool, table: pgx.Identifier{schema, "content_media_slots"}.Sanitize(),
		backfill: pgx.Identifier{schema, "content_media_slot_backfill"}.Sanitize()}, nil
}

// indexedSlot is one row: the slot's aspect and its public outputs by
// ascending width.
type indexedSlot struct {
	Aspect  Aspect
	Outputs []SlotRendition
}

func (s indexedSlot) equal(o indexedSlot) bool {
	a, _ := json.Marshal(s.Outputs)
	b, _ := json.Marshal(o.Outputs)
	return s.Aspect == o.Aspect && bytes.Equal(a, b)
}

// publicSlots are the item's registered slots with outputs in public/.
func publicSlots(item Item, root *Root) map[string]indexedSlot {
	out := map[string]indexedSlot{}
	if root == nil || root.Hidden {
		return out
	}
	for name, spec := range item.Kind().Slots {
		rec := root.Slots[name]
		if rec == nil || rec.Result == nil {
			continue
		}
		var outs []SlotRendition
		for _, o := range rec.Result.Outputs {
			if root.Private[o.Blob].Public {
				outs = append(outs, o)
			}
		}
		if len(outs) == 0 {
			continue
		}
		aspect := spec.Aspect
		if spec.Native() {
			aspect = AspectOf(outs[len(outs)-1].W, outs[len(outs)-1].H)
		}
		out[name] = indexedSlot{Aspect: aspect, Outputs: outs}
	}
	return out
}

// sync makes ref's rows want in one transaction, calling changed for every
// slot whose row it adds, replaces or removes; an error rolls everything back.
func (x *SlotIndex) sync(ctx context.Context, ref contentref.ContentRef, want map[string]indexedSlot,
	changed func(ctx context.Context, tx pgx.Tx, ref contentref.ContentRef, slot string, set bool) error) error {
	return pgx.BeginFunc(ctx, x.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT slot, aspect, outputs FROM `+x.table+`
			WHERE tenant_id = $1 AND content_kind = $2 AND content_id = $3 FOR UPDATE`,
			ref.TenantID, ref.ContentKind, ref.ContentID)
		if err != nil {
			return err
		}
		have := map[string]indexedSlot{}
		for rows.Next() {
			var slot, aspect string
			var outputs []byte
			if err := rows.Scan(&slot, &aspect, &outputs); err != nil {
				rows.Close()
				return err
			}
			s, err := decodeIndexed(aspect, outputs)
			if err != nil {
				rows.Close()
				return err
			}
			have[slot] = s
		}
		if err := rows.Err(); err != nil {
			return err
		}
		type change struct {
			slot string
			set  bool
		}
		var changes []change
		for slot, s := range want {
			if h, ok := have[slot]; ok && h.equal(s) {
				continue
			}
			outputs, err := json.Marshal(s.Outputs)
			if err != nil {
				return err
			}
			aspect, _ := s.Aspect.MarshalText()
			if _, err := tx.Exec(ctx, `INSERT INTO `+x.table+` (tenant_id, content_kind, content_id, slot, aspect, outputs)
				VALUES ($1, $2, $3, $4, $5, $6)
				ON CONFLICT (tenant_id, content_kind, content_id, slot)
				DO UPDATE SET aspect = EXCLUDED.aspect, outputs = EXCLUDED.outputs, updated_at = now()`,
				ref.TenantID, ref.ContentKind, ref.ContentID, slot, string(aspect), outputs); err != nil {
				return err
			}
			changes = append(changes, change{slot, true})
		}
		for slot := range have {
			if _, ok := want[slot]; ok {
				continue
			}
			if _, err := tx.Exec(ctx, `DELETE FROM `+x.table+`
				WHERE tenant_id = $1 AND content_kind = $2 AND content_id = $3 AND slot = $4`,
				ref.TenantID, ref.ContentKind, ref.ContentID, slot); err != nil {
				return err
			}
			changes = append(changes, change{slot, false})
		}
		if changed == nil {
			return nil
		}
		for _, c := range changes {
			if err := changed(ctx, tx, ref, c.slot, c.set); err != nil {
				return fmt.Errorf("media: Hooks.SlotChanged %s#%s: %w", ref, c.slot, err)
			}
		}
		return nil
	})
}

// deleteTx drops every row of the items, in the host's transaction.
func (x *SlotIndex) deleteTx(ctx context.Context, tx pgx.Tx, refs []contentref.ContentRef) error {
	for _, ref := range refs {
		if _, err := tx.Exec(ctx, `DELETE FROM `+x.table+` WHERE tenant_id = $1 AND content_kind = $2 AND content_id = $3`,
			ref.TenantID, ref.ContentKind, ref.ContentID); err != nil {
			return err
		}
	}
	return nil
}

// lookup reads the slot's rows of ids.
func (x *SlotIndex) lookup(ctx context.Context, tenant, kind, slot string, ids []string) (map[string]indexedSlot, error) {
	out := make(map[string]indexedSlot, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := x.pool.Query(ctx, `SELECT content_id, aspect, outputs FROM `+x.table+`
		WHERE tenant_id = $1 AND content_kind = $2 AND content_id = ANY($3) AND slot = $4`, tenant, kind, ids, slot)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, aspect string
		var outputs []byte
		if err := rows.Scan(&id, &aspect, &outputs); err != nil {
			return nil, err
		}
		s, err := decodeIndexed(aspect, outputs)
		if err != nil {
			return nil, err
		}
		out[id] = s
	}
	return out, rows.Err()
}

func decodeIndexed(aspect string, outputs []byte) (indexedSlot, error) {
	var s indexedSlot
	if err := s.Aspect.UnmarshalText([]byte(aspect)); err != nil {
		return s, fmt.Errorf("media: slot index aspect %q: %w", aspect, err)
	}
	if err := json.Unmarshal(outputs, &s.Outputs); err != nil {
		return s, fmt.Errorf("media: slot index outputs: %w", err)
	}
	return s, nil
}

// Picture is a listed slot image: URL is the output nearest a display width
// (the narrowest at least that wide, else the widest), W and H its size, and
// SrcSet every distinct output ("url 320w, …"). Every URL is immutable.
type Picture struct {
	URL    string `json:"url"`
	SrcSet string `json:"srcset"`
	W      int    `json:"w"`
	H      int    `json:"h"`
}

// picture builds an indexed slot's Picture for width.
func (s indexedSlot) picture(item Item, base string, width int) Picture {
	urls := OutputURLs{BaseURL: base}
	o := s.pick(width)
	p := Picture{URL: urls.url(item, o.Blob, true), W: o.W, H: o.H}
	set := make([]string, 0, len(s.Outputs))
	for i, o := range s.Outputs {
		if i > 0 && s.Outputs[i-1].Blob == o.Blob { // rungs capped at the edited width share one output
			continue
		}
		set = append(set, urls.url(item, o.Blob, true)+" "+strconv.Itoa(o.W)+"w")
	}
	p.SrcSet = strings.Join(set, ", ")
	return p
}

// pick is the narrowest output at least width wide, else the widest.
func (s indexedSlot) pick(width int) SlotRendition {
	for _, o := range s.Outputs {
		if o.W >= width {
			return o
		}
	}
	return s.Outputs[len(s.Outputs)-1]
}

// SlotIndexer schedules an item's slot index job after a change to its
// slots' public outputs: the host's *Jobs, or in the media worker a
// *HostQueue (ManifestOptions.Sweeps implements it).
type SlotIndexer interface {
	IndexSlots(ctx context.Context, ref contentref.ContentRef) error
}

// IndexSlots enqueues the slot index job for an item with registered slots:
// it brings the item's rows to its manifest and calls Hooks.SlotChanged for
// each row it changes. An equal job still waiting absorbs it; one running is
// followed by another. A no-op without JobsConfig.Slots.
func (j *Jobs) IndexSlots(ctx context.Context, ref contentref.ContentRef) error {
	if j.cfg.Slots == nil {
		return nil
	}
	item, err := j.cfg.Kinds.Item(ref.Content())
	if err != nil || len(item.Kind().Slots) == 0 {
		return err
	}
	return j.insertOnce(ctx, slotIndexArgs{Ref: ref.Content()}, river.InsertOpts{})
}

// reindex brings ref's slot index rows to its manifest under the folder lock,
// so reindexes of one item never interleave. Hooks.SlotChanged runs under that
// lock and must not edit the item's media.
func (j *Jobs) reindex(ctx context.Context, ref contentref.ContentRef) error {
	if j.cfg.Slots == nil {
		return nil
	}
	ref = ref.Content()
	item, err := j.cfg.Kinds.Item(ref)
	if err != nil || len(item.Kind().Slots) == 0 {
		return nil // an unregistered kind has no slots to index
	}
	unlock, err := j.cfg.Locker.Lock(ctx, item.ManifestKey())
	if err != nil {
		return err
	}
	defer unlock()
	root, _, err := j.manifests.Root(ctx, ref)
	if errors.Is(err, ErrNotFound) {
		root = nil
	} else if err != nil {
		return err
	}
	return j.cfg.Slots.sync(ctx, ref, publicSlots(item, root), j.cfg.Hooks.SlotChanged)
}

// reindexFolder is reindex for a swept folder.
func (j *Jobs) reindexFolder(ctx context.Context, prefix string) error {
	tenant, kind, id, err := parseFolder(prefix)
	if err != nil {
		return err
	}
	return j.reindex(ctx, contentref.New(tenant, kind, id))
}

type slotIndexArgs struct {
	Ref   contentref.ContentRef `json:"ref"`
	After int64                 `json:"after,omitempty"` // the running job this one follows
}

func (slotIndexArgs) Kind() string { return "contentkit_media_slot_index" }

func (a slotIndexArgs) FollowUp(id int64) river.JobArgs { a.After = id; return a }

type slotIndexWorker struct {
	river.WorkerDefaults[slotIndexArgs]
	j *Jobs
}

func (w *slotIndexWorker) Timeout(*river.Job[slotIndexArgs]) time.Duration { return 5 * time.Minute }

func (w *slotIndexWorker) Work(ctx context.Context, job *river.Job[slotIndexArgs]) (err error) {
	defer func() { err = SnoozeUnavailable(ctx, w.j.cfg.Store, job.JobRow, err) }()
	if _, err := w.j.cfg.Kinds.Item(job.Args.Ref); err != nil {
		return river.JobCancel(err)
	}
	if err := w.j.waitFor(ctx, job.Args.After); err != nil {
		return err
	}
	return w.j.reindex(ctx, job.Args.Ref)
}

// The backfill indexes the slots that predate the index (an upgrade), once
// per tenant: the host's media jobs enqueue it when they are bound to River
// while a tenant's backfill is incomplete (and hourly, as a backstop), as one
// unique job across replicas. It walks the tenant's folders of kinds with
// slots in key order, saving its place every backfillSave folders, so a retry
// resumes; a tenant done is recorded and never walked again.
const backfillSave = 100

// backfillState is the tenant's place: the last folder indexed, and done.
func (x *SlotIndex) backfillState(ctx context.Context, tenant string) (after string, done bool, err error) {
	var completed *time.Time
	err = x.pool.QueryRow(ctx, `SELECT after_folder, completed_at FROM `+x.backfill+` WHERE tenant_id = $1`, tenant).Scan(&after, &completed)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return after, completed != nil, err
}

// saveBackfill records the tenant's place, and its completion when done.
func (x *SlotIndex) saveBackfill(ctx context.Context, tenant, after string, done bool) error {
	_, err := x.pool.Exec(ctx, `INSERT INTO `+x.backfill+` (tenant_id, after_folder, completed_at)
		VALUES ($1, $2, CASE WHEN $3 THEN now() END)
		ON CONFLICT (tenant_id) DO UPDATE SET after_folder = EXCLUDED.after_folder, completed_at = EXCLUDED.completed_at`,
		tenant, after, done)
	return err
}

// backfillPending reports a tenant whose backfill has not completed.
func (x *SlotIndex) backfillPending(ctx context.Context, tenants []string) (bool, error) {
	if len(tenants) == 0 {
		return false, nil
	}
	var done int
	err := x.pool.QueryRow(ctx, `SELECT count(*) FROM `+x.backfill+` WHERE tenant_id = ANY($1) AND completed_at IS NOT NULL`, tenants).Scan(&done)
	return done < len(tenants), err
}

// scheduleBackfill enqueues the backfill while a tenant's is incomplete:
// when the jobs are bound to River. Best effort; the hourly job backs it up.
func (j *Jobs) scheduleBackfill(ctx context.Context) {
	if j.cfg.Slots == nil {
		return
	}
	pending, err := j.cfg.Slots.backfillPending(ctx, j.cfg.Tenants)
	if err == nil && pending {
		_, err = j.Insert(ctx, slotBackfillArgs{}, &river.InsertOpts{UniqueOpts: PendingOnce})
	}
	if err != nil {
		j.cfg.Logger.WarnContext(ctx, "media: schedule the slot index backfill", "error", err)
	}
}

// backfillSlots indexes the existing slots of every tenant whose backfill is
// incomplete (see backfillSave).
func (j *Jobs) backfillSlots(ctx context.Context) error {
	if j.cfg.Slots == nil {
		return nil
	}
	var kinds []string
	for name, k := range j.cfg.Kinds.kinds {
		if len(k.Slots) > 0 {
			kinds = append(kinds, name)
		}
	}
	slices.Sort(kinds)
	for _, tenant := range j.cfg.Tenants {
		after, done, err := j.cfg.Slots.backfillState(ctx, tenant)
		if err != nil {
			return err
		}
		if done {
			continue
		}
		n := 0
		for _, kind := range kinds {
			root := tenant + "/" + kind + "/"
			if after > root && !strings.HasPrefix(after, root) {
				continue // a later kind's folder: this one is done
			}
			for o, err := range j.cfg.Store.List(ctx, root) {
				if err != nil {
					return err
				}
				parts := strings.SplitN(o.Key, "/", 4)
				if len(parts) < 4 || parts[3] != layout.ManifestName {
					continue
				}
				folder, err := folderPrefix(parts[0], parts[1], parts[2])
				if err != nil || folder <= after {
					continue
				}
				if err := j.reindexFolder(ctx, folder); err != nil {
					return err
				}
				if after, n = folder, n+1; n%backfillSave == 0 {
					if err := j.cfg.Slots.saveBackfill(ctx, tenant, after, false); err != nil {
						return err
					}
				}
			}
		}
		if err := j.cfg.Slots.saveBackfill(ctx, tenant, after, true); err != nil {
			return err
		}
		j.cfg.Logger.InfoContext(ctx, "media: slot index backfilled", "tenant", tenant, "folders", n)
	}
	return nil
}

type slotBackfillArgs struct{}

func (slotBackfillArgs) Kind() string { return "contentkit_media_slot_backfill" }

type slotBackfillWorker struct {
	river.WorkerDefaults[slotBackfillArgs]
	j *Jobs
}

func (w *slotBackfillWorker) Timeout(*river.Job[slotBackfillArgs]) time.Duration {
	return 6 * time.Hour
}

func (w *slotBackfillWorker) Work(ctx context.Context, job *river.Job[slotBackfillArgs]) (err error) {
	defer func() { err = SnoozeUnavailable(ctx, w.j.cfg.Store, job.JobRow, err) }()
	return w.j.backfillSlots(ctx)
}
