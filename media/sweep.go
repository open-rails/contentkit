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

	"github.com/google/uuid"
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

// Sweep collects garbage by manifest reference and allocation age:
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
// Selection and retirement share a recoverable journal lease. A cleaner
// that loses its folder lock cannot delete from a stale manifest snapshot.
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
	for _, k := range j.cfg.Registry.all {
		for ref, err := range j.itemFolders(ctx, k) {
			if err != nil {
				return errors.Join(append(errs, err)...)
			}
			item, err := j.cfg.Registry.Item(ref)
			if err == nil {
				_, err = j.sweep(ctx, item.Prefix())
			}
			if err != nil {
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
	item, err := j.cfg.Registry.folderItem(prefix)
	if err != nil {
		return SweepResult{}, err
	}
	var result SweepResult
	err = j.manifests.cleanup(ctx, item, false, func(m *Manifest, _ bool) (journalEffects, error) {
		now := j.cfg.Now()
		keep := j.keeps(item, m)
		protected, err := j.manifests.protectedAllocations(ctx, item, now, j.cfg.Grace)
		if err != nil {
			return journalEffects{}, err
		}
		var effects journalEffects
		for o, err := range j.cfg.Store.List(ctx, prefix) {
			if err != nil {
				return journalEffects{}, err
			}
			k, ok := layout.Parse(o.Key)
			if !ok || k.Folder() != prefix || k.Area == layout.AreaManifest || keep[k.Area+"/"+k.Name] {
				continue
			}
			switch k.Area {
			case layout.AreaPublic:
				effects.Public = append(effects.Public, o.Key)
			case layout.AreaTemp:
				if !now.Before(o.LastModified.Add(j.cfg.TempTTL)) {
					effects.Private = append(effects.Private, o.Key)
				}
			case layout.AreaPrivate, layout.AreaDownload:
				if until, ok := protected[o.Key]; ok {
					if left := until.Sub(now); result.Wait == 0 || left < result.Wait {
						result.Wait = left + time.Second
					}
					continue
				}
				if left := o.LastModified.Add(j.cfg.Grace).Sub(now); left <= 0 {
					effects.Private = append(effects.Private, o.Key)
				} else if result.Wait == 0 || left < result.Wait {
					result.Wait = left + time.Second
				}
			}
		}
		result.Deleted = append(slices.Clone(effects.Public), effects.Private...)
		return effects, nil
	})
	if err != nil {
		return SweepResult{}, err
	}
	return result, nil
}

// keeps is what an item's manifest keeps, as "{area}/{name}": its blobs and
// staged uploads and, unless hidden, the public names its uploads' presets
// render.
func (j *Jobs) keeps(item Item, m *Manifest) map[string]bool {
	keep := map[string]bool{layout.AreaPublic + "/" + layout.ErasureFenceName: true}
	for _, b := range m.Blobs() {
		if area, name, ok := layout.SplitBlobRef(b); ok {
			keep[area+"/"+name] = true
		}
	}
	for _, s := range m.StagedNames() {
		keep[layout.AreaTemp+"/"+s] = true
	}
	for _, n := range item.Kind().PublicKept(m) {
		keep[layout.AreaPublic+"/"+n] = true
	}
	return keep
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

// deleteFolder removes every object under prefix but other items' folders
// within it: the manifest and public/ first, so readers stop resolving the
// item and public URLs stop answering, before its blobs go. Removed public
// keys are purged.
func (j *Jobs) deleteFolder(ctx context.Context, prefix string) error {
	objs, err := j.list(ctx, prefix)
	if err != nil {
		return err
	}
	var first, public, rest []string
	for _, o := range objs {
		if o.Key == prefix+layout.AreaPublic+"/"+layout.ErasureFenceName {
			continue
		}
		k, ok := layout.Parse(o.Key)
		if ok && k.Folder() != prefix {
			continue
		}
		switch k.Area {
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
	// 500): item ids, and of a Versions kind its version ids too. An id it
	// omits is an orphan.
	Exists func(ctx context.Context, ids []string) (map[string]bool, error)
	// Grace skips folders with an object newer than this (default the Jobs
	// grace), so an item created meanwhile is never taken for an orphan.
	Grace time.Duration
	// Delete removes orphans; otherwise they are only reported.
	Delete bool
}

// OrphanFolder is a folder no host item owns: ID is its item's id, or its
// version's. ValidID false: its id is not an item id, so no item can ever
// address it.
type OrphanFolder struct {
	Prefix  string
	ID      string
	ref     contentref.ContentRef
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
	root := k.root() + k.Name + "/"
	for o, err := range j.cfg.Store.List(ctx, root) {
		if err != nil {
			return rep, err
		}
		prefix, id := "", ""
		ref := contentref.ContentRef{}
		if key, ok := layout.Parse(o.Key); ok && key.Kind == k.Name && key.ID != layout.DefaultID {
			prefix, id = key.Folder(), cmpOr(key.Version, key.ID)
			ref, _ = parseFolder(prefix)
		} else if first, _, ok := strings.Cut(strings.TrimPrefix(o.Key, root), "/"); ok && first != "" && first != layout.DefaultID {
			prefix, id = root+first+"/", first
		} else {
			continue
		}
		f := folders[prefix]
		if f == nil {
			_, err := j.cfg.Registry.folderItem(prefix)
			f = &OrphanFolder{Prefix: prefix, ID: id, ref: ref, ValidID: err == nil}
			folders[prefix] = f
		}
		f.Objects++
		if o.LastModified.After(f.Newest) {
			f.Newest = o.LastModified
		}
	}
	rep.Folders = len(folders)
	prefixes := slices.Sorted(maps.Keys(folders))
	for start := 0; start < len(prefixes); start += 500 {
		batch := prefixes[start:min(start+500, len(prefixes))]
		var valid []string
		for _, p := range batch {
			if folders[p].ValidID {
				valid = append(valid, folders[p].ID)
			}
		}
		exists := map[string]bool{}
		if len(valid) > 0 {
			if exists, err = s.Exists(ctx, valid); err != nil {
				return rep, err
			}
		}
		for _, p := range batch {
			f := folders[p]
			if f.ValidID && exists[f.ID] || f.Newest.After(cutoff) {
				continue
			}
			if s.Delete {
				if f.ValidID {
					item, _ := j.cfg.Registry.Item(f.ref)
					m, _, err := j.manifests.get(ctx, item.ManifestKey())
					if errors.Is(err, ErrUpgradeRequired) {
						continue // never deleted before the upgrade reads it
					} else if err != nil && !errors.Is(err, ErrNotFound) {
						return rep, err
					}
					incarnation := ""
					if err == nil {
						incarnation = m.Incarnation
					}
					if err := j.deleteItem(ctx, item, "", uuid.New(), incarnation, cutoff); errors.Is(err, errOrphanChanged) {
						continue
					} else if err != nil {
						return rep, err
					}
				} else if err := j.deleteFolder(ctx, f.Prefix); err != nil {
					return rep, err
				}
				f.Deleted = true
			}
			rep.Orphans = append(rep.Orphans, *f)
		}
	}
	return rep, nil
}
