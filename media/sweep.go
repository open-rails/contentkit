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

// Sweep retires expired allocations recorded before storage writes. Unknown
// bucket objects are never discovered or deleted by this operation.
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
	item, err := j.itemForFolder(ctx, prefix)
	if err != nil {
		return SweepResult{}, err
	}
	if err := j.expireStaged(ctx, item); err != nil {
		return SweepResult{}, err
	}
	var result SweepResult
	err = j.manifests.cleanup(ctx, item, false, func(m *Manifest, _ bool) (journalEffects, error) {
		now := j.cfg.Now()
		keep := j.keeps(item, m)
		protected, err := j.manifests.allocationDeadlines(ctx, item, j.cfg.Grace)
		if err != nil {
			return journalEffects{}, err
		}
		var effects journalEffects
		// Only previously allocated keys are eligible. Unknown S3 objects are
		// an integrity failure, never inputs to routine cleanup.
		for key, until := range protected {
			parsed, ok := layout.Parse(key)
			if !ok || keep[parsed.Area+"/"+parsed.Name] {
				continue
			}
			if left := until.Sub(now); left > 0 {
				if result.Wait == 0 || left < result.Wait {
					result.Wait = left + time.Second
				}
				continue
			}
			if parsed.Area == layout.AreaPublic {
				effects.Public = append(effects.Public, key)
			} else {
				effects.Private = append(effects.Private, key)
			}
		}
		for _, key := range effects.Private {
			if _, live := protected[key]; live {
				// One bounded follow-up catches a PUT that was already issued
				// when this allocation was retired, including absent objects.
				effects.CleanupAt = now.Add(j.cfg.LateUploadWindow)
				effects.CleanupDelete = true
				break
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

// expireStaged drops unusable upload references at their known deadline.
// A user must upload again after temporary storage expires.
func (j *Jobs) expireStaged(ctx context.Context, item Item) error {
	deadlines, err := j.manifests.allocationDeadlines(ctx, item, j.cfg.Grace)
	if err != nil {
		return err
	}
	needsExpiry := false
	for key, deadline := range deadlines {
		if strings.HasPrefix(key, item.TempPrefix()) && !deadline.After(j.cfg.Now()) {
			needsExpiry = true
			break
		}
	}
	if !needsExpiry {
		return nil
	}
	var expired []string
	_, err = j.manifests.EditExisting(ctx, item.Ref(), func(m *Manifest) error {
		m.Files = slices.DeleteFunc(m.Files, func(f File) bool {
			if f.Staged == "" {
				return false
			}
			key, err := item.Staged(f.Staged)
			deadline, exists := deadlines[key]
			if err == nil && exists && !deadline.After(j.cfg.Now()) {
				expired = append(expired, f.Path)
				return true
			}
			return false
		})
		return nil
	})
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err == nil && len(expired) > 0 {
		j.cfg.Logger.WarnContext(ctx, "media: temporary uploads expired; upload again", "ref", item.Ref(), "paths", expired)
	}
	return err
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
// item and public URLs stop answering, before its blobs go.
func (j *Jobs) deleteFolder(ctx context.Context, prefix string) error {
	objs, err := j.list(ctx, prefix)
	if err != nil {
		return err
	}
	var first, rest []string
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
		default:
			rest = append(rest, o.Key)
		}
	}
	if err := j.deleteKeys(ctx, first); err != nil {
		return err
	}
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
			item, err := j.itemForFolder(ctx, prefix)
			if err == nil {
				ref = item.Ref()
			}
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
				if k.Versions && !f.ValidID {
					return rep, fmt.Errorf("media: cannot delete version folder without a canonical journal owner: %s", f.Prefix)
				}
				if f.ValidID {
					item, err := j.itemForFolder(ctx, f.Prefix)
					if err != nil {
						return rep, err
					}
					m, _, err := j.manifests.get(ctx, item.ManifestKey())
					if errors.Is(err, ErrUpgradeRequired) {
						continue // preserve old formats for migration with the previous release
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
