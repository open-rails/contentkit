package contentkit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
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
	// From instead copies this key of the item's bucket, outside
	// ContentKit's namespaces, server-side (root:import:write); Size, when
	// set, is checked against it.
	From string
	Size int64
	Meta map[string]any
}

// ingestPartSize is the multipart part IngestMedia prefers.
const ingestPartSize = 32 << 20

// IngestMedia uploads files into one media item (a content item's, or one
// version's) and commits them as one operation: the upload routes' presign,
// a PUT or multipart parts to the bucket over a separate storage client (a
// fresh unauthenticated client when nil), then commit. operationID is a UUID of this commit; sending the files
// again (a new operationID) finds bytes the folder holds and uploads nothing
// twice. Files with From are copied server-side, in one request. It is how
// an importer fills an imported version's folder. This helper does not retain
// upload plans: retry an uncertain commit with its exact CommitBody, not a
// fresh invocation under the same operationID. The call returns its attached
// uploads and pending processing; ZIP extraction and renditions run through
// the normal media workers. It changes no content metadata or publication. Create the item/version before uploading, and edit
// or publish it explicitly afterward. The storage client must not inject API
// credentials; its cookies and redirect handler are never used.
func (c *Client) IngestMedia(ctx context.Context, bucket *http.Client, ref media.RefBody, operationID string, files []MediaFile) (media.CommitReply, error) {
	bucket = mediaStorageClient(bucket)
	ops := make([]media.Op, len(files))
	var copies []media.CopyRequest
	var copied []int
	for i, f := range files {
		if f.From != "" {
			copies = append(copies, media.CopyRequest{Path: f.Path, Type: f.Type, From: f.From, Size: f.Size})
			copied = append(copied, i)
			continue
		}
		if f.Body == nil {
			return media.CommitReply{}, fmt.Errorf("contentkit: %s has no upload body", f.Path)
		}
		size, err := f.Body.Seek(0, io.SeekEnd)
		if err != nil {
			return media.CommitReply{}, err
		}
		sum, err := digest(contextSeeker{ctx, f.Body}, 0, size)
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
		ops[i] = media.Op{Op: media.OpPut, Path: plan.Path, Blob: blob, Meta: f.Meta}
	}
	if len(copies) > 0 {
		staged, err := c.PostMediaUploadCopy(ctx, media.CopyBody{Ref: ref, Files: copies})
		if err != nil {
			return media.CommitReply{}, err
		}
		if len(staged.Files) != len(copies) {
			return media.CommitReply{}, fmt.Errorf("contentkit: %d copies staged of %d", len(staged.Files), len(copies))
		}
		for j, s := range staged.Files {
			ops[copied[j]] = media.Op{Op: media.OpPut, Path: s.Path, Blob: s.Blob, Meta: files[copied[j]].Meta}
		}
	}
	return c.PostMediaUploadCommit(ctx, media.CommitBody{Ref: ref, OperationID: operationID, Ops: ops})
}

// mediaStorageClient isolates presigned capabilities from API authentication
// and browser sessions. A capability is valid only at its signed URL.
func mediaStorageClient(client *http.Client) *http.Client {
	if client == nil {
		client = &http.Client{}
	}
	copy := *client
	copy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	copy.Jar = nil
	return &copy
}

// multipart writes body in parts and answers the assembled upload's blob.
func (c *Client) multipart(ctx context.Context, bucket *http.Client, m media.MultipartReply, body io.ReadSeeker, size int64) (string, error) {
	if err := c.transferParts(ctx, bucket, m, body, size, false, nil); err != nil {
		return "", err
	}
	done, err := c.PostMediaUploadComplete(ctx, media.TicketBody{Ticket: m.Ticket})
	return done.Blob, err
}

type uploadPart struct {
	media.PartBody
	offset int64
}

// transferParts leaves failed uploads resumable. Only an explicit abort should
// discard bytes that landed before a cancelled request or transient failure.
func (c *Client) transferParts(ctx context.Context, bucket *http.Client, m media.MultipartReply, body io.ReadSeeker, size int64, resume bool, progress func(int64)) error {
	if size <= 0 || body == nil || m.Ticket == "" || m.MinPartSize <= 0 || m.MaxPartSize < m.MinPartSize || m.MaxParts <= 0 {
		return errors.New("contentkit: invalid multipart upload plan")
	}
	part := max(int64(ingestPartSize), m.MinPartSize)
	part = min(part, m.MaxPartSize)
	if n := (size-1)/part + 1; n > int64(m.MaxParts) {
		part = (size-1)/int64(m.MaxParts) + 1
	}
	if part > m.MaxPartSize {
		return errors.New("contentkit: multipart upload exceeds its part limits")
	}
	var parts []uploadPart
	for off, n := int64(0), int32(1); off < size; off, n = off+part, n+1 {
		sum, err := digest(contextSeeker{ctx, body}, off, min(part, size-off))
		if err != nil {
			return err
		}
		parts = append(parts, uploadPart{PartBody: media.PartBody{Number: n, Size: min(part, size-off), SHA256: sum}, offset: off})
	}
	landed := make(map[int32]bool)
	var loaded int64
	if resume {
		stored, err := c.PostMediaUploadPartsList(ctx, media.TicketBody{Ticket: m.Ticket})
		if err != nil {
			var e *Error
			if errors.As(err, &e) && e.Code == media.CodeNotFound {
				// A lost completion response can leave an assembled object and
				// no multipart upload. The caller's idempotent complete checks it.
				return nil
			}
			return err
		}
		for _, p := range stored.Parts {
			if p.Number < 1 || int(p.Number) > len(parts) || landed[p.Number] {
				return errors.New("contentkit: multipart resume returned an unknown or duplicate part")
			}
			want := parts[p.Number-1]
			if p.Size != want.Size || p.SHA256 != "" && p.SHA256 != want.SHA256 {
				return errors.New("contentkit: multipart resume does not match this archive")
			}
			landed[p.Number] = true
			loaded += want.Size
		}
	}
	if progress != nil {
		progress(loaded)
	}
	var pending []uploadPart
	for _, p := range parts {
		if !landed[p.Number] {
			pending = append(pending, p)
		}
	}
	for start := 0; start < len(pending); start += 100 {
		batch := pending[start:min(start+100, len(pending))]
		request := media.PartsBody{Ticket: m.Ticket, Parts: make([]media.PartBody, len(batch))}
		for i, p := range batch {
			request.Parts[i] = p.PartBody
		}
		signed, err := c.PostMediaUploadParts(ctx, request)
		if err != nil {
			return err
		}
		requests := make(map[int32]media.RequestReply, len(batch))
		for _, p := range signed.Parts {
			if p.Number < batch[0].Number || p.Number > batch[len(batch)-1].Number || landed[p.Number] || p.Request == nil {
				return errors.New("contentkit: unexpected multipart signature")
			}
			if _, exists := requests[p.Number]; exists {
				return errors.New("contentkit: duplicate multipart signature")
			}
			requests[p.Number] = *p.Request
		}
		if len(requests) != len(batch) {
			return errors.New("contentkit: missing multipart signatures")
		}
		for _, p := range batch {
			r, ok := requests[p.Number]
			if !ok {
				return fmt.Errorf("contentkit: part %d was not presigned", p.Number)
			}
			if err := upload(ctx, bucket, r, body, p.offset, p.Size); err != nil {
				return err
			}
			loaded += p.Size
			if progress != nil {
				progress(loaded)
			}
		}
	}
	return nil
}

type contextSeeker struct {
	ctx context.Context
	io.ReadSeeker
}

func (r contextSeeker) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.ReadSeeker.Read(p)
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
		return &Error{Status: res.StatusCode, Code: "storage", Message: "bucket refused upload: " + http.StatusText(res.StatusCode), RetryAfter: retryAfter(res.Header)}
	}
	_, _ = io.Copy(io.Discard, res.Body)
	return nil
}
