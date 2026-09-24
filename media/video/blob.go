package video

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/open-rails/contentkit/media"
)

// Blobs above multipartAbove upload in partSize parts (a 2 h 4K rendition
// exceeds S3's 5 GiB single PUT).
var (
	multipartAbove int64 = 1 << 30
	partSize       int64 = 64 << 20
)

const blobCacheControl = "max-age=31536000, immutable"

// put stores the file as a content-addressed blob unless it already exists,
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
	sum := h.Sum(nil)
	name := media.SHA256Name(sum)
	key, _ := item.Blob(name)
	if obj, err := e.c.Store.Head(ctx, key); err == nil && obj.Size == size {
		fp.skipped(size)
		return name, size, nil
	} else if err != nil && !errors.Is(err, media.ErrNotFound) {
		return "", 0, err
	}
	start := time.Now()
	if size > multipartAbove {
		if err := e.putMultipart(ctx, key, f, size, contentType, fp); err != nil {
			return "", 0, err
		}
		fp.transferred(size, time.Since(start))
		return name, size, nil
	}
	opts := media.PutOptions{ContentType: contentType, CacheControl: blobCacheControl, ChecksumSHA256: sum}
	if e.c.Store.Capabilities().ConditionalPut {
		opts.IfNoneMatch = "*"
	}
	_, err = e.c.Store.Put(ctx, key, fp.reader(io.NewSectionReader(f, 0, size)), size, opts)
	if err != nil && !errors.Is(err, media.ErrPreconditionFailed) {
		return "", 0, err
	}
	fp.transferred(size, time.Since(start))
	return name, size, nil
}

// putMultipart uploads parts through the Store, each bound to its length and
// SHA-256. A failed upload is aborted (and the bucket's abort-incomplete rule
// catches a killed process).
func (e *Encoder) putMultipart(ctx context.Context, key string, f *os.File, size int64, contentType string, fp *fileProgress) (err error) {
	id, err := e.c.Store.CreateMultipart(ctx, key, contentType)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = e.c.Store.AbortMultipart(context.WithoutCancel(ctx), key, id)
		}
	}()
	var parts []media.Part
	for off, n := int64(0), int32(1); off < size; off, n = off+partSize, n+1 {
		length := min(partSize, size-off)
		h := sha256.New()
		if _, err := io.Copy(h, io.NewSectionReader(f, off, length)); err != nil {
			return err
		}
		p, err := e.c.Store.PutPart(ctx, key, id, n, fp.reader(io.NewSectionReader(f, off, length)), length, h.Sum(nil))
		if err != nil {
			return fmt.Errorf("media/video: part %d of %s: %w", n, key, err)
		}
		parts = append(parts, p)
	}
	_, err = e.c.Store.CompleteMultipart(ctx, key, id, parts)
	return err
}
