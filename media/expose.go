package media

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media/layout"
)

// SyncPublic makes public/ match the manifest's PublicCopies: a missing copy
// is copied from private/, a fixed slot name holding another rendition is
// overwritten, and a hidden item's public/ is emptied at once, as are fixed
// names no slot exposes any more (a removed slot, a dropped rung). A visible
// item's other unlisted copies (older file renditions) are left to the sweep,
// so pages rendered a moment ago still load. It returns the public/ keys it
// deleted, overwrote or first wrote under a fixed name, for a CDN purge
// (Hooks.PurgePublic). The folder lock covers the visibility read and
// completed copies/deletions.
func (m *Manifests) SyncPublic(ctx context.Context, ref contentref.ContentRef) ([]string, error) {
	item, err := m.kinds.Item(ref.Content())
	if err != nil {
		return nil, err
	}
	unlock, err := m.locker.Lock(ctx, item.ManifestKey())
	if err != nil {
		return nil, err
	}
	defer unlock()
	root, _, err := m.root(ctx, item.ManifestKey())
	if errors.Is(err, ErrNotFound) {
		root = &Root{Hidden: true}
	} else if err != nil {
		return nil, err
	}
	want := root.PublicCopies()
	have := map[string]Object{}
	var changed []string
	for o, err := range m.store.List(ctx, item.PublicPrefix()) {
		if err != nil {
			return changed, err
		}
		name := strings.TrimPrefix(o.Key, item.PublicPrefix())
		if _, ok := want[name]; ok {
			have[name] = o
			continue
		}
		if root.Hidden || layout.ValidSlotFileName(name) {
			if err := m.store.Delete(ctx, o.Key); err != nil && !errors.Is(err, ErrNotFound) {
				return changed, err
			}
			changed = append(changed, o.Key)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(want)) {
		src, _ := item.Private(want[name])
		dst := item.PublicPrefix() + name
		fixed := layout.ValidSlotFileName(name)
		if cur, ok := have[name]; ok {
			if !fixed {
				continue // content-named: immutable
			}
			head, err := m.store.Head(ctx, src)
			if errors.Is(err, ErrNotFound) {
				continue
			} else if err != nil {
				return changed, err
			}
			if head.ETag == cur.ETag {
				continue
			}
		}
		if _, err := m.store.Copy(ctx, src, dst, CopyOptions{}); errors.Is(err, ErrNotFound) {
			continue
		} else if err != nil {
			return changed, err
		}
		if fixed {
			changed = append(changed, dst)
		}
	}
	return changed, nil
}

var errNoManifest = errors.New("media: no manifest")

// Expose brings an item's public/ copies to its visibility: it resolves the
// item for an anonymous actor (JobsConfig.Resolver), records Hidden when
// anonymous viewers cannot see it, and syncs public/ (SyncPublic): a hidden
// item's copies are deleted at once and reported to Hooks.PurgePublic; an
// unhidden item's are copied back, and the slot index follows (IndexSlots).
// It re-resolves after writing and repeats until the state holds, so an
// Expose racing a visibility change ends at the newer one. An item without a
// manifest is left alone.
func (j *Jobs) Expose(ctx context.Context, ref contentref.ContentRef) error {
	if j.cfg.Resolver == nil {
		return errors.New("media: Expose needs JobsConfig.Resolver")
	}
	ref = ref.Content()
	hidden, err := j.hidden(ctx, ref)
	if err != nil {
		return err
	}
	for range 4 {
		_, err := j.manifests.EditRoot(ctx, ref, func(r *Root) error {
			if r.Originals == nil {
				return errNoManifest
			}
			r.Hidden = hidden
			return nil
		})
		if errors.Is(err, errNoManifest) {
			return nil
		} else if err != nil {
			return err
		}
		removed, err := j.manifests.SyncPublic(ctx, ref)
		if len(removed) > 0 && j.cfg.Hooks.PurgePublic != nil {
			j.cfg.Hooks.PurgePublic(ctx, ref, removed)
		}
		if err != nil {
			return err
		}
		now, err := j.hidden(ctx, ref)
		if err != nil {
			return err
		}
		if now == hidden {
			return j.IndexSlots(ctx, ref)
		}
		hidden = now
	}
	return fmt.Errorf("media: visibility of %s kept changing", ref)
}

func (j *Jobs) hidden(ctx context.Context, ref contentref.ContentRef) (bool, error) {
	res, err := access.ResolveOne(ctx, j.cfg.Resolver, ref, access.Actor{Anonymous: true})
	if err != nil {
		return false, fmt.Errorf("media: resolve %s for exposure: %w", ref, err)
	}
	return !res.Visible, nil
}

// ExposeTx enqueues Expose for each item in the host's transaction. Call it
// whenever whether anonymous viewers may see an item changes: create (a
// draft), publish, unpublish, hide, soft delete, restore.
func (j *Jobs) ExposeTx(ctx context.Context, tx pgx.Tx, refs ...contentref.ContentRef) error {
	c, err := j.bound()
	if err != nil {
		return err
	}
	params := make([]river.InsertManyParams, 0, len(refs))
	for _, ref := range refs {
		if _, err := j.cfg.Kinds.Item(ref.Content()); err != nil {
			return err
		}
		// Not unique: River's uniqueness always covers running jobs, and a
		// running Expose may have resolved before this change. Expose is
		// idempotent, so a duplicate costs one listing.
		params = append(params, river.InsertManyParams{Args: exposeArgs{Ref: ref.Content()}, InsertOpts: j.opts(nil)})
	}
	if len(params) == 0 {
		return nil
	}
	_, err = c.InsertManyTx(ctx, tx, params)
	return err
}

type exposeArgs struct {
	Ref contentref.ContentRef `json:"ref"`
}

func (exposeArgs) Kind() string { return "contentkit_media_expose" }

type exposeWorker struct {
	river.WorkerDefaults[exposeArgs]
	j *Jobs
}

func (w *exposeWorker) Timeout(*river.Job[exposeArgs]) time.Duration { return 5 * time.Minute }

func (w *exposeWorker) Work(ctx context.Context, job *river.Job[exposeArgs]) (err error) {
	defer func() { err = SnoozeUnavailable(ctx, w.j.cfg.Store, job.JobRow, err) }()
	if _, err := w.j.cfg.Kinds.Item(job.Args.Ref); err != nil {
		return river.JobCancel(err)
	}
	return w.j.Expose(ctx, job.Args.Ref)
}
