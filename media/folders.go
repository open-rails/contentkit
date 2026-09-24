package media

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media/layout"
)

// ErrFolderNotEmpty: a new item's folder already holds objects. Content ids
// are never reused, so leftovers mean a host bug (a reset id, a restored
// database); they are never adopted. Purge them deliberately with Jobs.Purge.
var ErrFolderNotEmpty = errors.New("media: new item's folder is not empty")

// FolderNotEmptyError names the folder and a few of its keys.
type FolderNotEmptyError struct {
	Prefix string
	Keys   []string
}

func (e *FolderNotEmptyError) Error() string {
	return fmt.Sprintf("%v: %s holds %s", ErrFolderNotEmpty, e.Prefix, strings.Join(e.Keys, ", "))
}
func (e *FolderNotEmptyError) Unwrap() error { return ErrFolderNotEmpty }

// Create starts a new item: it writes the item's empty manifest, and fails
// with ErrFolderNotEmpty if the folder already holds any object (a manifest,
// an upload, an output). Hosts call it when they create the item's row, so a
// reused id surfaces there instead of showing another item's files.
func (m *Manifests) Create(ctx context.Context, ref contentref.ContentRef) (*Manifest, error) {
	item, err := m.kinds.Item(ref)
	if err != nil {
		return nil, err
	}
	key, err := item.ManifestKey()
	if err != nil {
		return nil, err
	}
	if err := m.requireEmpty(ctx, item.Prefix()); err != nil {
		return nil, err
	}
	man := &Manifest{Files: []File{}}
	body := []byte(`{"files":[]}`)
	obj, err := m.store.Put(ctx, key, bytes.NewReader(body), int64(len(body)), PutOptions{ContentType: "application/json", CacheControl: "no-store", IfNoneMatch: "*"})
	if errors.Is(err, ErrPreconditionFailed) {
		return nil, &FolderNotEmptyError{Prefix: item.Prefix(), Keys: []string{key}}
	}
	if err != nil {
		return nil, err
	}
	m.cache.put(key, obj.ETag, body)
	return man, nil
}

// requireFresh backs Edit's first manifest of a folder: derived blobs with no
// manifest are a previous item's leftovers (uploads and slots may precede a
// first commit; blobs never do).
func (m *Manifests) requireFresh(ctx context.Context, item Item) error {
	var blobs []string
	for o, err := range m.store.List(ctx, item.Prefix()) {
		if err != nil {
			return err
		}
		k, ok := layout.Parse(o.Key)
		if ok && k.Area == AreaManifest {
			return nil
		}
		if ok && k.Area == AreaBlobs && len(blobs) < 3 {
			blobs = append(blobs, o.Key)
		}
	}
	if len(blobs) > 0 {
		return &FolderNotEmptyError{Prefix: item.Prefix(), Keys: blobs}
	}
	return nil
}

func (m *Manifests) requireEmpty(ctx context.Context, prefix string) error {
	var keys []string
	for o, err := range m.store.List(ctx, prefix) {
		if err != nil {
			return err
		}
		if keys = append(keys, o.Key); len(keys) == 3 {
			break
		}
	}
	if len(keys) > 0 {
		return &FolderNotEmptyError{Prefix: prefix, Keys: keys}
	}
	return nil
}

// Purge deletes an item's whole folder (every version) now: the explicit
// reset before deliberately recreating an item, or an operator cleanup. It
// releases the owner's quota for the folder's manifests when Owner is set.
// Hosts deleting content use DeleteItemsTx.
func (j *Jobs) Purge(ctx context.Context, d Deletion) error {
	if d.Ref.Version() != "" {
		return fmt.Errorf("media: purge %s: folders hold every version; pass the work ref", d.Ref)
	}
	prefix, err := folderPrefix(d.Ref.TenantID, d.Ref.ContentKind, d.Ref.ContentID)
	if err != nil {
		return err
	}
	var release int64
	if j.cfg.Limiter != nil && d.Owner != "" {
		if release, err = j.originalBytes(ctx, prefix); err != nil {
			return err
		}
	}
	if err := j.deleteFolder(ctx, prefix); err != nil {
		return err
	}
	if release > 0 {
		return j.cfg.Limiter.Settle(ctx, Settlement{Tenant: d.Ref.TenantID, Owner: d.Owner, Delta: -release})
	}
	return nil
}

// OrphanSweep configures SweepOrphans.
type OrphanSweep struct {
	Tenant, Kind string
	// Exists reports which of ids the host still has (a batch of at most
	// 500). An id it omits is an orphan.
	Exists func(ctx context.Context, ids []string) (map[string]bool, error)
	// Grace skips folders with an object newer than this (default the Jobs
	// grace), so an item created meanwhile is never taken for an orphan.
	Grace time.Duration
	// Delete removes orphans; otherwise they are only reported.
	Delete bool
}

// OrphanFolder is a folder no host item owns. ValidID false: its id is not a
// content id (e.g. a legacy integer), so no item can ever address it.
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

const orphanBatch = 500

// SweepOrphans lists the kind's folders in a tenant and reports, or with
// Delete removes, those the host says do not exist, once past the grace
// period. The host runs it (a command or a periodic job): only it knows
// which items exist.
func (j *Jobs) SweepOrphans(ctx context.Context, s OrphanSweep) (OrphanReport, error) {
	var rep OrphanReport
	if s.Exists == nil || !layout.ValidSegment(s.Tenant) {
		return rep, errors.New("media: SweepOrphans needs a tenant and an Exists check")
	}
	if _, err := j.cfg.Kinds.Kind(s.Kind); err != nil {
		return rep, err
	}
	if s.Grace <= 0 {
		s.Grace = j.cfg.Grace
	}
	cutoff := j.cfg.Now().Add(-s.Grace)
	var batch []OrphanFolder
	settle := func(folders []OrphanFolder) error {
		var ids []string
		for _, f := range folders {
			if f.ValidID {
				ids = append(ids, f.ID)
			}
		}
		exists := map[string]bool{}
		if len(ids) > 0 {
			var err error
			if exists, err = s.Exists(ctx, ids); err != nil {
				return err
			}
		}
		for _, f := range folders {
			if exists[f.ID] {
				continue
			}
			if s.Delete {
				if err := j.deleteFolder(ctx, f.Prefix); err != nil {
					return err
				}
				f.Deleted = true
			}
			rep.Orphans = append(rep.Orphans, f)
		}
		return nil
	}
	var cur *OrphanFolder
	flush := func() error {
		if cur == nil {
			return nil
		}
		f := *cur
		cur = nil
		rep.Folders++
		if f.Newest.After(cutoff) {
			return nil
		}
		if batch = append(batch, f); len(batch) < orphanBatch {
			return nil
		}
		err := settle(batch)
		batch = batch[:0]
		return err
	}
	root := s.Tenant + "/" + s.Kind + "/"
	for o, err := range j.cfg.Store.List(ctx, root) {
		if err != nil {
			return rep, err
		}
		id, _, ok := strings.Cut(strings.TrimPrefix(o.Key, root), "/")
		if !ok || id == "" {
			continue
		}
		if cur == nil || cur.ID != id {
			if err := flush(); err != nil {
				return rep, err
			}
			cur = &OrphanFolder{Prefix: root + id + "/", ID: id, ValidID: contentref.ValidateID(id) == nil}
		}
		cur.Objects++
		if o.LastModified.After(cur.Newest) {
			cur.Newest = o.LastModified
		}
	}
	if err := flush(); err != nil {
		return rep, err
	}
	if len(batch) > 0 {
		if err := settle(batch); err != nil {
			return rep, err
		}
	}
	return rep, nil
}
