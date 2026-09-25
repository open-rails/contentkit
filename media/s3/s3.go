// Package s3 implements media.Store over aws-sdk-go-v2 for Ceph RGW
// (production) and MinIO (tests). It is the only package importing the SDK.
package s3

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"github.com/open-rails/helpers/deps"

	"github.com/open-rails/contentkit/media"
)

// Config configures one bucket.
type Config struct {
	Bucket          string
	Region          string // RGW zonegroup; default us-east-1
	Endpoint        string // e.g. http://rook-ceph-rgw.svc:80
	PublicEndpoint  string // presign host browsers reach, e.g. https://s3.doujins.ai; default Endpoint
	AccessKeyID     string
	SecretAccessKey string
	UsePathStyle    bool
	// Capabilities of the backend as recorded for its release. Nil: Check
	// probes them once the bucket answers; until then the store claims none
	// (edits write unconditionally under the Locker, uploads rehash).
	Capabilities *media.Capabilities
	// CopyPartSize is the part size of a multipart copy, used for objects
	// larger than it; default MaxSingleCopy (tests set a few MiB).
	CopyPartSize int64
}

// MaxSingleCopy is S3's CopyObject limit; larger objects copy in parts.
const MaxSingleCopy = 5 << 30

// Store is a media.Store over one bucket.
type Store struct {
	client   *s3.Client
	creds    aws.CredentialsProvider
	signer   *v4.Signer
	bucket   string
	region   string
	internal *url.URL
	public   *url.URL
	pathSty  bool
	caps     atomic.Pointer[media.Capabilities]
	probed   atomic.Bool
	part     int64
}

var _ media.Store = (*Store)(nil)

// MaxPresignTTL is the SigV4 validity cap.
const MaxPresignTTL = 7 * 24 * time.Hour

func New(cfg Config) (*Store, error) {
	if cfg.Bucket == "" || cfg.Endpoint == "" {
		return nil, errors.New("s3: Bucket and Endpoint are required")
	}
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	if cfg.PublicEndpoint == "" {
		cfg.PublicEndpoint = cfg.Endpoint
	}
	if cfg.CopyPartSize <= 0 || cfg.CopyPartSize > MaxSingleCopy {
		cfg.CopyPartSize = MaxSingleCopy
	}
	internal, err := url.Parse(strings.TrimRight(cfg.Endpoint, "/"))
	if err != nil || (internal.Scheme != "http" && internal.Scheme != "https") || internal.Host == "" {
		return nil, fmt.Errorf("s3: invalid Endpoint %q", cfg.Endpoint)
	}
	pub, err := url.Parse(strings.TrimRight(cfg.PublicEndpoint, "/"))
	if err != nil || pub.Scheme == "" || pub.Host == "" {
		return nil, fmt.Errorf("s3: invalid PublicEndpoint %q", cfg.PublicEndpoint)
	}
	creds := credentials.NewStaticCredentialsProvider(cfg.AccessKeyID, cfg.SecretAccessKey, "")
	client := s3.New(s3.Options{
		Region:       cfg.Region,
		Credentials:  creds,
		BaseEndpoint: aws.String(cfg.Endpoint),
		UsePathStyle: cfg.UsePathStyle,
		// Only send checksums the caller supplies: RGW and MinIO reject or
		// mishandle the SDK's default trailing CRC32 on some releases.
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
	})
	st := &Store{client: client, creds: creds, signer: v4.NewSigner(), bucket: cfg.Bucket, region: cfg.Region,
		internal: internal, public: pub, pathSty: cfg.UsePathStyle, part: cfg.CopyPartSize}
	caps := media.Capabilities{}
	if cfg.Capabilities != nil {
		caps = *cfg.Capabilities
	}
	st.caps.Store(&caps)
	st.probed.Store(cfg.Capabilities != nil)
	return st, nil
}

func (s *Store) Capabilities() media.Capabilities { return *s.caps.Load() }

// Check reports whether the bucket answers, probing the backend's
// capabilities (media.Probe under prefix) until that has succeeded once. New
// never dials, so hosts start without the bucket and run Check as their S3
// dependency probe.
func (s *Store) Check(ctx context.Context, prefix string) error {
	if s.probed.Load() {
		for _, err := range s.List(ctx, prefix+"_health/") {
			return err
		}
		return nil
	}
	caps, err := media.Probe(ctx, s, prefix)
	if err != nil {
		return err
	}
	s.caps.Store(&caps)
	s.probed.Store(true)
	return nil
}

// Client exposes the SDK client for bucket administration and tests.
func (s *Store) Client() *s3.Client { return s.client }
func (s *Store) Bucket() string     { return s.bucket }

func (s *Store) Put(ctx context.Context, key string, body io.Reader, size int64, o media.PutOptions) (media.Object, error) {
	in := &s3.PutObjectInput{Bucket: &s.bucket, Key: &key, Body: body, ContentLength: &size}
	in.ContentType = optional(o.ContentType)
	in.CacheControl = optional(o.CacheControl)
	in.IfMatch = optional(o.IfMatch)
	in.IfNoneMatch = optional(o.IfNoneMatch)
	in.Metadata = o.Metadata
	if o.ChecksumSHA256 != nil {
		in.ChecksumSHA256 = aws.String(base64.StdEncoding.EncodeToString(o.ChecksumSHA256))
	}
	out, err := s.client.PutObject(ctx, in)
	if err != nil {
		return media.Object{}, mapErr("put", key, err)
	}
	return media.Object{Key: key, Size: size, ETag: aws.ToString(out.ETag), ContentType: o.ContentType,
		CacheControl: o.CacheControl, ChecksumSHA256: decode(out.ChecksumSHA256)}, nil
}

func (s *Store) Get(ctx context.Context, key string, o media.GetOptions) (io.ReadCloser, media.Object, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: &s.bucket, Key: &key,
		IfNoneMatch: optional(o.IfNoneMatch), Range: optional(o.Range)})
	if err != nil {
		return nil, media.Object{}, mapErr("get", key, err)
	}
	return out.Body, media.Object{Key: key, Size: aws.ToInt64(out.ContentLength), ETag: aws.ToString(out.ETag),
		ContentType: aws.ToString(out.ContentType), CacheControl: aws.ToString(out.CacheControl),
		ContentRange: aws.ToString(out.ContentRange), LastModified: aws.ToTime(out.LastModified), Metadata: out.Metadata}, nil
}

// Head returns ErrNotFound for a missing key. Some backends answer 403 instead
// when the caller lacks ListBucket; host keys must have it.
func (s *Store) Head(ctx context.Context, key string) (media.Object, error) {
	out, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &s.bucket, Key: &key, ChecksumMode: types.ChecksumModeEnabled})
	if err != nil {
		return media.Object{}, mapErr("head", key, err)
	}
	return media.Object{Key: key, Size: aws.ToInt64(out.ContentLength), ETag: aws.ToString(out.ETag),
		ContentType: aws.ToString(out.ContentType), CacheControl: aws.ToString(out.CacheControl),
		LastModified: aws.ToTime(out.LastModified), ChecksumSHA256: fullObject(out.ChecksumSHA256, out.ChecksumType),
		Metadata: out.Metadata}, nil
}

func (s *Store) Delete(ctx context.Context, key string) error {
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &s.bucket, Key: &key})
	return mapErr("delete", key, err)
}

func (s *Store) List(ctx context.Context, prefix string) iter.Seq2[media.Object, error] {
	return func(yield func(media.Object, error) bool) {
		p := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{Bucket: &s.bucket, Prefix: &prefix})
		for p.HasMorePages() {
			page, err := p.NextPage(ctx)
			if err != nil {
				yield(media.Object{}, mapErr("list", prefix, err))
				return
			}
			for _, o := range page.Contents {
				if !yield(media.Object{Key: aws.ToString(o.Key), Size: aws.ToInt64(o.Size), ETag: aws.ToString(o.ETag),
					LastModified: aws.ToTime(o.LastModified)}, nil) {
					return
				}
			}
		}
	}
}

// Copy copies src to dst in the bucket: one CopyObject up to the copy part
// size, else UploadPartCopy ranges of it, each conditional on the source ETag.
func (s *Store) Copy(ctx context.Context, src, dst string, o media.CopyOptions) (media.Object, error) {
	head, err := s.Head(ctx, src)
	if err != nil {
		return media.Object{}, err
	}
	if o.IfMatch != "" && head.ETag != o.IfMatch {
		return media.Object{}, fmt.Errorf("%w: s3 copy %s: source changed", media.ErrPreconditionFailed, src)
	}
	source := s.bucket + "/" + src
	ifMatch := optional(head.ETag)
	if head.Size <= s.part {
		out, err := s.client.CopyObject(ctx, &s3.CopyObjectInput{Bucket: &s.bucket, Key: &dst, CopySource: &source, CopySourceIfMatch: ifMatch})
		if err != nil {
			return media.Object{}, mapErr("copy", src, err)
		}
		var etag string
		if out.CopyObjectResult != nil {
			etag = aws.ToString(out.CopyObjectResult.ETag)
		}
		return media.Object{Key: dst, Size: head.Size, ETag: etag, ContentType: head.ContentType, CacheControl: head.CacheControl}, nil
	}
	up, err := s.client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: &s.bucket, Key: &dst,
		ContentType: optional(head.ContentType), CacheControl: optional(head.CacheControl), Metadata: head.Metadata})
	if err != nil {
		return media.Object{}, mapErr("copy", dst, err)
	}
	abort := func(err error) (media.Object, error) {
		_ = s.AbortMultipart(context.WithoutCancel(ctx), dst, aws.ToString(up.UploadId))
		return media.Object{}, err
	}
	var parts []types.CompletedPart
	for n, off := int32(1), int64(0); off < head.Size; n, off = n+1, off+s.part {
		end := min(off+s.part, head.Size) - 1
		out, err := s.client.UploadPartCopy(ctx, &s3.UploadPartCopyInput{Bucket: &s.bucket, Key: &dst, UploadId: up.UploadId,
			PartNumber: aws.Int32(n), CopySource: &source, CopySourceIfMatch: ifMatch,
			CopySourceRange: aws.String("bytes=" + strconv.FormatInt(off, 10) + "-" + strconv.FormatInt(end, 10))})
		if err != nil {
			return abort(mapErr("copy part", src, err))
		}
		if out.CopyPartResult == nil {
			return abort(fmt.Errorf("s3 copy part %s: no result", src))
		}
		parts = append(parts, types.CompletedPart{PartNumber: aws.Int32(n), ETag: out.CopyPartResult.ETag})
	}
	out, err := s.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{Bucket: &s.bucket, Key: &dst,
		UploadId: up.UploadId, MultipartUpload: &types.CompletedMultipartUpload{Parts: parts}})
	if err != nil {
		return abort(mapErr("complete copy", dst, err))
	}
	return media.Object{Key: dst, Size: head.Size, ETag: aws.ToString(out.ETag), ContentType: head.ContentType, CacheControl: head.CacheControl}, nil
}

// PresignPut signs Content-Type, Content-Length and x-amz-checksum-sha256, so
// the store rejects any other type, length or body.
func (s *Store) PresignPut(ctx context.Context, key string, p media.PresignPut) (media.PresignedRequest, error) {
	if p.ContentType == "" || p.Size <= 0 || len(p.SHA256) != 32 {
		return media.PresignedRequest{}, errors.New("s3: presign put needs a content type, size and SHA-256")
	}
	h := http.Header{}
	h.Set("Content-Type", p.ContentType)
	h.Set("X-Amz-Checksum-Sha256", base64.StdEncoding.EncodeToString(p.SHA256))
	return s.presign(ctx, s.public, http.MethodPut, key, nil, h, p.Size, p.TTL)
}

// PresignGet gives the media worker a read-only URL on the internal S3
// endpoint. Range is deliberately not signed, so ffprobe and ffmpeg can seek.
func (s *Store) PresignGet(ctx context.Context, key string, ttl time.Duration) (media.PresignedRequest, error) {
	return s.presign(ctx, s.internal, http.MethodGet, key, nil, nil, -1, ttl)
}

func (s *Store) CreateMultipart(ctx context.Context, key, contentType string) (string, error) {
	out, err := s.client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: &s.bucket, Key: &key,
		ContentType: optional(contentType), ChecksumAlgorithm: types.ChecksumAlgorithmSha256})
	if err != nil {
		return "", mapErr("create multipart", key, err)
	}
	return aws.ToString(out.UploadId), nil
}

// PresignPart binds one part to its exact length and SHA-256.
func (s *Store) PresignPart(ctx context.Context, key, uploadID string, number int32, size int64, sum []byte, ttl time.Duration) (media.PresignedRequest, error) {
	if uploadID == "" || number < 1 || number > 10000 || size <= 0 || len(sum) != 32 {
		return media.PresignedRequest{}, errors.New("s3: presign part needs an upload id, part 1-10000, size and SHA-256")
	}
	q := url.Values{"partNumber": {strconv.Itoa(int(number))}, "uploadId": {uploadID}}
	h := http.Header{}
	h.Set("X-Amz-Checksum-Sha256", base64.StdEncoding.EncodeToString(sum))
	return s.presign(ctx, s.public, http.MethodPut, key, q, h, size, ttl)
}

func (s *Store) PutPart(ctx context.Context, key, uploadID string, number int32, body io.Reader, size int64, sum []byte) (media.Part, error) {
	if uploadID == "" || number < 1 || number > 10000 || size <= 0 || len(sum) != 32 {
		return media.Part{}, errors.New("s3: put part needs an upload id, part 1-10000, size and SHA-256")
	}
	out, err := s.client.UploadPart(ctx, &s3.UploadPartInput{Bucket: &s.bucket, Key: &key, UploadId: &uploadID,
		PartNumber: aws.Int32(number), Body: body, ContentLength: aws.Int64(size),
		ChecksumSHA256: aws.String(base64.StdEncoding.EncodeToString(sum))})
	if err != nil {
		return media.Part{}, mapErr("upload part", key, err)
	}
	return media.Part{Number: number, Size: size, ETag: aws.ToString(out.ETag), SHA256: sum}, nil
}

func (s *Store) ListParts(ctx context.Context, key, uploadID string) ([]media.Part, error) {
	var parts []media.Part
	p := s3.NewListPartsPaginator(s.client, &s3.ListPartsInput{Bucket: &s.bucket, Key: &key, UploadId: &uploadID})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, mapErr("list parts", key, err)
		}
		for _, pt := range page.Parts {
			parts = append(parts, media.Part{Number: aws.ToInt32(pt.PartNumber), Size: aws.ToInt64(pt.Size),
				ETag: aws.ToString(pt.ETag), SHA256: decode(pt.ChecksumSHA256)})
		}
	}
	return parts, nil
}

func (s *Store) CompleteMultipart(ctx context.Context, key, uploadID string, parts []media.Part) (media.Object, error) {
	done := make([]types.CompletedPart, len(parts))
	for i, p := range parts {
		done[i] = types.CompletedPart{PartNumber: aws.Int32(p.Number), ETag: aws.String(p.ETag)}
		if p.SHA256 != nil {
			done[i].ChecksumSHA256 = aws.String(base64.StdEncoding.EncodeToString(p.SHA256))
		}
	}
	out, err := s.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{Bucket: &s.bucket, Key: &key,
		UploadId: &uploadID, MultipartUpload: &types.CompletedMultipartUpload{Parts: done}})
	if err != nil {
		return media.Object{}, mapErr("complete multipart", key, err)
	}
	return media.Object{Key: key, ETag: aws.ToString(out.ETag)}, nil
}

func (s *Store) AbortMultipart(ctx context.Context, key, uploadID string) error {
	_, err := s.client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{Bucket: &s.bucket, Key: &key, UploadId: &uploadID})
	return mapErr("abort multipart", key, err)
}

// Configure applies the media bucket policy: versioning on, noncurrent
// versions kept restoreDays, incomplete multipart uploads aborted after 1 day,
// and unservable video work fragments expired after 7 days.
func (s *Store) Configure(ctx context.Context, restoreDays int32) error {
	if _, err := s.client.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{Bucket: &s.bucket,
		VersioningConfiguration: &types.VersioningConfiguration{Status: types.BucketVersioningStatusEnabled}}); err != nil {
		return fmt.Errorf("s3: enable versioning: %w", err)
	}
	_, err := s.client.PutBucketLifecycleConfiguration(ctx, &s3.PutBucketLifecycleConfigurationInput{Bucket: &s.bucket,
		LifecycleConfiguration: &types.BucketLifecycleConfiguration{Rules: []types.LifecycleRule{{
			ID:                             aws.String("contentkit-media"),
			Status:                         types.ExpirationStatusEnabled,
			Filter:                         &types.LifecycleRuleFilter{Prefix: aws.String("")},
			NoncurrentVersionExpiration:    &types.NoncurrentVersionExpiration{NoncurrentDays: aws.Int32(restoreDays)},
			AbortIncompleteMultipartUpload: &types.AbortIncompleteMultipartUpload{DaysAfterInitiation: aws.Int32(1)},
		}, {
			ID:         aws.String("contentkit-video-work"),
			Status:     types.ExpirationStatusEnabled,
			Filter:     &types.LifecycleRuleFilter{Prefix: aws.String("work/")},
			Expiration: &types.LifecycleExpiration{Days: aws.Int32(7)},
		}}}})
	if err != nil {
		return fmt.Errorf("s3: put lifecycle: %w", err)
	}
	return nil
}

// presign signs with header hoisting disabled so every header in h (and the
// length) is a signed header the client must send verbatim. The SDK
// presigner would drop or hoist them.
func (s *Store) presign(ctx context.Context, base *url.URL, method, key string, q url.Values, h http.Header, size int64, ttl time.Duration) (media.PresignedRequest, error) {
	if ttl <= 0 || ttl > MaxPresignTTL {
		return media.PresignedRequest{}, fmt.Errorf("s3: presign ttl must be in (0, %s]", MaxPresignTTL)
	}
	u := *base
	if s.pathSty {
		u.Path = u.Path + "/" + s.bucket + "/" + key
	} else {
		u.Host = s.bucket + "." + u.Host
		u.Path = u.Path + "/" + key
	}
	if q == nil {
		q = url.Values{}
	}
	q.Set("X-Amz-Expires", strconv.Itoa(int(ttl/time.Second)))
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, method, u.String(), nil)
	if err != nil {
		return media.PresignedRequest{}, err
	}
	req.Header = h
	if size >= 0 {
		req.ContentLength = size
	}
	creds, err := s.creds.Retrieve(ctx)
	if err != nil {
		return media.PresignedRequest{}, err
	}
	now := time.Now().UTC()
	signed, _, err := s.signer.PresignHTTP(ctx, creds, req, "UNSIGNED-PAYLOAD", "s3", s.region, now,
		func(o *v4.SignerOptions) { o.DisableHeaderHoisting = true })
	if err != nil {
		return media.PresignedRequest{}, fmt.Errorf("s3: presign %s: %w", key, err)
	}
	return media.PresignedRequest{Method: method, URL: signed, Header: h.Clone(), Expires: now.Add(ttl)}, nil
}

func mapErr(op, key string, err error) error {
	if err == nil {
		return nil
	}
	var sentinel error
	var re interface{ HTTPStatusCode() int }
	var api smithy.APIError
	code := ""
	if errors.As(err, &api) {
		code = api.ErrorCode()
	}
	switch {
	case code == "NoSuchKey" || code == "NotFound" || code == "NoSuchUpload":
		sentinel = media.ErrNotFound
	case code == "PreconditionFailed" || code == "ConditionalRequestConflict":
		sentinel = media.ErrPreconditionFailed
	case code == "XAmzContentChecksumMismatch" || code == "BadDigest" || code == "InvalidDigest":
		sentinel = media.ErrChecksumMismatch
	case code == "NotImplemented":
		sentinel = media.ErrNotImplemented
	case errors.As(err, &re):
		switch re.HTTPStatusCode() {
		case http.StatusNotFound:
			if code == "" || code == "NoSuchKey" || code == "NotFound" {
				sentinel = media.ErrNotFound
			}
		case http.StatusPreconditionFailed:
			sentinel = media.ErrPreconditionFailed
		case http.StatusNotModified:
			sentinel = media.ErrNotModified
		case http.StatusNotImplemented:
			sentinel = media.ErrNotImplemented
		default:
			if re.HTTPStatusCode() >= 500 {
				sentinel = media.ErrUnavailable
			}
		}
	}
	if sentinel == nil && deps.IsConnectivity(err) {
		return fmt.Errorf("%w: s3 %s %s: %w", media.ErrUnavailable, op, key, err)
	}
	if sentinel != nil {
		return fmt.Errorf("%w: s3 %s %s", sentinel, op, key)
	}
	return fmt.Errorf("s3 %s %s: %w", op, key, err)
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func decode(b64 *string) []byte {
	if b64 == nil {
		return nil
	}
	b, err := base64.StdEncoding.DecodeString(*b64)
	if err != nil || len(b) != 32 {
		return nil
	}
	return b
}

// fullObject keeps only a full-object SHA-256, not a composite "hash-N".
func fullObject(b64 *string, t types.ChecksumType) []byte {
	if t == types.ChecksumTypeComposite {
		return nil
	}
	return decode(b64)
}
