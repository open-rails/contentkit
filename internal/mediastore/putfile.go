package mediastore

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"

	media "github.com/open-rails/contentkit/media"
)

// Files past SinglePutMax go up in PartSize parts: S3 (and Ceph RGW) refuse
// a single PUT past 5 GiB, and a lossless video master or a 2 h 4K
// rendition exceeds it.
const (
	SinglePutMax int64 = 1 << 30
	PartSize     int64 = 64 << 20
)

// FileOptions are PutFile's. Zero SinglePutMax and PartSize take the
// defaults; Read wraps each body read (progress).
type FileOptions struct {
	media.PutOptions
	SinglePutMax, PartSize int64
	Read                   func(io.Reader) io.Reader
}

// PutFile writes size bytes of f at key: one PUT up to SinglePutMax, else a
// multipart upload streamed from f, each part bound to its length and
// SHA-256. A failed multipart upload is aborted; the bucket's
// abort-incomplete rule catches a killed process.
func PutFile(ctx context.Context, s media.Store, key string, f io.ReaderAt, size int64, o FileOptions) (err error) {
	single, part := o.SinglePutMax, o.PartSize
	if single <= 0 {
		single = SinglePutMax
	}
	if part <= 0 {
		part = PartSize
	}
	read := o.Read
	if read == nil {
		read = func(r io.Reader) io.Reader { return r }
	}
	if size <= single {
		_, err := s.Put(ctx, key, read(io.NewSectionReader(f, 0, size)), size, o.PutOptions)
		return err
	}
	id, err := s.CreateMultipart(ctx, key, o.ContentType)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = s.AbortMultipart(context.WithoutCancel(ctx), key, id)
		}
	}()
	var parts []media.Part
	for off, n := int64(0), int32(1); off < size; off, n = off+part, n+1 {
		length := min(part, size-off)
		h := sha256.New()
		if _, err := io.Copy(h, io.NewSectionReader(f, off, length)); err != nil {
			return err
		}
		p, err := s.PutPart(ctx, key, id, n, read(io.NewSectionReader(f, off, length)), length, h.Sum(nil))
		if err != nil {
			return fmt.Errorf("media: part %d of %s: %w", n, key, err)
		}
		parts = append(parts, p)
	}
	_, err = s.CompleteMultipart(ctx, key, id, parts)
	return err
}
