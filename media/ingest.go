package media

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"slices"
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
// server-side import, not a browser upload) into Ref's originals and commits
// it as Name.
type IngestRequest struct {
	Ref  contentref.ContentRef
	Name string
	Type string
	Body io.Reader // read once, sequentially
	Size int64     // expected size, checked when > 0; required when the actor is rate- or quota-limited
	Op   string    // OpInsert (default) or OpReplace
	Meta map[string]any

	// Resume continues a multipart upload a previous Ingest left behind:
	// parts already stored with the same bytes are not uploaded again (the
	// body is still read and hashed). An unknown upload starts afresh.
	Resume *IngestUpload
	// OnUpload receives the multipart upload once created, for the host to
	// persist for Resume. With it set a failed Ingest keeps the upload (the
	// bucket's abort-incomplete rule removes it after a day); without it the
	// upload is aborted.
	OnUpload func(IngestUpload) error

	PartSize    int64 // default IngestPartSize; at least 5 MiB
	Concurrency int   // parallel part uploads; default IngestConcurrency
}

// IngestUpload identifies an in-progress multipart ingest.
type IngestUpload struct {
	Original string `json:"original"` // u-{uuid}
	UploadID string `json:"upload_id"`
}

// IngestResult is the committed original.
type IngestResult struct {
	Original string
	Size     int64
	SHA256   []byte // of the whole file
	Manifest *Manifest
}

// Ingest uploads req.Body into the item's originals (one checksum-bound PUT
// when it fits in one part, else multipart with per-part SHA-256 and retries)
// and commits it with Commit, which re-checks the upload and enqueues
// processing. Every check Commit makes on a browser upload applies.
func (u *Uploads) Ingest(ctx context.Context, actor access.Actor, req IngestRequest) (IngestResult, error) {
	item, err := u.item(req.Ref)
	if err != nil {
		return IngestResult{}, err
	}
	if _, err := item.Section(); err != nil {
		return IngestResult{}, uploadErr(CodeInvalid, "%v", err)
	}
	if req.Op == "" {
		req.Op = OpInsert
	}
	if req.Name == "" || req.Type == "" || req.Body == nil || (req.Op != OpInsert && req.Op != OpReplace) || req.Size < 0 {
		return IngestResult{}, uploadErr(CodeInvalid, "ingest needs a name, type, body and an insert or replace op")
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
	if err := item.Kind().Allows(req.Type, req.Size); err != nil {
		return IngestResult{}, err
	}
	grant, err := u.authorize(ctx, actor, req.Ref)
	if err != nil {
		return IngestResult{}, err
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

	var name string
	if single {
		sum := sha256.Sum256(first)
		name = SHA256Name(sum[:])
	} else if req.Resume != nil && layout.ValidSourceName(req.Resume.Original) {
		name = req.Resume.Original
	} else {
		name = NewUploadName()
	}
	key, _ := item.Original(name)
	if limited {
		if err := u.o.Limiter.Reserve(ctx, Reservation{Tenant: req.Ref.TenantID, Uploader: uploaderID(actor),
			Owner: grant.Owner, Key: key, Size: req.Size}); err != nil {
			return IngestResult{}, err
		}
	}
	res, err := u.ingest(ctx, item, key, name, req, first, single)
	if err != nil {
		if limited {
			_ = u.o.Limiter.Settle(context.WithoutCancel(ctx), Settlement{Tenant: req.Ref.TenantID, Keys: []string{key}})
		}
		return IngestResult{}, err
	}
	man, err := u.Commit(ctx, actor, req.Ref, []Op{{Op: req.Op, Name: req.Name, Original: name, Meta: req.Meta}})
	if err != nil {
		return IngestResult{}, err
	}
	res.Manifest = man
	return res, nil
}

func (u *Uploads) ingest(ctx context.Context, item Item, key, name string, req IngestRequest, first []byte, single bool) (IngestResult, error) {
	if single {
		if req.Size > 0 && int64(len(first)) != req.Size {
			return IngestResult{}, uploadErr(CodeInvalid, "read %d bytes; declared %d", len(first), req.Size)
		}
		sum := sha256.Sum256(first)
		if _, err := u.o.Store.Put(ctx, key, bytes.NewReader(first), int64(len(first)),
			PutOptions{ContentType: req.Type, ChecksumSHA256: sum[:]}); err != nil {
			return IngestResult{}, err
		}
		return IngestResult{Original: name, Size: int64(len(first)), SHA256: sum[:]}, nil
	}

	var id string
	stored := map[int32]Part{}
	if req.Resume != nil && req.Resume.Original == name {
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
			if err := req.OnUpload(IngestUpload{Original: name, UploadID: id}); err != nil {
				_ = u.o.Store.AbortMultipart(context.WithoutCancel(ctx), key, id)
				return IngestResult{}, err
			}
		}
	}
	res, err := u.ingestParts(ctx, item, key, id, req, first, stored)
	if err != nil {
		if _, invalid := AsUploadError(err); req.OnUpload == nil || invalid {
			_ = u.o.Store.AbortMultipart(context.WithoutCancel(ctx), key, id)
		}
		return IngestResult{}, err
	}
	res.Original = name
	return res, nil
}

// ingestParts reads the body part by part into a bounded buffer pool while
// Concurrency uploaders send them, then completes the upload.
func (u *Uploads) ingestParts(ctx context.Context, item Item, key, id string, req IngestRequest, first []byte, stored map[int32]Part) (IngestResult, error) {
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

	whole := sha256.New()
	var total int64
	readErr := func() error {
		defer close(jobs)
		buf := first
		for n := int32(1); ; n++ {
			if n > ingestMaxParts {
				return uploadErr(CodeTooLarge, "over %d parts of %d bytes", ingestMaxParts, req.PartSize)
			}
			total += int64(len(buf))
			if err := item.Kind().Allows(req.Type, total); err != nil {
				return err
			}
			if req.Size > 0 && total > req.Size {
				return uploadErr(CodeInvalid, "body exceeds the declared %d bytes", req.Size)
			}
			whole.Write(buf)
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
		return IngestResult{}, readErr
	}
	if err := context.Cause(ctx); err != nil {
		return IngestResult{}, err
	}
	if req.Size > 0 && total != req.Size {
		return IngestResult{}, uploadErr(CodeInvalid, "read %d bytes; declared %d", total, req.Size)
	}
	slices.SortFunc(parts, func(a, b Part) int { return int(a.Number - b.Number) })
	if _, err := u.o.Store.CompleteMultipart(ctx, key, id, parts); err != nil {
		return IngestResult{}, err
	}
	return IngestResult{Size: total, SHA256: whole.Sum(nil)}, nil
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
