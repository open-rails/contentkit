package video

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/layout"
)

// Blobs above multipartAbove upload in partSize parts (a 2 h 4K rendition
// exceeds S3's 5 GiB single PUT).
var (
	multipartAbove int64 = 1 << 30
	partSize       int64 = 64 << 20
)

const blobCacheControl = "max-age=31536000, immutable"

// checkOutputs runs under the manifest lock, which also fences sweep deletion.
func (e *Encoder) checkOutputs(ctx context.Context, item media.Item, blobs ...string) error {
	for _, name := range blobs {
		key, err := item.Blob(name)
		if err != nil {
			return err
		}
		if _, err := e.store.Head(ctx, key); err != nil {
			return fmt.Errorf("media/video: publish output %s: %w", key, err)
		}
	}
	return nil
}

// put stores a file as a content-addressed private blob unless it exists,
// which makes retries cheap: outputs are byte-identical.
func (e *Encoder) put(ctx context.Context, item media.Item, path, contentType string, fp *fileProgress) (string, int64, error) {
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
	return e.putBlob(ctx, item, f, size, h.Sum(nil), contentType, fp)
}

// putJSON stores v as a private JSON blob (a track index).
func (e *Encoder) putJSON(ctx context.Context, item media.Item, v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	name, _, err := e.putBlob(ctx, item, bytes.NewReader(b), int64(len(b)), sum[:], "application/json", nil)
	return name, err
}

func (e *Encoder) putBlob(ctx context.Context, item media.Item, body io.ReaderAt, size int64, sum []byte, contentType string, fp *fileProgress) (string, int64, error) {
	name := layout.SHA256Name(sum)
	key, _ := item.Blob(name)
	if obj, err := e.store.Head(ctx, key); err == nil && obj.Size == size {
		fp.skipped(size)
		return name, size, nil
	} else if err != nil && !errors.Is(err, media.ErrNotFound) {
		return "", 0, err
	}
	start := time.Now()
	if size > multipartAbove {
		if err := e.putMultipart(ctx, key, body, size, contentType, fp); err != nil {
			return "", 0, err
		}
		fp.transferred(size, time.Since(start))
		return name, size, nil
	}
	opts := media.PutOptions{ContentType: contentType, CacheControl: blobCacheControl, ChecksumSHA256: sum}
	if e.store.Capabilities().ConditionalPut {
		opts.IfNoneMatch = "*"
	}
	_, err := e.store.Put(ctx, key, fp.reader(io.NewSectionReader(body, 0, size)), size, opts)
	if err != nil && !errors.Is(err, media.ErrPreconditionFailed) {
		return "", 0, err
	}
	fp.transferred(size, time.Since(start))
	return name, size, nil
}

// putMultipart uploads parts through the Store, each bound to its length and
// SHA-256. A failed upload is aborted (and the bucket's abort-incomplete rule
// catches a killed process).
func (e *Encoder) putMultipart(ctx context.Context, key string, f io.ReaderAt, size int64, contentType string, fp *fileProgress) (err error) {
	id, err := e.store.CreateMultipart(ctx, key, contentType)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = e.store.AbortMultipart(context.WithoutCancel(ctx), key, id)
		}
	}()
	var parts []media.Part
	for off, n := int64(0), int32(1); off < size; off, n = off+partSize, n+1 {
		length := min(partSize, size-off)
		h := sha256.New()
		if _, err := io.Copy(h, io.NewSectionReader(f, off, length)); err != nil {
			return err
		}
		p, err := e.store.PutPart(ctx, key, id, n, fp.reader(io.NewSectionReader(f, off, length)), length, h.Sum(nil))
		if err != nil {
			return fmt.Errorf("media/video: part %d of %s: %w", n, key, err)
		}
		parts = append(parts, p)
	}
	_, err = e.store.CompleteMultipart(ctx, key, id, parts)
	return err
}

// fetch downloads a blob to path. Blobs are verified when placed or
// produced, so the bytes are not hashed again.
func (e *Encoder) fetch(ctx context.Context, item media.Item, blob, path string, fp *fileProgress) error {
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
func (e *Encoder) readIndex(ctx context.Context, item media.Item, blob string) (media.TrackIndex, error) {
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
