package video

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	mediastore "github.com/open-rails/contentkit/internal/mediastore"

	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/layout"
)

const blobCacheControl = "max-age=31536000, immutable"

// put hashes a file and writes it under its hash in area.
func (e *Encoder) put(ctx context.Context, item mediastore.Item, area, path, contentType string, fp *fileProgress) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	size, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return e.putBlob(ctx, item, area, f, size, h.Sum(nil), contentType, fp)
}

// putJSON stores v as a private JSON blob (a track index).
func (e *Encoder) putJSON(ctx context.Context, item mediastore.Item, v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	name, _, err := e.putBlob(ctx, item, outputArea(item), bytes.NewReader(b), int64(len(b)), sum[:], "application/json", nil)
	return name, err
}

// outputArea is where video outputs and their indexes are written:
// internal/ for a version, until its release moves them; private/ for
// another kind.
func outputArea(item mediastore.Item) string {
	if item.Kind().Versions {
		return layout.AreaInternal
	}
	return layout.AreaPrivate
}

// putBlob writes bytes under their hash: identical bytes have the same name,
// so writing them again is harmless.
func (e *Encoder) putBlob(ctx context.Context, item mediastore.Item, area string, body io.ReaderAt, size int64, sum []byte, contentType string, fp *fileProgress) (string, int64, error) {
	name := layout.BlobRef(area, sum)
	key, _ := item.Blob(name)
	start := time.Now()
	err := mediastore.PutFile(ctx, e.store, key, body, size, mediastore.FileOptions{Read: fp.reader,
		PutOptions: media.PutOptions{ContentType: contentType, CacheControl: blobCacheControl, ChecksumSHA256: sum}})
	if err != nil {
		return "", 0, err
	}
	fp.transferred(size, time.Since(start))
	return name, size, nil
}

// fetch downloads a blob to path. Blobs are verified when placed or
// produced, so the bytes are not hashed again.
func (e *Encoder) fetch(ctx context.Context, item mediastore.Item, blob, path string, fp *fileProgress) error {
	key, err := item.Blob(blob)
	if err != nil {
		return err
	}
	rc, obj, err := e.store.Get(ctx, key, media.GetOptions{})
	if err != nil {
		return err
	}
	defer rc.Close()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	start := time.Now()
	n, err := io.Copy(f, rc)
	fp.transferred(n, time.Since(start))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && n != obj.Size {
		err = fmt.Errorf("media/video: read %d of %d bytes of %s", n, obj.Size, key)
	}
	return err
}

// readIndex reads a track's index blob.
func (e *Encoder) readIndex(ctx context.Context, item mediastore.Item, blob string) (media.TrackIndex, error) {
	var idx media.TrackIndex
	key, err := item.Blob(blob)
	if err != nil {
		return idx, err
	}
	rc, _, err := e.store.Get(ctx, key, media.GetOptions{})
	if err != nil {
		return idx, err
	}
	defer rc.Close()
	err = json.NewDecoder(io.LimitReader(rc, 64<<20)).Decode(&idx)
	return idx, err
}
