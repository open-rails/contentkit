package media

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
)

// SyncPublic makes public/ match the manifest: every exposed rendition (the
// slot and inline outputs of an item that is not hidden) is copied from
// private/ under the same name, and a hidden item's public/ is emptied at
// once. It returns the deleted keys. A visible item's unlisted copies (older
// outputs) are left to the sweep, so pages rendered a moment ago still load.
func (m *Manifests) SyncPublic(ctx context.Context, ref contentref.ContentRef) ([]string, error) {
	item, err := m.kinds.Item(ref.Content())
	if err != nil {
		return nil, err
	}
	root, _, err := m.root(ctx, item.ManifestKey())
	if errors.Is(err, ErrNotFound) {
		root = &Root{Hidden: true}
	} else if err != nil {
		return nil, err
	}
	want := root.PublicNames()
	have := map[string]bool{}
	var removed []string
	for o, err := range m.store.List(ctx, item.PublicPrefix()) {
		if err != nil {
			return removed, err
		}
		name := strings.TrimPrefix(o.Key, item.PublicPrefix())
		if slices.Contains(want, name) {
			have[name] = true
			continue
		}
		if root.Hidden {
			if err := m.store.Delete(ctx, o.Key); err != nil && !errors.Is(err, ErrNotFound) {
				return removed, err
			}
			removed = append(removed, o.Key)
		}
	}
	for _, name := range want {
		if have[name] {
			continue
		}
		src, _ := item.Private(name)
		dst, _ := item.Public(name)
		if _, err := m.store.Copy(ctx, src, dst, CopyOptions{}); err != nil && !errors.Is(err, ErrNotFound) {
			return removed, err
		}
	}
	return removed, nil
}

var errNoManifest = errors.New("media: no manifest")

// Expose brings an item's public/ copies to its visibility: it resolves the
// item for an anonymous actor (JobsConfig.Resolver), records Hidden when
// anonymous viewers cannot see it, and syncs public/ (SyncPublic): a hidden
// item's copies are deleted at once and reported to Hooks.PublicRemoved; an
// unhidden item's are copied back. It re-resolves after writing and repeats
// until the state holds, so an Expose racing a visibility change ends at the
// newer one. An item without a manifest is left alone.
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
		if len(removed) > 0 && j.cfg.Hooks.PublicRemoved != nil {
			j.cfg.Hooks.PublicRemoved(ctx, ref, removed)
		}
		if err != nil {
			return err
		}
		now, err := j.hidden(ctx, ref)
		if err != nil {
			return err
		}
		if now == hidden {
			return nil
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
