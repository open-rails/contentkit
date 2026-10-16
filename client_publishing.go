package contentkit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/publishing"
)

// ArchivePublication uploads one ZIP and waits for its durable publishing
// operation. The Request's archive size and checksum are calculated from File.
type ArchivePublication struct {
	// OperationID is a UUID saved before the first request. Reuse it with the
	// same file and metadata after an interrupted call; leave empty to generate
	// one. OnOperation receives the generated ID before any network request.
	OperationID string
	File        io.ReadSeeker
	Filename    string
	Request     publishing.Request
	// StorageClient sends only presigned storage requests. Do not give it a
	// transport that injects ContentKit credentials. Nil uses a separate,
	// unauthenticated client; the ContentKit client's transport is never reused.
	StorageClient *http.Client
	OnOperation   func(publishing.Operation)
	OnProgress    func(PublishingProgress)
	// PollInterval defaults to two seconds. Cancellation leaves the operation
	// and completed upload parts available for a later call.
	PollInterval time.Duration
}

// PublishingProgress is progress within a stage. Hashing and uploading count
// bytes; processing stages use the server's units, such as rendered pages.
type PublishingProgress struct {
	OperationID string
	Stage       string
	Completed   int64
	Total       int64
}

// PublishArchive prepares metadata, resumes or sends its archive, and waits
// for a ready draft, scheduled release or publication. A scheduled result
// continues toward publication on the server without keeping this call open.
// A failed call still returns its operation
// ID, so a lost response never requires creating a second gallery. A moderation
// hold or revoked authority returns the durable receipt and its typed OperationError.
// Processing continues on the server when the caller's context ends.
func (c *Client) PublishArchive(ctx context.Context, input ArchivePublication) (operation publishing.Operation, resultErr error) {
	// The helper returns a receipt, never a presigned storage capability.
	defer func() { operation.Upload = nil }()
	id := input.OperationID
	if id == "" {
		id = uuid.NewString()
	}
	op := publishing.Operation{ID: id}
	if input.OnOperation != nil {
		input.OnOperation(op)
	}
	if parsed, err := uuid.Parse(id); err != nil || parsed == uuid.Nil || parsed.String() != id {
		return op, errors.New("contentkit: operation ID must be a canonical nonzero UUID")
	}
	if input.File == nil {
		return op, errors.New("contentkit: archive file is required")
	}
	size, err := input.File.Seek(0, io.SeekEnd)
	if err != nil {
		return op, err
	}
	if size <= 0 {
		return op, errors.New("contentkit: archive must not be empty")
	}
	emitProgress := func(stage string, completed, total int64) {
		if input.OnProgress != nil {
			input.OnProgress(PublishingProgress{OperationID: id, Stage: stage, Completed: completed, Total: total})
		}
	}
	emitProgress("hashing", 0, size)
	sum, err := digest(contextSeeker{ctx, input.File}, 0, size)
	if err != nil {
		return op, err
	}
	emitProgress("hashing", size, size)
	req := input.Request
	if input.Filename != "" {
		req.Archive.Name = input.Filename
	}
	if req.Archive.Name == "" {
		req.Archive.Name = "archive.zip"
	}
	if req.Archive.Size != 0 && req.Archive.Size != size || req.Archive.SHA256 != "" && req.Archive.SHA256 != sum {
		return op, errors.New("contentkit: archive declaration does not match file")
	}
	req.Archive.Size, req.Archive.SHA256 = size, sum
	// Freeze maps and slices once: every transport retry uses the same input.
	raw, err := json.Marshal(req)
	if err != nil {
		return op, err
	}
	var frozen publishing.Request
	if err := json.Unmarshal(raw, &frozen); err != nil {
		return op, err
	}
	req = frozen
	record := func(next publishing.Operation) {
		if next.ID == "" {
			next.ID = id
		}
		op = next
		if input.OnOperation != nil {
			copy := next
			copy.Upload = nil // upload capabilities are not progress receipts
			input.OnOperation(copy)
		}
		if next.Progress.Stage != "" {
			emitProgress(next.Progress.Stage, int64(next.Progress.Completed), int64(next.Progress.Total))
		}
	}
	prepare := func() error {
		next, err := c.PutPublishingOperation(ctx, id, req)
		if err == nil {
			record(next)
		}
		return err
	}
	if err := retryPublishing(ctx, prepare); err != nil {
		return op, err
	}
	storage := publicationStorageClient(input.StorageClient)
	if op.State == publishing.AwaitingUpload {
		// A refreshed plan still belongs to the same immutable operation.
		// Multipart failures preserve the landed parts and retry their listing.
		for refresh := 0; ; refresh++ {
			if op.Upload == nil {
				return op, errors.New("contentkit: publication has no archive upload plan")
			}
			plan := *op.Upload
			err = retryPublishing(ctx, func() error {
				switch {
				case plan.Exists:
					emitProgress("uploading", size, size)
					return nil
				case plan.Put != nil:
					emitProgress("uploading", 0, size)
					if err := upload(ctx, storage, *plan.Put, input.File, 0, size); err != nil {
						return err
					}
					emitProgress("uploading", size, size)
					return nil
				case plan.Multipart != nil:
					return c.transferParts(ctx, storage, *plan.Multipart, input.File, size, true, func(n int64) {
						emitProgress("uploading", n, size)
					})
				default:
					return errors.New("contentkit: publication planned no archive upload")
				}
			})
			if err == nil {
				err = retryPublishing(ctx, func() error {
					next, err := c.PostPublishingOperationComplete(ctx, id, publishing.CompleteRequest{})
					if err == nil {
						record(next)
					}
					return err
				})
			}
			if err == nil {
				break
			}
			var api *Error
			if refresh > 0 || !errors.As(err, &api) || !(api.Code == media.CodeNotUploaded || api.Code == "storage" && api.Status == http.StatusForbidden) {
				return op, err
			}
			if err := retryPublishing(ctx, prepare); err != nil {
				return op, err
			}
			if op.State != publishing.AwaitingUpload {
				break
			}
		}
	}
	interval := input.PollInterval
	if interval <= 0 {
		interval = 2 * time.Second
	}
	for {
		switch op.State {
		case publishing.Published, publishing.Ready, publishing.Scheduled:
			return op, nil
		case publishing.Failed, publishing.Blocked, publishing.ReviewRequired:
			if op.Error != nil {
				return op, op.Error
			}
			return op, &publishing.OperationError{Code: string(op.State), Message: "publication " + string(op.State)}
		case publishing.Processing, publishing.Publishing:
		default:
			return op, fmt.Errorf("contentkit: unknown publishing state %q", op.State)
		}
		if err := waitPublishing(ctx, interval); err != nil {
			return op, err
		}
		if err := retryPublishing(ctx, func() error {
			next, err := c.GetPublishingOperation(ctx, id)
			if err == nil {
				record(next)
			}
			return err
		}); err != nil {
			return op, err
		}
	}
}

func publicationStorageClient(client *http.Client) *http.Client {
	if client == nil {
		client = &http.Client{}
	}
	copy := *client
	// Presigned capabilities are for their exact target. Never follow storage
	// redirects to a new destination, even if a custom client normally does.
	copy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	copy.Jar = nil
	return &copy
}

func retryPublishing(ctx context.Context, call func() error) error {
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := call()
		if err == nil || attempt == 3 || !publishingTransient(err) {
			return err
		}
		wait := time.Duration(1<<attempt) * 250 * time.Millisecond
		var api *Error
		if errors.As(err, &api) {
			wait = max(wait, api.RetryAfter)
		}
		if err := waitPublishing(ctx, wait); err != nil {
			return err
		}
	}
}

func publishingTransient(err error) bool {
	if errors.Is(err, context.Canceled) {
		return false
	}
	var api *Error
	if errors.As(err, &api) {
		if api.Code == media.CodeNotUploaded {
			// The next useful action is renewing the upload plan, not repeating
			// completion against bytes that the server has already found absent.
			return false
		}
		return api.Retryable || api.Status == http.StatusRequestTimeout || api.Status == http.StatusTooManyRequests || api.Status >= 500
	}
	var network *url.Error
	return errors.As(err, &network) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

func waitPublishing(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
