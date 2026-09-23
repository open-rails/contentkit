package video

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
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
func (e *Encoder) put(ctx context.Context, item media.Item, path, contentType string) (string, int64, error) {
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
		return name, size, nil
	} else if err != nil && !errors.Is(err, media.ErrNotFound) {
		return "", 0, err
	}
	if size > multipartAbove {
		return name, size, e.putMultipart(ctx, key, f, size, contentType)
	}
	opts := media.PutOptions{ContentType: contentType, CacheControl: blobCacheControl, ChecksumSHA256: sum}
	if e.c.Store.Capabilities().ConditionalPut {
		opts.IfNoneMatch = "*"
	}
	_, err = e.c.Store.Put(ctx, key, io.NewSectionReader(f, 0, size), size, opts)
	if err != nil && !errors.Is(err, media.ErrPreconditionFailed) {
		return "", 0, err
	}
	return name, size, nil
}

// putMultipart uploads through the Store's presigned part URLs, each bound to
// its length and SHA-256. A failed upload is aborted (and the bucket's
// abort-incomplete rule catches a killed process).
func (e *Encoder) putMultipart(ctx context.Context, key string, f *os.File, size int64, contentType string) (err error) {
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
		sum := h.Sum(nil)
		req, err := e.c.Store.PresignPart(ctx, key, id, n, length, sum, time.Hour)
		if err != nil {
			return err
		}
		etag, err := sendPart(ctx, req, io.NewSectionReader(f, off, length), length)
		if err != nil {
			return fmt.Errorf("media/video: part %d of %s: %w", n, key, err)
		}
		parts = append(parts, media.Part{Number: n, Size: length, ETag: etag, SHA256: sum})
	}
	_, err = e.c.Store.CompleteMultipart(ctx, key, id, parts)
	return err
}

func sendPart(ctx context.Context, p media.PresignedRequest, body io.Reader, size int64) (string, error) {
	req, err := http.NewRequestWithContext(ctx, p.Method, p.URL, body)
	if err != nil {
		return "", err
	}
	req.Header = p.Header.Clone()
	req.ContentLength = size
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("status %d: %s", resp.StatusCode, msg)
	}
	return resp.Header.Get("ETag"), nil
}
