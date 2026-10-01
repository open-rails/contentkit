package media

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media/layout"
)

// SweepResult reports one folder sweep. Wait > 0 is when the next
// unreferenced blob comes due; sweep again then.
type SweepResult struct {
	Deleted []string
	Wait    time.Duration
}

// Sweep collects garbage by manifest reference, with no other state:
//   - private/ blobs the manifest does not reference, once the blob is
//     older than the grace period (a job's outputs not recorded yet, editor
//     views). Later edits never hold it back. A blob written long ago and
//     dropped just now is old already, so a viewer mid-stream on a replaced
//     file may lose it within the grace period;
//   - public/ names no preset expects (a removed upload, a hidden item), at
//     once, purged;
//   - temp/ by age (JobsConfig.TempTTL), but for staged uploads the
//     manifest references.
//
// It holds the manifest lock through selection and deletion, and decides on
// a second listing, so a manifest written meanwhile keeps what it references.
func (j *Jobs) Sweep(ctx context.Context, ref contentref.ContentRef) (SweepResult, error) {
	item, err := j.cfg.Registry.Item(ref)
	if err != nil {
		return SweepResult{}, err
	}
	return j.sweep(ctx, item.Prefix())
}

// SweepAll sweeps every item folder of the registry's kinds: the periodic
// backstop for missed schedules and abandoned uploads.
func (j *Jobs) SweepAll(ctx context.Context) error {
	var errs []error
	for _, k := range j.cfg.Registry.cfg.Kinds {
		root := k.ns + "/" + k.Name + "/"
		last := ""
		for o, err := range j.cfg.Store.List(ctx, root) {
			if err != nil {
				return errors.Join(append(errs, err)...)
			}
			id, _, _ := strings.Cut(strings.TrimPrefix(o.Key, root), "/")
			if id == last || contentref.ValidateID(id) != nil {
				continue
			}
			last = id
			if _, err := j.sweep(ctx, root+id+"/"); err != nil {
				errs = append(errs, err)
			}
		}
	}
	if len(errs) > 10 {
		errs = append(errs[:10], fmt.Errorf("media: and %d more folders failed", len(errs)-10))
	}
	return errors.Join(errs...)
}

func (j *Jobs) sweep(ctx context.Context, prefix string) (SweepResult, error) {
	ns, kind, id, err := parseFolder(prefix)
	if err != nil {
		return SweepResult{}, err
	}
	item, err := j.cfg.Registry.Item(contentref.New(ns, kind, id))
	if err != nil {
		return SweepResult{}, err
	}
	unlock, err := j.cfg.Locker.Lock(ctx, item.ManifestKey())
	if err != nil {
		return SweepResult{}, err
	}
	defer unlock()
	objs, err := j.list(ctx, prefix)
	if err != nil {
		return SweepResult{}, err
	}
	man, etag := manifestOf(objs)
	now := j.cfg.Now()
	m := &Manifest{} // no manifest: uploads never committed, or a deleted item's leftovers
	if man.Key != "" {
		if m, err = j.readManifest(ctx, man.Key); errors.Is(err, ErrNotFound) {
			return SweepResult{Wait: time.Minute}, nil
		} else if err != nil {
			return SweepResult{}, err
		}
	}
	keep := j.keeps(item, m)
	var wait time.Duration // until the youngest-due unreferenced blob is a grace period old
	doomed := func(objs []Object) []string {
		var keys []string
		wait = 0
		for _, o := range objs {
			k, ok := layout.Parse(o.Key)
			if !ok || k.Area == layout.AreaManifest || keep[k.Area+"/"+k.Name] {
				continue
			}
			switch k.Area {
			case layout.AreaPublic:
				keys = append(keys, o.Key)
			case layout.AreaTemp:
				if !now.Before(o.LastModified.Add(j.cfg.TempTTL)) {
					keys = append(keys, o.Key)
				}
			case layout.AreaPrivate:
				if left := o.LastModified.Add(j.cfg.Grace).Sub(now); left <= 0 {
					keys = append(keys, o.Key)
				} else if wait == 0 || left < wait {
					wait = left + time.Second
				}
			}
		}
		return keys
	}
	if len(doomed(objs)) == 0 {
		return SweepResult{Wait: wait}, nil
	}
	// A manifest written since the listing may reference a doomed blob, and
	// an upload may have refreshed one: only the second listing decides.
	again, err := j.list(ctx, prefix)
	if err != nil {
		return SweepResult{}, err
	}
	if m2, etag2 := manifestOf(again); m2.Key != man.Key || etag2 != etag {
		return SweepResult{Wait: time.Minute}, nil
	}
	keys := doomed(again)
	if err := j.deleteKeys(ctx, keys); err != nil {
		return SweepResult{}, err
	}
	j.purge(ctx, slices.DeleteFunc(slices.Clone(keys), func(k string) bool { return !strings.Contains(k, "/"+layout.AreaPublic+"/") }))
	return SweepResult{Deleted: keys, Wait: wait}, nil
}

// keeps is what an item's manifest keeps, as "{area}/{name}": its blobs and
// staged uploads and, unless hidden, the public names its uploads' presets
// render.
func (j *Jobs) keeps(item Item, m *Manifest) map[string]bool {
	keep := map[string]bool{}
	for _, b := range m.Blobs() {
		keep[layout.AreaPrivate+"/"+b] = true
	}
	for _, s := range m.StagedNames() {
		keep[layout.AreaTemp+"/"+s] = true
	}
	for _, n := range item.Kind().PublicKept(m) {
		keep[layout.AreaPublic+"/"+n] = true
	}
	return keep
}

func manifestOf(objs []Object) (Object, string) {
	for _, o := range objs {
		if k, ok := layout.Parse(o.Key); ok && k.Area == layout.AreaManifest {
			return o, o.ETag
		}
	}
	return Object{}, ""
}

// errBadManifest: a manifest that was read but does not decode.
var errBadManifest = errors.New("media: manifest does not decode")

func (j *Jobs) readManifest(ctx context.Context, key string) (*Manifest, error) {
	rc, _, err := j.cfg.Store.Get(ctx, key, GetOptions{})
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	body, err := io.ReadAll(io.LimitReader(rc, MaxManifestBytes+1))
	if err != nil {
		return nil, err
	}
	m, err := decodeManifest(body)
	if err != nil {
		return nil, fmt.Errorf("%w %s: %w", errBadManifest, key, err)
	}
	return m, nil
}

func (j *Jobs) list(ctx context.Context, prefix string) ([]Object, error) {
	var objs []Object
	for o, err := range j.cfg.Store.List(ctx, prefix) {
		if err != nil {
			return nil, err
		}
		objs = append(objs, o)
	}
	return objs, nil
}

// deleteFolder removes every object under prefix: the manifest and public/
// first, so readers stop resolving the item and public URLs stop answering,
// before its blobs go. Removed public keys are purged.
func (j *Jobs) deleteFolder(ctx context.Context, prefix string) error {
	objs, err := j.list(ctx, prefix)
	if err != nil {
		return err
	}
	var first, public, rest []string
	for _, o := range objs {
		switch k, _ := layout.Parse(o.Key); k.Area {
		case layout.AreaManifest:
			first = append(first, o.Key)
		case layout.AreaPublic:
			first = append(first, o.Key)
			public = append(public, o.Key)
		default:
			rest = append(rest, o.Key)
		}
	}
	if err := j.deleteKeys(ctx, first); err != nil {
		return err
	}
	j.purge(ctx, public)
	return j.deleteKeys(ctx, rest)
}

func (j *Jobs) deleteKeys(ctx context.Context, keys []string) error {
	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(8)
	for _, key := range keys {
		g.Go(func() error {
			if err := j.cfg.Store.Delete(ctx, key); err != nil && !errors.Is(err, ErrNotFound) {
				return err
			}
			return nil
		})
	}
	return g.Wait()
}

// OrphanSweep configures SweepOrphans.
type OrphanSweep struct {
	Kind string
	// Exists reports which of ids the host still has (a batch of at most
	// 500). An id it omits is an orphan.
	Exists func(ctx context.Context, ids []string) (map[string]bool, error)
	// Grace skips folders with an object newer than this (default the Jobs
	// grace), so an item created meanwhile is never taken for an orphan.
	Grace time.Duration
	// Delete removes orphans; otherwise they are only reported.
	Delete bool
}

// OrphanFolder is a folder no host item owns. ValidID false: its id is not
// an item id, so no item can ever address it.
type OrphanFolder struct {
	Prefix  string
	ID      string
	ValidID bool
	Objects int
	Newest  time.Time
	Deleted bool
}

// OrphanReport lists a kind's orphans; Folders counts every folder seen.
type OrphanReport struct {
	Folders int
	Orphans []OrphanFolder
}

// SweepOrphans lists the kind's folders and reports, or with Delete removes,
// those the host says do not exist, once past the grace period. The host
// runs it: only it knows which items exist.
func (j *Jobs) SweepOrphans(ctx context.Context, s OrphanSweep) (OrphanReport, error) {
	var rep OrphanReport
	k, err := j.cfg.Registry.Kind(s.Kind)
	if err != nil || s.Exists == nil {
		return rep, errors.Join(err, errors.New("media: SweepOrphans needs a kind and an Exists check"))
	}
	if s.Grace <= 0 {
		s.Grace = j.cfg.Grace
	}
	cutoff := j.cfg.Now().Add(-s.Grace)
	folders := map[string]*OrphanFolder{}
	root := k.ns + "/" + k.Name + "/"
	for o, err := range j.cfg.Store.List(ctx, root) {
		if err != nil {
			return rep, err
		}
		id, _, ok := strings.Cut(strings.TrimPrefix(o.Key, root), "/")
		if !ok || id == "" || id == layout.DefaultID {
			continue
		}
		f := folders[id]
		if f == nil {
			f = &OrphanFolder{Prefix: root + id + "/", ID: id, ValidID: contentref.ValidateID(id) == nil}
			folders[id] = f
		}
		f.Objects++
		if o.LastModified.After(f.Newest) {
			f.Newest = o.LastModified
		}
	}
	rep.Folders = len(folders)
	ids := slices.Sorted(maps.Keys(folders))
	for start := 0; start < len(ids); start += 500 {
		batch := ids[start:min(start+500, len(ids))]
		var valid []string
		for _, id := range batch {
			if folders[id].ValidID {
				valid = append(valid, id)
			}
		}
		exists := map[string]bool{}
		if len(valid) > 0 {
			if exists, err = s.Exists(ctx, valid); err != nil {
				return rep, err
			}
		}
		for _, id := range batch {
			f := folders[id]
			if exists[id] || f.Newest.After(cutoff) {
				continue
			}
			if s.Delete {
				if err := j.deleteFolder(ctx, f.Prefix); err != nil {
					return rep, err
				}
				f.Deleted = true
			}
			rep.Orphans = append(rep.Orphans, *f)
		}
	}
	return rep, nil
}
