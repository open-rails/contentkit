package media

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
)

// Expose brings an item's public files to its visibility, resolved for an
// anonymous viewer (Hooks.Resolver). Hiding deletes and purges public/,
// records Hidden, and deletes again what a pass wrote meanwhile; it needs no
// readable manifest, so a manifest over its bound still hides. Unhiding
// marks the public presets pending on their kept sources and asks the worker
// to render them. It re-resolves after writing and repeats until the state
// holds, so an Expose racing a visibility change ends at the newer one. An
// item without a manifest has no public files (its first commit resolves
// it).
func (j *Jobs) Expose(ctx context.Context, ref contentref.ContentRef) error {
	item, err := j.cfg.Registry.Item(ref)
	if err != nil {
		return err
	}
	if j.cfg.Registry.cfg.Hooks.Resolver == nil {
		return errors.New("media: Expose needs Hooks.Resolver")
	}
	hidden, err := j.hidden(ctx, ref)
	if err != nil {
		return err
	}
	for range 4 {
		if hidden {
			if err := j.deletePublic(ctx, item); err != nil {
				return err
			}
		}
		changed := false
		_, err := j.manifests.EditExisting(ctx, ref, func(m *Manifest) error {
			changed = m.Hidden != hidden
			setHidden(item.Kind(), m, hidden)
			return nil
		})
		switch {
		case errors.Is(err, ErrNotFound):
			return nil
		case hidden && errors.Is(err, ErrManifestTooLarge):
			// Nothing can process it either, so public/ stays empty.
			j.cfg.Logger.WarnContext(ctx, "media: hid an item whose manifest does not decode", "ref", ref.String(), "error", err)
			return nil
		case err != nil:
			return err
		}
		if hidden {
			if err := j.deletePublic(ctx, item); err != nil {
				return err
			}
		} else if changed && j.cfg.Processes != nil {
			if err := j.cfg.Processes.Enqueue(ctx, ProcessJob{Ref: ref}); err != nil {
				return err
			}
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

// setHidden records hidden: a hidden item's public presets are not pending
// (nothing renders them); a visible item's are, on every attached upload.
func setHidden(k *Kind, m *Manifest, hidden bool) {
	was := m.Hidden
	m.Hidden = hidden
	if was == hidden {
		return
	}
	for i := range m.Files {
		f := &m.Files[i]
		if !f.IsUpload() || f.Unattached {
			continue
		}
		pending := slices.DeleteFunc(slices.Clone(f.Pending), func(p string) bool { return k.public(p) != nil })
		if !hidden && f.Source() != "" {
			for _, p := range k.PublicFor(f.Path) {
				pending = append(pending, p.Name)
			}
		}
		if len(pending) == 0 {
			pending = nil
		}
		f.Pending = pending
	}
}

// deletePublic deletes the item's public files and purges them.
func (j *Jobs) deletePublic(ctx context.Context, item Item) error {
	objs, err := j.list(ctx, item.PublicPrefix())
	if err != nil {
		return err
	}
	keys := make([]string, len(objs))
	for i, o := range objs {
		keys[i] = o.Key
	}
	if err := j.deleteKeys(ctx, keys); err != nil {
		return err
	}
	j.purge(ctx, keys)
	return nil
}

func (j *Jobs) hidden(ctx context.Context, ref contentref.ContentRef) (bool, error) {
	res, err := access.ResolveOne(ctx, j.cfg.Registry.cfg.Hooks.Resolver, ref, access.Actor{Anonymous: true})
	if err != nil {
		return false, fmt.Errorf("media: resolve %s for exposure: %w", ref, err)
	}
	return !res.Visible, nil
}

// ExposeTx enqueues Expose for each item in the host's transaction. Call it
// whenever whether anonymous viewers may see an item changes: publish,
// unpublish, hide, hold, soft delete, restore.
func (j *Jobs) ExposeTx(ctx context.Context, tx pgx.Tx, refs ...contentref.ContentRef) error {
	c, err := j.bound()
	if err != nil {
		return err
	}
	return exposeTx(ctx, j.cfg.Registry, tx, c, j.cfg.Queue, refs)
}

func exposeTx(ctx context.Context, reg *Registry, tx pgx.Tx, c *river.Client[pgx.Tx], queue string, refs []contentref.ContentRef) error {
	params := make([]river.InsertManyParams, 0, len(refs))
	for _, ref := range refs {
		if _, err := reg.Item(ref); err != nil {
			return err
		}
		// Not unique: River's uniqueness always covers running jobs, and a
		// running Expose may have resolved before this change. Expose is
		// idempotent, so a duplicate costs one resolve.
		params = append(params, river.InsertManyParams{Args: exposeArgs{Ref: ref}, InsertOpts: &river.InsertOpts{Queue: queue}})
	}
	if len(params) == 0 {
		return nil
	}
	_, err := c.InsertManyTx(ctx, tx, params)
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
	if _, err := w.j.cfg.Registry.Item(job.Args.Ref); err != nil {
		return river.JobCancel(err)
	}
	return w.j.Expose(ctx, job.Args.Ref)
}
