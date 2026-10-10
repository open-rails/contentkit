package media

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"path"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media/layout"
)

// Ingest defaults: parts are buffered in memory, so memory is about
// (IngestConcurrency+1) × IngestPartSize.
const (
	IngestPartSize    = 32 << 20
	IngestConcurrency = 3
	ingestMinPart     = 5 << 20 // S3's minimum for every part but the last
	ingestMaxParts    = 10000
	ingestAttempts    = 5
)

// IngestRequest streams one file of unknown or huge size from the host (a
// server-side import, not a browser upload) into Ref and puts it at Path.
type IngestRequest struct {
	Ref  contentref.ContentRef
	Path string
	Type string
	Body io.Reader // read once, sequentially
	Size int64     // expected size, checked when > 0; required when the actor is rate- or quota-limited
	Meta map[string]any

	// Resume continues a multipart write a previous Ingest left behind:
	// parts already stored with the same bytes are not uploaded again (the
	// body is still read and hashed). An unknown upload starts afresh.
	Resume *IngestUpload
	// OnUpload receives the multipart write once created, for the host to
	// persist for Resume. With it set a failed Ingest keeps the upload (the
	// bucket's abort-incomplete rule removes it after a day); without it the
	// upload is aborted.
	OnUpload func(IngestUpload) error

	PartSize    int64 // default IngestPartSize; at least 5 MiB
	Concurrency int   // parallel part uploads; default IngestConcurrency
}

// IngestUpload identifies an in-progress multipart ingest in temp/.
type IngestUpload struct {
	Temp     string `json:"temp"` // the staged name (u-{uuid})
	UploadID string `json:"upload_id"`
}

// IngestResult is the committed upload; Staged is its name in temp/ until
// the worker places it.
type IngestResult struct {
	Staged   string
	Size     int64
	Manifest *Manifest
}

// Ingest writes req.Body to a staged upload in temp/, one checksum-bound
// PUT when it fits in one part, else a multipart write, and commits a put
// of it: the worker places and processes it like a browser upload.
func (u *Uploads) Ingest(ctx context.Context, actor access.Actor, req IngestRequest) (IngestResult, error) {
	item, err := u.item(req.Ref)
	if err != nil {
		return IngestResult{}, err
	}
	if req.Path == "" || req.Type == "" || req.Body == nil || req.Size < 0 {
		return IngestResult{}, uploadErr(CodeInvalid, "ingest needs a path, type and body")
	}
	if req.PartSize == 0 {
		req.PartSize = IngestPartSize
	}
	if req.PartSize < ingestMinPart {
		return IngestResult{}, uploadErr(CodeInvalid, "ingest parts are at least %d bytes", ingestMinPart)
	}
	if req.Concurrency <= 0 {
		req.Concurrency = IngestConcurrency
	}
	g, _, _, _, ok := item.Kind().upload(req.Path)
	if !ok {
		return IngestResult{}, uploadErr(CodeNotFound, "kind %q has no upload path %q", item.Kind().Name, req.Path)
	}
	up := item.Kind().Uploads[g]
	if err := allows(up, req.Type, max(req.Size, 1)); err != nil {
		return IngestResult{}, err
	}
	stem, _ := splitExt(req.Path)
	grant, err := u.authorize(ctx, actor, UploadTarget{Ref: req.Ref, Path: stem})
	if err != nil {
		return IngestResult{}, err
	}
	if !u.o.Store.Capabilities().ConditionalPut {
		return IngestResult{}, ErrConditionalPutRequired
	}
	limited := u.o.Limiter != nil && !grant.Exempt
	if limited && req.Size == 0 {
		return IngestResult{}, uploadErr(CodeInvalid, "a limited uploader must declare the size")
	}

	first := make([]byte, req.PartSize)
	n, err := io.ReadFull(req.Body, first)
	single := errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF)
	if err != nil && !single {
		return IngestResult{}, err
	}
	first = first[:n]
	if n == 0 {
		return IngestResult{}, uploadErr(CodeInvalid, "ingest body is empty")
	}
	temp := NewStaged()
	if req.Resume != nil && layout.ValidStagedName(req.Resume.Temp) {
		temp = req.Resume.Temp
	}
	reserve, _ := item.Staged(temp)
	if req.Resume != nil && temp == req.Resume.Temp {
		if err := u.o.Manifests.checkAllocations(ctx, item, []string{reserve}); err != nil {
			return IngestResult{}, err
		}
	} else if err := u.o.Manifests.journal.allocate(ctx, item, reserve); err != nil {
		return IngestResult{}, err
	}
	if limited {
		if err := u.o.Limiter.Reserve(ctx, Reservation{Tenant: req.Ref.TenantID, Uploader: uploaderID(actor),
			Owner: grant.Owner, Key: reserve, Size: req.Size}); err != nil {
			return IngestResult{}, err
		}
		defer func() {
			_ = u.o.Limiter.Settle(context.WithoutCancel(ctx), Settlement{Tenant: req.Ref.TenantID, Keys: []string{reserve}})
		}()
	}
	var res IngestResult
	if single {
		res, err = u.ingestOne(ctx, up, req, reserve, first)
	} else {
		res, err = u.ingestParts(ctx, up, req, reserve, first)
	}
	if err != nil {
		return IngestResult{}, err
	}
	res.Staged = temp
	man, err := u.Commit(ctx, actor, req.Ref, strings.TrimPrefix(temp, "u-"), []Op{{Op: OpPut, Path: req.Path, Blob: temp, Meta: req.Meta}})
	if err != nil {
		return IngestResult{}, err
	}
	res.Manifest = man
	return res, nil
}

func (u *Uploads) ingestOne(ctx context.Context, up Upload, req IngestRequest, key string, body []byte) (IngestResult, error) {
	if req.Size > 0 && int64(len(body)) != req.Size {
		return IngestResult{}, uploadErr(CodeInvalid, "read %d bytes; declared %d", len(body), req.Size)
	}
	if err := allows(up, req.Type, int64(len(body))); err != nil {
		return IngestResult{}, err
	}
	sum := sha256.Sum256(body)
	if _, err := u.o.Store.Put(ctx, key, bytes.NewReader(body), int64(len(body)), PutOptions{ContentType: req.Type, ChecksumSHA256: sum[:]}); err != nil {
		return IngestResult{}, err
	}
	return IngestResult{Size: int64(len(body))}, nil
}

// ingestParts writes the body to its staged key part by part.
func (u *Uploads) ingestParts(ctx context.Context, up Upload, req IngestRequest, key string, first []byte) (IngestResult, error) {
	var id string
	stored := map[int32]Part{}
	if req.Resume != nil && req.Resume.Temp == path.Base(key) {
		parts, err := u.o.Store.ListParts(ctx, key, req.Resume.UploadID)
		switch {
		case err == nil:
			id = req.Resume.UploadID
			for _, p := range parts {
				stored[p.Number] = p
			}
		case !errors.Is(err, ErrNotFound):
			return IngestResult{}, err
		}
	}
	if id == "" {
		var err error
		if id, err = u.o.Store.CreateMultipart(ctx, key, req.Type); err != nil {
			return IngestResult{}, err
		}
		if req.OnUpload != nil {
			if err := req.OnUpload(IngestUpload{Temp: path.Base(key), UploadID: id}); err != nil {
				_ = u.o.Store.AbortMultipart(context.WithoutCancel(ctx), key, id)
				return IngestResult{}, err
			}
		}
	}
	total, err := u.writeParts(ctx, key, id, up, req, first, stored)
	if err != nil {
		if _, invalid := AsUploadError(err); req.OnUpload == nil || invalid {
			_ = u.o.Store.AbortMultipart(context.WithoutCancel(ctx), key, id)
		}
		return IngestResult{}, err
	}
	return IngestResult{Size: total}, nil
}

// writeParts reads the body part by part into a bounded buffer pool while
// Concurrency uploaders send them, then completes the upload.
func (u *Uploads) writeParts(ctx context.Context, key, id string, up Upload, req IngestRequest, first []byte, stored map[int32]Part) (int64, error) {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	type job struct {
		n   int32
		buf []byte
		sum []byte
	}
	free := make(chan []byte, req.Concurrency+1)
	for range req.Concurrency {
		free <- make([]byte, req.PartSize)
	}
	jobs := make(chan job)
	var (
		mu    sync.Mutex
		parts []Part
		wg    sync.WaitGroup
	)
	for range req.Concurrency {
		wg.Go(func() {
			for j := range jobs {
				p, ok := stored[j.n]
				if !ok || p.Size != int64(len(j.buf)) || !bytes.Equal(p.SHA256, j.sum) {
					var err error
					if p, err = u.putPart(ctx, key, id, j.n, j.buf, j.sum); err != nil {
						cancel(err)
					}
				}
				mu.Lock()
				parts = append(parts, p)
				mu.Unlock()
				free <- j.buf[:cap(j.buf)]
			}
		})
	}
	var total int64
	readErr := func() error {
		defer close(jobs)
		buf := first
		for n := int32(1); ; n++ {
			if n > ingestMaxParts {
				return uploadErr(CodeTooLarge, "over %d parts of %d bytes", ingestMaxParts, req.PartSize)
			}
			total += int64(len(buf))
			if err := allows(up, req.Type, total); err != nil {
				return err
			}
			if req.Size > 0 && total > req.Size {
				return uploadErr(CodeInvalid, "body exceeds the declared %d bytes", req.Size)
			}
			sum := sha256.Sum256(buf)
			select {
			case jobs <- job{n: n, buf: buf, sum: sum[:]}:
			case <-ctx.Done():
				return context.Cause(ctx)
			}
			var next []byte
			select {
			case next = <-free:
			case <-ctx.Done():
				return context.Cause(ctx)
			}
			k, err := io.ReadFull(req.Body, next)
			if k == 0 && (errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)) {
				return nil
			}
			if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
				return err
			}
			buf = next[:k]
		}
	}()
	wg.Wait()
	if readErr != nil {
		return 0, readErr
	}
	if err := context.Cause(ctx); err != nil {
		return 0, err
	}
	if req.Size > 0 && total != req.Size {
		return 0, uploadErr(CodeInvalid, "read %d bytes; declared %d", total, req.Size)
	}
	slices.SortFunc(parts, func(a, b Part) int { return int(a.Number - b.Number) })
	if _, err := u.o.Store.CompleteMultipart(ctx, key, id, parts); err != nil {
		return 0, err
	}
	return total, nil
}

func (u *Uploads) putPart(ctx context.Context, key, id string, n int32, buf, sum []byte) (Part, error) {
	var err error
	for attempt := range ingestAttempts {
		if attempt > 0 {
			select {
			case <-time.After(time.Duration(1<<(attempt-1)) * time.Second):
			case <-ctx.Done():
				return Part{}, context.Cause(ctx)
			}
		}
		var p Part
		if p, err = u.o.Store.PutPart(ctx, key, id, n, bytes.NewReader(buf), int64(len(buf)), sum); err == nil {
			return p, nil
		}
		if ctx.Err() != nil {
			return Part{}, context.Cause(ctx)
		}
	}
	return Part{}, fmt.Errorf("media: ingest part %d of %s after %d attempts: %w", n, key, ingestAttempts, err)
}
