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
	"time"
)

var (
	ErrNotFound           = errors.New("media: object not found")
	ErrPreconditionFailed = errors.New("media: precondition failed")
	ErrNotModified        = errors.New("media: not modified")
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

	PresignPut(ctx context.Context, key string, p PresignPut) (PresignedRequest, error)
	CreateMultipart(ctx context.Context, key, contentType string) (uploadID string, err error)
	PresignPart(ctx context.Context, key, uploadID string, number int32, size int64, sha256 []byte, ttl time.Duration) (PresignedRequest, error)
	ListParts(ctx context.Context, key, uploadID string) ([]Part, error)
	CompleteMultipart(ctx context.Context, key, uploadID string, parts []Part) (Object, error)
	AbortMultipart(ctx context.Context, key, uploadID string) error

	Capabilities() Capabilities
}

// Probe measures the backend's capabilities with scratch objects under prefix,
// which it removes. Results depend on the backend release (RGW vs MinIO).
func Probe(ctx context.Context, s Store, prefix string) (Capabilities, error) {
	var c Capabilities
	key := prefix + "probe-" + NewUploadName()
	defer func() { _ = s.Delete(context.WithoutCancel(ctx), key) }()
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
	c.ConditionalPut = errors.Is(createErr, ErrPreconditionFailed) && errors.Is(staleErr, ErrPreconditionFailed) && matchErr == nil
	wrong := sha256.Sum256([]byte("not the body"))
	_, badErr := put("e", PutOptions{ChecksumSHA256: wrong[:]})
	good := sha256.Sum256([]byte("f"))
	_, goodErr := put("f", PutOptions{ChecksumSHA256: good[:]})
	c.ChecksumSHA256 = badErr != nil && goodErr == nil
	return c, nil
}
