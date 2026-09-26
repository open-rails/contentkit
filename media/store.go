package media

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/http"
	"slices"
	"time"
)

var (
	ErrNotFound           = errors.New("media: object not found")
	ErrPreconditionFailed = errors.New("media: precondition failed")
	ErrNotModified        = errors.New("media: not modified")
	// ErrUnavailable marks a store that could not be reached or answered 5xx;
	// HTTP handlers answer 503.
	ErrUnavailable = errors.New("media: store unavailable")
	// ErrChecksumMismatch: the backend rejected a PUT whose body does not
	// match its x-amz-checksum-sha256.
	ErrChecksumMismatch = errors.New("media: checksum mismatch")
	// ErrNotImplemented: the backend does not support a request feature (501).
	ErrNotImplemented = errors.New("media: not implemented by the store")
)

// Object is object metadata. For a ranged Get, Size is the body length and
// ContentRange is set.
type Object struct {
	Key            string
	Size           int64
	ETag           string
	ContentType    string
	CacheControl   string
	ContentRange   string
	LastModified   time.Time
	ChecksumSHA256 []byte            // set when the backend stored a full-object SHA-256
	Metadata       map[string]string // user metadata (x-amz-meta-*), lower-case keys; Head and Get
}

// PutOptions are conditions and headers for Put. IfNoneMatch "*" creates only.
type PutOptions struct {
	ContentType    string
	CacheControl   string
	ChecksumSHA256 []byte
	IfMatch        string
	IfNoneMatch    string
	Metadata       map[string]string
}

// GetOptions: IfNoneMatch returns ErrNotModified on a match; Range is an HTTP Range value.
type GetOptions struct {
	IfNoneMatch string
	Range       string
}

// CopyOptions: IfMatch copies only while the source's ETag is this one
// (ErrPreconditionFailed otherwise).
type CopyOptions struct {
	IfMatch string
}

// PresignedRequest is a request a browser sends as is, with exactly Header.
type PresignedRequest struct {
	Method  string
	URL     string
	Header  http.Header
	Expires time.Time
}

// PresignPut binds a direct upload to its exact type, length and SHA-256.
type PresignPut struct {
	ContentType string
	Size        int64
	SHA256      []byte
	TTL         time.Duration
}

// Part is one uploaded multipart part.
type Part struct {
	Number int32
	Size   int64
	ETag   string
	SHA256 []byte
}

// Capabilities are backend features the library depends on, established by Probe.
type Capabilities struct {
	ConditionalPut bool // If-Match / If-None-Match on PUT
	ChecksumSHA256 bool // x-amz-checksum-sha256 enforced on PUT
}

// Store is the bucket. Keys are built by Item; implementations do not
// interpret them.
type Store interface {
	Put(ctx context.Context, key string, body io.Reader, size int64, opts PutOptions) (Object, error)
	Get(ctx context.Context, key string, opts GetOptions) (io.ReadCloser, Object, error)
	Head(ctx context.Context, key string) (Object, error)
	Delete(ctx context.Context, key string) error
	List(ctx context.Context, prefix string) iter.Seq2[Object, error]
	// Copy copies src to dst server-side, keeping its content type, cache
	// control and metadata; objects past the backend's single-copy limit
	// (5 GiB on S3) copy in parts. The returned Object has dst's size.
	Copy(ctx context.Context, src, dst string, o CopyOptions) (Object, error)

	PresignPut(ctx context.Context, key string, p PresignPut) (PresignedRequest, error)
	// PresignGet is for a worker reading an original over HTTP ranges. Its URL
	// uses the store's internal endpoint and must not be returned to browsers.
	PresignGet(ctx context.Context, key string, ttl time.Duration) (PresignedRequest, error)
	CreateMultipart(ctx context.Context, key, contentType string) (uploadID string, err error)
	PresignPart(ctx context.Context, key, uploadID string, number int32, size int64, sha256 []byte, ttl time.Duration) (PresignedRequest, error)
	// PutPart uploads one part from the server, bound to its length and SHA-256.
	PutPart(ctx context.Context, key, uploadID string, number int32, body io.Reader, size int64, sha256 []byte) (Part, error)
	ListParts(ctx context.Context, key, uploadID string) ([]Part, error)
	CompleteMultipart(ctx context.Context, key, uploadID string, parts []Part) (Object, error)
	AbortMultipart(ctx context.Context, key, uploadID string) error

	Capabilities() Capabilities
	// Check reports whether the backend answers. The first success also
	// establishes Capabilities (Probe under prefix) when they were not given.
	Check(ctx context.Context, prefix string) error
}

// Probe measures the backend's capabilities with scratch objects under prefix,
// which it removes. Results depend on the backend release (RGW vs MinIO).
// Every step must succeed or be refused cleanly (412, a checksum mismatch,
// 501); anything else (throttling, timeouts, a proxy's 403, cancellation) is
// an error and nothing is recorded, so a transient failure never reads as a
// missing capability.
func Probe(ctx context.Context, s Store, prefix string) (Capabilities, error) {
	var c Capabilities
	key := prefix + "probe-" + NewUploadName()
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_ = s.Delete(cleanup, key)
	}()
	put := func(body string, o PutOptions) (Object, error) {
		return s.Put(ctx, key, bytes.NewReader([]byte(body)), int64(len(body)), o)
	}
	first, err := put("a", PutOptions{IfNoneMatch: "*"})
	if err != nil {
		return c, fmt.Errorf("media: probe put: %w", err)
	}
	_, createErr := put("b", PutOptions{IfNoneMatch: "*"})
	_, staleErr := put("c", PutOptions{IfMatch: `"0123456789abcdef0123456789abcdef"`})
	_, matchErr := put("d", PutOptions{IfMatch: first.ETag})
	wrong := sha256.Sum256([]byte("not the body"))
	_, badErr := put("e", PutOptions{ChecksumSHA256: wrong[:]})
	good := sha256.Sum256([]byte("f"))
	_, goodErr := put("f", PutOptions{ChecksumSHA256: good[:]})
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
			return Capabilities{}, fmt.Errorf("media: probe: %w", step.err)
		}
	}
	c.ConditionalPut = errors.Is(createErr, ErrPreconditionFailed) && errors.Is(staleErr, ErrPreconditionFailed) && matchErr == nil
	c.ChecksumSHA256 = errors.Is(badErr, ErrChecksumMismatch)
	return c, nil
}
