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
)

// SweepResult reports one folder sweep. Wait > 0 means a manifest changed
// within the grace period and nothing was deleted; sweep again after Wait.
type SweepResult struct {
	Deleted []string
	Wait    time.Duration
}

// Sweep deletes the item folder's blobs/ and hash-named originals/ that no
// manifest in the folder references, once every manifest is older than the
// grace period, and only objects older than it. Slot originals, public/ and
// manifests are never swept.
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
	cutoff := j.cfg.Now().Add(-j.cfg.Grace)
	manifests, newest := manifestETags(objs)
	if newest.After(cutoff) {
		return SweepResult{Wait: newest.Sub(cutoff) + time.Second}, nil
	}
	refs := map[string]bool{}
	for key := range manifests {
		man, err := j.readManifest(ctx, key)
		if errors.Is(err, ErrNotFound) {
			return SweepResult{Wait: j.cfg.Grace}, nil
		}
		if err != nil {
			return SweepResult{}, err
		}
		for _, n := range man.Blobs() {
			refs[AreaBlobs+"/"+n] = true
		}
		for _, n := range man.Originals() {
			refs[AreaOriginals+"/"+n] = true
		}
	}
	var doomed []string
	for _, o := range objs {
		k, ok := ParseKey(o.Key)
		if !ok || !isBlobName(k.Name) || (k.Area != AreaBlobs && k.Area != AreaOriginals) {
			continue
		}
		if !refs[k.Area+"/"+k.Name] && !o.LastModified.After(cutoff) {
			doomed = append(doomed, o.Key)
		}
	}
	if len(doomed) == 0 {
		return SweepResult{}, nil
	}
	// A manifest written since the listing may reference a doomed object.
	again, err := j.list(ctx, prefix)
	if err != nil {
		return SweepResult{}, err
	}
	if now, _ := manifestETags(again); !maps.Equal(now, manifests) {
		return SweepResult{Wait: j.cfg.Grace}, nil
	}
	if err := j.deleteKeys(ctx, doomed); err != nil {
		return SweepResult{}, err
	}
	return SweepResult{Deleted: doomed}, nil
}

func manifestKeys(objs []Object) map[string]string {
	m, _ := manifestETags(objs)
	return m
}

func manifestETags(objs []Object) (map[string]string, time.Time) {
	etags := map[string]string{}
	var newest time.Time
	for _, o := range objs {
		if k, ok := ParseKey(o.Key); ok && k.Area == AreaManifest {
			etags[o.Key] = o.ETag
			if o.LastModified.After(newest) {
				newest = o.LastModified
			}
		}
	}
	return etags, newest
}

func (j *Jobs) readManifest(ctx context.Context, key string) (*Manifest, error) {
	rc, _, err := j.cfg.Store.Get(ctx, key, GetOptions{})
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err != nil {
		return nil, err
	}
	var man Manifest
	if err := json.Unmarshal(body, &man); err != nil {
		return nil, fmt.Errorf("media: decode manifest %s: %w", key, err)
	}
	return &man, nil
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

// deleteFolder removes every object under prefix, manifests first so readers
// stop resolving the item before its files go.
func (j *Jobs) deleteFolder(ctx context.Context, prefix string) error {
	objs, err := j.list(ctx, prefix)
	if err != nil {
		return err
	}
	var manifests, rest []string
	for _, o := range objs {
		if k, ok := ParseKey(o.Key); ok && k.Area == AreaManifest {
			manifests = append(manifests, o.Key)
		} else {
			rest = append(rest, o.Key)
		}
	}
	if err := j.deleteKeys(ctx, manifests); err != nil {
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
