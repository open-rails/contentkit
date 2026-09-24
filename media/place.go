package media

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media/layout"
)

// ErrStagedGone is Place's answer when neither the staged upload nor its
// placed original exists: the file was replaced, removed or swept.
var ErrStagedGone = errors.New("media: staged upload is gone")

// Staged is a staged upload the caller has read in full: its name, the ETag
// it read and the SHA-256 of the bytes.
type Staged struct {
	Name   string
	ETag   string
	SHA256 []byte
}

// Place moves a staged upload (staging/u-{uuid}) to its content address,
// originals/sha256-{hex}, and returns that name. The media worker calls it
// with the hash it computed while reading the upload for processing:
//
//  1. copy staging → originals server-side, unless the folder already holds
//     the hash (dedupe), and verify the copy's size (and full-object SHA-256
//     when the store reports one);
//  2. rename every reference in the folder's manifests (original, master,
//     hls source, download inputs), one conditional edit per manifest;
//  3. delete the staged object.
//
// Each step is idempotent and the manifests switch only after a verified
// copy, so a crash anywhere converges on the next run; the sweep removes a
// staged object left unreferenced.
func (m *Manifests) Place(ctx context.Context, ref contentref.ContentRef, s Staged) (string, error) {
	item, err := m.kinds.Item(ref.Content())
	if err != nil {
		return "", err
	}
	if !layout.ValidStagedName(s.Name) || len(s.SHA256) != sha256.Size {
		return "", fmt.Errorf("media: place %q: needs a staged name and a SHA-256", s.Name)
	}
	name := SHA256Name(s.SHA256)
	src, _ := item.Original(s.Name)
	dst, _ := item.Original(name)

	staged, err := m.store.Head(ctx, src)
	switch {
	case errors.Is(err, ErrNotFound):
		// Placed and deleted by an earlier run, which may have stopped before
		// renaming every manifest.
		if _, err := m.store.Head(ctx, dst); errors.Is(err, ErrNotFound) {
			return "", fmt.Errorf("%w: %s", ErrStagedGone, src)
		} else if err != nil {
			return "", err
		}
	case err != nil:
		return "", err
	default:
		if s.ETag != "" && staged.ETag != s.ETag {
			return "", fmt.Errorf("%w: %s changed since it was hashed", ErrPreconditionFailed, src)
		}
		if err := m.copyStaged(ctx, item, staged, dst, name, s.SHA256); err != nil {
			return "", err
		}
	}
	if err := m.renameSource(ctx, item, s.Name, name); err != nil {
		return "", err
	}
	if err := m.store.Delete(ctx, src); err != nil && !errors.Is(err, ErrNotFound) {
		return "", err
	}
	return name, nil
}

// copyStaged puts the staged object at dst unless a manifest in the folder
// already references dst: an unreferenced one may be near the sweep, and
// copying over it refreshes it.
func (m *Manifests) copyStaged(ctx context.Context, item Item, staged Object, dst, name string, sum []byte) error {
	if cur, err := m.store.Head(ctx, dst); err == nil && cur.Size == staged.Size {
		if ok, err := m.references(ctx, item, name); err != nil || ok {
			return err
		}
	} else if err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	if _, err := m.store.Copy(ctx, staged.Key, dst, CopyOptions{IfMatch: staged.ETag}); err != nil {
		return err
	}
	got, err := m.store.Head(ctx, dst)
	if err != nil {
		return err
	}
	if got.Size != staged.Size || got.ChecksumSHA256 != nil && !bytes.Equal(got.ChecksumSHA256, sum) {
		_ = m.store.Delete(context.WithoutCancel(ctx), dst)
		return fmt.Errorf("media: placed copy %s does not match %s", dst, staged.Key)
	}
	return nil
}

// renameSource points every manifest in the folder that references from at to.
func (m *Manifests) renameSource(ctx context.Context, item Item, from, to string) error {
	keys, err := m.manifestKeys(ctx, item)
	if err != nil {
		return err
	}
	for _, key := range keys {
		man, _, err := m.get(ctx, key)
		if errors.Is(err, ErrNotFound) {
			continue
		} else if err != nil {
			return err
		}
		if !man.renameSource(from, to) {
			continue
		}
		ref := item.Ref().Content()
		if item.Kind().Versioned {
			v := strings.TrimSuffix(strings.TrimPrefix(key, item.ManifestsPrefix()), ".json")
			ref = ref.WithVersion(v)
		}
		if _, err := m.Edit(ctx, ref, func(man *Manifest) error {
			man.renameSource(from, to)
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}

// manifestKeys lists the folder's manifests.
func (m *Manifests) manifestKeys(ctx context.Context, item Item) ([]string, error) {
	if !item.Kind().Versioned {
		return []string{item.Prefix() + "manifest.json"}, nil
	}
	var keys []string
	for o, err := range m.store.List(ctx, item.ManifestsPrefix()) {
		if err != nil {
			return nil, err
		}
		if k, ok := layout.Parse(o.Key); ok && k.Area == AreaManifest {
			keys = append(keys, o.Key)
		}
	}
	return keys, nil
}

// renameSource replaces the original name from with to wherever the
// manifest refers to it, keeping derived outputs fresh; it reports a change.
func (m *Manifest) renameSource(from, to string) bool {
	changed := false
	swap := func(s *string) {
		if *s == from {
			*s, changed = to, true
		}
	}
	for i := range m.Files {
		f := &m.Files[i]
		swap(&f.Original)
		swap(&f.Master)
		if f.HLS != nil {
			swap(&f.HLS.Source)
		}
		if f.Failure != nil {
			if rest, ok := strings.CutPrefix(f.Failure.Of, from+"."); ok {
				f.Failure.Of, changed = to+"."+rest, true
			}
		}
	}
	for k, d := range m.Downloads {
		if d.Inputs == from {
			d.Inputs, changed = to, true
			m.Downloads[k] = d
		}
	}
	return changed
}
