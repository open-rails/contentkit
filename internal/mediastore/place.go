package mediastore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"

	"github.com/open-rails/contentkit/contentref"
	media "github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/layout"
	"golang.org/x/sync/errgroup"
)

// placeConcurrency bounds the staged uploads one Place reads at once; each
// single-PUT upload is held in memory (at most MaxSinglePut).
const placeConcurrency = 4

// Place moves ref's staged uploads (tmp/u-{uuid}) to their hash names,
// {area}/{sha256} (Kind.Area) of the bytes the server read, and returns how
// many files it placed. The media worker runs it before processing:
//
//  1. read each staged upload, hashing it (the bucket does not verify a
//     client's checksum). A single PUT (at most MaxSinglePut) is written from
//     the bytes that were hashed; a multipart upload, fixed once completed,
//     is copied server-side while its ETag is the one read. Identical bytes
//     land on the same name, so a repeated write is harmless;
//  2. point the files still staged there at their blobs in one edit;
//  3. delete the staged objects the manifest no longer names.
//
// A staged upload that is gone or not the size committed fails its file
// (CodeNotUploaded: upload it again). Every step is idempotent, so a crash
// converges on the next run; a blob a crash leaves unnamed is an orphan GC
// removes.
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
	sizes := map[string]int64{}
	for _, f := range man.Files {
		if f.Staged != "" && f.Fail() == nil {
			sizes[f.Staged] = f.Size
		}
	}
	if len(sizes) == 0 {
		return 0, nil
	}
	area := item.Kind().Area("")
	var mu sync.Mutex
	blobs := make(map[string]string, len(sizes)) // staged name → blob, "" when gone or changed
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(placeConcurrency)
	for name, size := range sizes {
		g.Go(func() error {
			blob, err := m.placeOne(gctx, item, area, name, size)
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
	cur, err := m.EditExisting(ctx, ref, func(cur *Manifest) error {
		placed, failed = 0, failed[:0]
		for i := range cur.Files {
			f := &cur.Files[i]
			blob, ok := blobs[f.Staged]
			switch {
			case f.Staged == "" || !ok || f.Fail() != nil:
				continue
			case blob == "":
				f.Failed = &media.Failure{Of: f.Key(), Message: "the upload is gone or changed; upload it again", Code: media.CodeNotUploaded}
				f.Pending = nil
				failed = append(failed, f.Path)
				continue
			}
			f.Blob, f.Staged = blob, ""
			placed++
		}
		if placed == 0 && len(failed) == 0 {
			return ErrUnchanged
		}
		return nil
	})
	if errors.Is(err, ErrNotFound) {
		return 0, nil // deleted meanwhile: GC takes what this run wrote
	} else if err != nil {
		return 0, err
	}
	if h := m.reg.hooks.Failed; h != nil {
		for _, p := range failed {
			h(ctx, ref, p, errors.New("media: the staged upload is gone or changed"))
		}
	}
	staged := cur.StagedNames()
	for name := range blobs {
		if !slices.Contains(staged, name) {
			key, _ := item.Staged(name)
			if err := m.store.Delete(ctx, key); err != nil && !errors.Is(err, ErrNotFound) {
				return placed, err
			}
		}
	}
	return placed, nil
}

// placeOne hashes the staged upload and writes it under its hash. It
// returns the blob, or "" when the upload is gone or not size bytes.
func (m *Manifests) placeOne(ctx context.Context, item Item, area, name string, size int64) (string, error) {
	src, _ := item.Staged(name)
	rc, obj, err := m.store.Get(ctx, src, media.GetOptions{})
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
	if size <= media.MaxSinglePut {
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
	blob := layout.BlobRef(area, sum)
	dst, _ := item.Blob(blob)
	if body != nil {
		_, err = m.store.Put(ctx, dst, bytes.NewReader(body), size, media.PutOptions{ContentType: obj.ContentType, ChecksumSHA256: sum})
	} else {
		_, err = m.store.Copy(ctx, src, dst, media.CopyOptions{IfMatch: obj.ETag})
	}
	return blob, err
}
