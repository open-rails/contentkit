package mediastore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	media "github.com/open-rails/contentkit/media"
)

var (
	ErrNotFound = errors.New("media: object not found")
)

var (
	ErrPreconditionFailed = errors.New("media: precondition failed")
)

var (
	ErrNotModified = errors.New("media: not modified")
)

var (
	// ErrChecksumMismatch: the backend rejected a PUT whose body does not
	// match its x-amz-checksum-sha256.
	ErrChecksumMismatch = errors.New("media: checksum mismatch")
)

var (
	// ErrNotImplemented: the backend does not support a request feature (501).
	ErrNotImplemented = errors.New("media: not implemented by the store")
)

// EditorReadStore optionally signs an exact administrative preview using a
// browser-reachable storage endpoint. Callers must establish current editor
// authority for source exports, or read-restricted authority for a ready
// derivative or explicitly declared reader asset. This is never an ordinary
// audience delivery capability.
type EditorReadStore interface {
	PresignEditorGet(ctx context.Context, key string, ttl time.Duration) (media.PresignedRequest, error)
}

// Probe measures the backend's capabilities with scratch objects under prefix,
// which it removes. Results depend on the backend release (RGW vs MinIO).
// Every step must succeed or be refused cleanly (412, a checksum mismatch,
// 501); anything else (throttling, timeouts, a proxy's 403, cancellation) is
// an error and nothing is recorded, so a transient failure never reads as a
// missing capability.
func Probe(ctx context.Context, s media.Store, prefix string) (media.Capabilities, error) {
	var c media.Capabilities
	key := prefix + "probe-" + uuid.NewString()
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_ = s.Delete(cleanup, key)
	}()
	put := func(body string, o media.PutOptions) (media.Object, error) {
		return s.Put(ctx, key, bytes.NewReader([]byte(body)), int64(len(body)), o)
	}
	first, err := put("a", media.PutOptions{IfNoneMatch: "*"})
	if err != nil {
		return c, fmt.Errorf("media: probe put: %w", err)
	}
	_, createErr := put("b", media.PutOptions{IfNoneMatch: "*"})
	_, staleErr := put("c", media.PutOptions{IfMatch: `"0123456789abcdef0123456789abcdef"`})
	_, matchErr := put("d", media.PutOptions{IfMatch: first.ETag})
	wrong := sha256.Sum256([]byte("not the body"))
	_, badErr := put("e", media.PutOptions{ChecksumSHA256: wrong[:]})
	good := sha256.Sum256([]byte("f"))
	_, goodErr := put("f", media.PutOptions{ChecksumSHA256: good[:]})
	for _, step := range []struct {
		err   error
		clean []error
	}{
		{createErr, []error{ErrPreconditionFailed, ErrNotImplemented}},
		{staleErr, []error{ErrPreconditionFailed, ErrNotImplemented}},
		// 412 is clean here too: a backend ignoring If-None-Match (Ceph RGW)
		// overwrote the object above, so the first ETag no longer matches.
		{matchErr, []error{ErrPreconditionFailed, ErrNotImplemented}},
		{badErr, []error{ErrChecksumMismatch, ErrNotImplemented}},
		{goodErr, nil},
	} {
		if step.err != nil && !slices.ContainsFunc(step.clean, func(e error) bool { return errors.Is(step.err, e) }) {
			return media.Capabilities{}, fmt.Errorf("media: probe: %w", step.err)
		}
	}
	c.ConditionalPut = errors.Is(createErr, ErrPreconditionFailed) && errors.Is(staleErr, ErrPreconditionFailed) && matchErr == nil
	c.ChecksumSHA256 = errors.Is(badErr, ErrChecksumMismatch)
	return c, nil
}
