package media

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"sync"

	"golang.org/x/sync/errgroup"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media/layout"
)

// placeConcurrency bounds the staged uploads one Place reads at once; each
// single-PUT upload is held in memory (at most MaxSinglePut).
const placeConcurrency = 4

// Place moves ref's staged uploads (temp/u-{uuid}) to their content
// addresses, private/sha256-{hex} of the bytes the server read, and returns
// how many files it placed. The media worker runs it before processing:
//
//  1. read each staged upload, hashing it. A single PUT (at most
//     MaxSinglePut, the only kind a client can send again while its URL
//     lives) is written from the bytes that were hashed; a multipart upload,
//     fixed once completed, is copied server-side while its ETag is the one
//     read. Neither is written when the blob exists: a blob is never
//     overwritten, so its bytes always hash to its name;
//  2. point the files at their blobs in one edit, which checks under the
//     folder lock (the sweep holds it too) that each blob still exists;
//  3. delete the staged objects no manifest references, and, in that
//     edit, the blobs of uploads removed meanwhile (a takedown's stay gone).
//
// A staged upload that is gone or not the size committed fails its file
// (CodeNotUploaded: upload it again). Every step is idempotent, so a crash
// converges on the next run.
func (m *Manifests) Place(ctx context.Context, ref contentref.ContentRef) (int, error) {
	item, err := m.reg.Item(ref)
	if err != nil {
		return 0, err
	}
	man, _, err := m.Get(ctx, ref)
	if errors.Is(err, ErrNotFound) {
		return 0, nil
	} else if err != nil {
		return 0, err
	}
	if man.Full {
		return 0, nil // placing would grow it: wait for a commit that shrinks it
	}
	sizes := map[string]int64{}
	for _, f := range man.Files {
		if f.Staged != "" && f.Fail() == nil {
			sizes[f.Staged] = f.Size
		}
	}
	if len(sizes) == 0 {
		return 0, nil
	}
	var mu sync.Mutex
	blobs := make(map[string]string, len(sizes)) // staged name → blob, "" when gone or changed
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(placeConcurrency)
	for name, size := range sizes {
		g.Go(func() error {
			blob, err := m.placeOne(gctx, item, name, size)
			if err != nil {
				return fmt.Errorf("media: place %s%s: %w", item.TempPrefix(), name, err)
			}
			mu.Lock()
			blobs[name] = blob
			mu.Unlock()
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return 0, err
	}
	var placed int
	var failed []string
	_, err = m.EditExisting(ctx, ref, func(cur *Manifest) error {
		placed, failed = 0, failed[:0]
		live := map[string]bool{}
		for i := range cur.Files {
			f := &cur.Files[i]
			blob, ok := blobs[f.Staged]
			live[f.Staged] = true
			switch {
			case f.Staged == "" || !ok || f.Fail() != nil:
				continue
			case blob == "":
				f.Failed = &Failure{Of: f.Key(), Message: "the upload is gone or changed; upload it again", Code: CodeNotUploaded}
				f.Pending = nil
				failed = append(failed, f.Path)
				continue
			}
			// A sweep run since the check above may have taken an existing
			// blob; the next run writes it again.
			key, _ := item.Blob(blob)
			if _, err := m.store.Head(ctx, key); err != nil {
				return fmt.Errorf("media: place %s: %w", key, err)
			}
			f.Blob, f.Staged = blob, ""
			placed++
		}
		var gone []string
		for name, blob := range blobs {
			if blob != "" && !live[name] {
				gone = append(gone, blob)
			}
		}
		return m.DeleteUnreferenced(ctx, item, cur, gone)
	})
	if errors.Is(err, ErrNotFound) {
		// Deleted meanwhile: drop what this run wrote, as a folder deletion
		// that ran before it could not.
		var written []string
		for _, b := range blobs {
			if b != "" {
				written = append(written, b)
			}
		}
		return 0, errors.Join(m.DropIfDeleted(ctx, ref, written), m.dropStaged(ctx, item, blobs))
	} else if err != nil {
		return 0, err
	}
	if h := m.reg.cfg.Hooks.Failed; h != nil {
		for _, p := range failed {
			h(ctx, ref, p, errors.New("media: the staged upload is gone or changed"))
		}
	}
	return placed, m.dropStaged(ctx, item, blobs)
}

// placeOne hashes the staged upload and writes its blob unless it exists.
// It returns the blob, or "" when the upload is gone or not size bytes.
func (m *Manifests) placeOne(ctx context.Context, item Item, name string, size int64) (string, error) {
	src, _ := item.Staged(name)
	rc, obj, err := m.store.Get(ctx, src, GetOptions{})
	if errors.Is(err, ErrNotFound) {
		return "", nil
	} else if err != nil {
		return "", err
	}
	defer rc.Close()
	if obj.Size != size {
		return "", nil
	}
	h := sha256.New()
	var body []byte
	if size <= MaxSinglePut {
		body = make([]byte, size)
		_, err = io.ReadFull(io.TeeReader(rc, h), body)
	} else {
		var n int64
		if n, err = io.Copy(h, rc); err == nil && n != size {
			err = fmt.Errorf("read %d of %d bytes", n, size)
		}
	}
	if err != nil {
		return "", err
	}
	sum := h.Sum(nil)
	blob := layout.SHA256Name(sum)
	dst, _ := item.Blob(blob)
	if _, err := m.store.Head(ctx, dst); err == nil {
		return blob, nil
	} else if !errors.Is(err, ErrNotFound) {
		return "", err
	}
	if body != nil {
		opts := PutOptions{ContentType: obj.ContentType, ChecksumSHA256: sum}
		if m.store.Capabilities().ConditionalPut {
			opts.IfNoneMatch = "*"
		}
		_, err = m.store.Put(ctx, dst, bytes.NewReader(body), size, opts)
	} else {
		_, err = m.store.Copy(ctx, src, dst, CopyOptions{IfMatch: obj.ETag})
	}
	if body != nil && errors.Is(err, ErrPreconditionFailed) {
		err = nil // placed meanwhile, from the same bytes
	}
	return blob, err
}

// dropStaged deletes the staged objects of names that the manifest no longer
// references, under the folder lock, so a commit naming one either sees it
// gone or keeps it.
func (m *Manifests) dropStaged(ctx context.Context, item Item, names map[string]string) error {
	unlock, err := m.locker.Lock(ctx, item.ManifestKey())
	if err != nil {
		return err
	}
	defer unlock()
	keep := map[string]bool{}
	if man, _, err := m.get(ctx, item.ManifestKey()); err == nil {
		for _, s := range man.StagedNames() {
			keep[s] = true
		}
	} else if !errors.Is(err, ErrNotFound) {
		return err
	}
	var errs []error
	for name := range names {
		if keep[name] {
			continue
		}
		key, _ := item.Staged(name)
		if err := m.store.Delete(ctx, key); err != nil && !errors.Is(err, ErrNotFound) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
