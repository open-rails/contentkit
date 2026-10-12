package contentkit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"

	"github.com/open-rails/contentkit/media"
)

// MediaFile is one file IngestMedia uploads.
type MediaFile struct {
	// Path is its upload path ("originals/0001.png"); a named path's {name}
	// is chosen by the server.
	Path string
	Type string
	// Body is read twice: hashed, then sent.
	Body io.ReadSeeker
	Meta map[string]any
}

// ingestPartSize is the multipart part IngestMedia prefers.
const ingestPartSize = 32 << 20

// IngestMedia uploads files into one media item (a content item's, or one
// version's) and commits them as one operation: the upload routes' presign,
// a PUT or multipart parts to the bucket over bucket (http.DefaultClient when
// nil), then commit. operationID is a stable UUID of the commit, so sending
// it again after a failure commits once. It is how an importer fills an
// imported version's folder.
func (c *Client) IngestMedia(ctx context.Context, bucket *http.Client, ref media.RefBody, operationID string, files []MediaFile) (media.CommitReply, error) {
	if bucket == nil {
		bucket = http.DefaultClient
	}
	ops := make([]media.Op, 0, len(files))
	for _, f := range files {
		size, err := f.Body.Seek(0, io.SeekEnd)
		if err != nil {
			return media.CommitReply{}, err
		}
		sum, err := digest(f.Body, 0, size)
		if err != nil {
			return media.CommitReply{}, err
		}
		plan, err := c.PostMediaUploadPresign(ctx, media.PresignBody{Ref: ref, Path: f.Path, Type: f.Type, Size: size, SHA256: sum})
		if err != nil {
			return media.CommitReply{}, err
		}
		blob := plan.Blob
		switch {
		case plan.Exists:
		case plan.Put != nil:
			err = upload(ctx, bucket, *plan.Put, f.Body, 0, size)
		case plan.Multipart != nil:
			blob, err = c.multipart(ctx, bucket, *plan.Multipart, f.Body, size)
		default:
			err = fmt.Errorf("contentkit: presign of %s planned no upload", f.Path)
		}
		if err != nil {
			return media.CommitReply{}, err
		}
		ops = append(ops, media.Op{Op: media.OpPut, Path: plan.Path, Blob: blob, Meta: f.Meta})
	}
	return c.PostMediaUploadCommit(ctx, media.CommitBody{Ref: ref, OperationID: operationID, Ops: ops})
}

// multipart writes body in parts and answers the assembled upload's blob.
func (c *Client) multipart(ctx context.Context, bucket *http.Client, m media.MultipartReply, body io.ReadSeeker, size int64) (string, error) {
	part := max(int64(ingestPartSize), m.MinPartSize)
	if m.MaxPartSize > 0 {
		part = min(part, m.MaxPartSize)
	}
	if n := (size + part - 1) / part; m.MaxParts > 0 && n > int64(m.MaxParts) {
		part = (size + int64(m.MaxParts) - 1) / int64(m.MaxParts)
	}
	var parts []media.PartBody
	for off, n := int64(0), int32(1); off < size; off, n = off+part, n+1 {
		sum, err := digest(body, off, min(part, size-off))
		if err != nil {
			return "", err
		}
		parts = append(parts, media.PartBody{Number: n, Size: min(part, size-off), SHA256: sum})
	}
	ticket := media.TicketBody{Ticket: m.Ticket}
	signed, err := c.PostMediaUploadParts(ctx, media.PartsBody{Ticket: m.Ticket, Parts: parts})
	if err == nil {
		for i, p := range signed.Parts {
			if p.Request == nil || i >= len(parts) {
				err = fmt.Errorf("contentkit: part %d was not presigned", p.Number)
				break
			}
			if err = upload(ctx, bucket, *p.Request, body, int64(p.Number-1)*part, parts[i].Size); err != nil {
				break
			}
		}
	}
	if err != nil {
		_ = c.PostMediaUploadAbort(context.WithoutCancel(ctx), ticket)
		return "", err
	}
	done, err := c.PostMediaUploadComplete(ctx, ticket)
	return done.Blob, err
}

// digest is the hex SHA-256 of body's n bytes from off.
func digest(body io.ReadSeeker, off, n int64) (string, error) {
	if _, err := body.Seek(off, io.SeekStart); err != nil {
		return "", err
	}
	h := sha256.New()
	if _, err := io.CopyN(h, body, n); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// upload sends body's n bytes from off as the presigned request.
func upload(ctx context.Context, bucket *http.Client, r media.RequestReply, body io.ReadSeeker, off, n int64) error {
	if _, err := body.Seek(off, io.SeekStart); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, r.Method, r.URL, io.LimitReader(body, n))
	if err != nil {
		return err
	}
	req.ContentLength = n
	for k, v := range r.Headers {
		req.Header.Set(k, v)
	}
	res, err := bucket.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return fmt.Errorf("contentkit: bucket answered %d to an upload: %s", res.StatusCode, msg)
	}
	_, _ = io.Copy(io.Discard, res.Body)
	return nil
}
