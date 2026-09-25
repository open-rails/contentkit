package media

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media/layout"
)

// SweepResult reports one folder sweep. Wait > 0 means a manifest changed
// within the grace period and nothing was deleted; sweep again after Wait.
type SweepResult struct {
	Deleted []string
	Wait    time.Duration
}

// Sweep deletes what the item's manifest does not keep: originals/,
// private/ and public/ objects outside its index and staged uploads no file
// references, once the manifest is older than the grace period, and only
// objects past abandonedAt. Removed public/ keys go to Hooks.PublicRemoved.
//
// Invariant: the sweep deletes only objects the manifest does not reference
// and that no in-flight commit or job can newly reference. Uploads keeps the
// second half: presign reuses an existing original, and a commit accepts one,
// only while it is referenced or well before abandonedAt (see protected);
// jobs write renditions before the edit that lists them, well within grace.
func (j *Jobs) Sweep(ctx context.Context, ref contentref.ContentRef) (SweepResult, error) {
	item, err := j.cfg.Kinds.Item(ref.Content())
	if err != nil {
		return SweepResult{}, err
	}
	return j.sweep(ctx, item.Prefix(), nil)
}

// SweepAll sweeps every folder of the configured tenants whose kind is
// registered: the periodic backstop for missed schedules and abandoned uploads.
func (j *Jobs) SweepAll(ctx context.Context) error {
	var errs []error
	for _, tenant := range j.cfg.Tenants {
		var folder string
		var objs []Object
		flush := func() {
			if folder != "" {
				if _, err := j.sweep(ctx, folder, objs); err != nil {
					errs = append(errs, err)
				}
			}
			folder, objs = "", nil
		}
		for obj, err := range j.cfg.Store.List(ctx, tenant+"/") {
			if err != nil {
				return errors.Join(append(errs, err)...)
			}
			parts := strings.SplitN(obj.Key, "/", 4)
			if len(parts) < 4 {
				continue
			}
			if _, err := j.cfg.Kinds.Kind(parts[1]); err != nil {
				continue
			}
			p, err := folderPrefix(parts[0], parts[1], parts[2])
			if err != nil {
				continue
			}
			if p != folder {
				flush() // keys sharing a prefix are listed contiguously
				folder = p
			}
			objs = append(objs, obj)
		}
		flush()
	}
	if len(errs) > 10 {
		errs = append(errs[:10], fmt.Errorf("media: and %d more folders failed", len(errs)-10))
	}
	return errors.Join(errs...)
}

func (j *Jobs) sweep(ctx context.Context, prefix string, objs []Object) (SweepResult, error) {
	if objs == nil {
		var err error
		if objs, err = j.list(ctx, prefix); err != nil {
			return SweepResult{}, err
		}
	}
	now := j.cfg.Now()
	cutoff := now.Add(-j.cfg.Grace)
	manifests, newest := manifestETags(objs)
	if newest.After(cutoff) {
		return SweepResult{Wait: newest.Sub(cutoff) + time.Second}, nil
	}
	refs := map[string]bool{}
	for key := range manifests {
		root, err := j.readRoot(ctx, key)
		if errors.Is(err, ErrNotFound) {
			return SweepResult{Wait: j.cfg.Grace}, nil
		}
		if err != nil {
			return SweepResult{}, err
		}
		refs = root.Refs()
	}
	abandoned := func(objs []Object) []string {
		var keys []string
		for _, o := range objs {
			k, ok := layout.Parse(o.Key)
			if !ok || k.Area == AreaManifest {
				continue
			}
			if !refs[k.Area+"/"+k.Name] && !now.Before(abandonedAt(k.Name, o.LastModified, j.cfg.Grace)) {
				keys = append(keys, o.Key)
			}
		}
		return keys
	}
	if len(abandoned(objs)) == 0 {
		return SweepResult{}, nil
	}
	// A manifest written since the listing may reference a doomed object, and
	// an upload may have refreshed one: only the second listing decides.
	again, err := j.list(ctx, prefix)
	if err != nil {
		return SweepResult{}, err
	}
	if now, _ := manifestETags(again); !maps.Equal(now, manifests) {
		return SweepResult{Wait: j.cfg.Grace}, nil
	}
	doomed := abandoned(again)
	if len(doomed) == 0 {
		return SweepResult{}, nil
	}
	if err := j.deleteKeys(ctx, doomed); err != nil {
		return SweepResult{}, err
	}
	j.publicRemoved(ctx, doomed)
	return SweepResult{Deleted: doomed}, nil
}

// multipartLife is the bucket's 1-day abort rule: backends may date a
// completed multipart object at its initiation.
const multipartLife = 24 * time.Hour

// commitMargin bounds the time between a commit's check of an original and its
// manifest edit landing (the edit runs under half of it): grace/4, at most 1 h.
func commitMargin(grace time.Duration) time.Duration { return min(grace/4, time.Hour) }

// abandonedAt is when the sweep may delete an unreferenced blob or original.
func abandonedAt(name string, modified time.Time, grace time.Duration) time.Time {
	if strings.HasPrefix(name, layout.UploadPrefix) {
		return modified.Add(grace + multipartLife)
	}
	return modified.Add(grace)
}

func manifestKeys(objs []Object) map[string]string {
	m, _ := manifestETags(objs)
	return m
}

func manifestETags(objs []Object) (map[string]string, time.Time) {
	etags := map[string]string{}
	var newest time.Time
	for _, o := range objs {
		if k, ok := layout.Parse(o.Key); ok && k.Area == AreaManifest {
			etags[o.Key] = o.ETag
			if o.LastModified.After(newest) {
				newest = o.LastModified
			}
		}
	}
	return etags, newest
}

func (j *Jobs) readRoot(ctx context.Context, key string) (*Root, error) {
	rc, _, err := j.cfg.Store.Get(ctx, key, GetOptions{})
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err != nil {
		return nil, err
	}
	var root Root
	if err := json.Unmarshal(body, &root); err != nil {
		return nil, fmt.Errorf("media: decode manifest %s: %w", key, err)
	}
	return &root, nil
}

// publicRemoved reports deleted public/ keys, grouped by item, to
// Hooks.PublicRemoved.
func (j *Jobs) publicRemoved(ctx context.Context, keys []string) {
	if j.cfg.Hooks.PublicRemoved == nil {
		return
	}
	byItem := map[contentref.ContentRef][]string{}
	for _, key := range keys {
		if k, ok := layout.Parse(key); ok && k.Area == AreaPublic {
			ref := contentref.New(k.Tenant, k.Kind, k.ID)
			byItem[ref] = append(byItem[ref], key)
		}
	}
	for ref, keys := range byItem {
		j.cfg.Hooks.PublicRemoved(ctx, ref, keys)
	}
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

// deleteFolder removes every object under prefix, manifests and public/ first
// so readers stop resolving the item, and tokenless URLs stop answering,
// before its other files go.
func (j *Jobs) deleteFolder(ctx context.Context, prefix string) error {
	objs, err := j.list(ctx, prefix)
	if err != nil {
		return err
	}
	var manifests, rest []string
	for _, o := range objs {
		if k, ok := layout.Parse(o.Key); ok && (k.Area == AreaManifest || k.Area == AreaPublic) {
			manifests = append(manifests, o.Key)
		} else {
			rest = append(rest, o.Key)
		}
	}
	if err := j.deleteKeys(ctx, manifests); err != nil {
		return err
	}
	j.publicRemoved(ctx, manifests)
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
