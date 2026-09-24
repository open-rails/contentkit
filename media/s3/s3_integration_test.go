package s3_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"io"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/internal/s3test"
	mediaS3 "github.com/open-rails/contentkit/media/s3"
	"github.com/open-rails/contentkit/media/token"
)

func send(t *testing.T, p media.PresignedRequest, body []byte, override http.Header) *http.Response {
	t.Helper()
	req, err := http.NewRequest(p.Method, p.URL, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header = p.Header.Clone()
	for k, v := range override {
		req.Header[k] = v
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp
}

func random(t *testing.T, n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

func TestPresignedPutIsBoundToTypeLengthAndChecksum(t *testing.T) {
	env := s3test.Open(t)
	ctx := context.Background()
	body := random(t, 4096)
	sum := sha256.Sum256(body)
	key := env.Tenant + "/gallery/1/originals/" + media.SHA256Name(sum[:])
	p, err := env.Store.PresignPut(ctx, key, media.PresignPut{ContentType: "image/png", Size: int64(len(body)), SHA256: sum[:], TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}

	wrongBody := slices.Clone(body)
	wrongBody[0] ^= 0xff
	if r := send(t, p, wrongBody, nil); r.StatusCode < 400 {
		if env.Store.Capabilities().ChecksumSHA256 {
			t.Fatalf("wrong body accepted: %d", r.StatusCode)
		}
		t.Logf("backend accepted a body not matching the signed checksum (%d): the commit re-hash fallback applies", r.StatusCode)
	}
	if r := send(t, p, body, http.Header{"Content-Type": {"image/jpeg"}}); r.StatusCode != http.StatusForbidden {
		t.Fatalf("wrong type: %d", r.StatusCode)
	}
	if r := send(t, p, body[:len(body)-1], nil); r.StatusCode != http.StatusForbidden {
		t.Fatalf("wrong length: %d", r.StatusCode)
	}
	if r := send(t, p, body, nil); r.StatusCode != http.StatusOK {
		t.Fatalf("exact upload: %d", r.StatusCode)
	}
	obj, err := env.Store.Head(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if obj.Size != int64(len(body)) || obj.ContentType != "image/png" {
		t.Fatalf("head %+v", obj)
	}
	if env.Store.Capabilities().ChecksumSHA256 && !bytes.Equal(obj.ChecksumSHA256, sum[:]) {
		t.Fatalf("stored checksum %x, want %x", obj.ChecksumSHA256, sum)
	}
}

func TestConditionalWritesAndReads(t *testing.T) {
	env := s3test.Open(t)
	if !env.Store.Capabilities().ConditionalPut {
		t.Skip("backend lacks conditional PUT: manifests use the advisory-lock fallback")
	}
	ctx := context.Background()
	key := env.Tenant + "/post/1/manifest.json"
	put := func(body string, o media.PutOptions) (media.Object, error) {
		return env.Store.Put(ctx, key, bytes.NewReader([]byte(body)), int64(len(body)), o)
	}
	first, err := put("1", media.PutOptions{IfNoneMatch: "*"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := put("2", media.PutOptions{IfNoneMatch: "*"}); !errors.Is(err, media.ErrPreconditionFailed) {
		t.Fatalf("create over existing: %v", err)
	}
	second, err := put("3", media.PutOptions{IfMatch: first.ETag})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := put("4", media.PutOptions{IfMatch: first.ETag}); !errors.Is(err, media.ErrPreconditionFailed) {
		t.Fatalf("stale If-Match: %v", err)
	}
	if _, _, err := env.Store.Get(ctx, key, media.GetOptions{IfNoneMatch: second.ETag}); !errors.Is(err, media.ErrNotModified) {
		t.Fatalf("conditional get: %v", err)
	}
	rc, obj, err := env.Store.Get(ctx, key, media.GetOptions{IfNoneMatch: first.ETag})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	rc.Close()
	if string(b) != "3" || obj.ETag != second.ETag {
		t.Fatalf("got %q %s", b, obj.ETag)
	}
}

func TestDirectPutChecksumAndObjectOps(t *testing.T) {
	env := s3test.Open(t)
	ctx := context.Background()
	prefix := env.Tenant + "/video/9/blobs/"
	body := []byte("0123456789")
	sum := sha256.Sum256(body)
	key := prefix + media.SHA256Name(sum[:])
	wrong := sha256.Sum256([]byte("other"))
	_, err := env.Store.Put(ctx, key, bytes.NewReader(body), int64(len(body)), media.PutOptions{ChecksumSHA256: wrong[:]})
	if env.Store.Capabilities().ChecksumSHA256 && err == nil {
		t.Fatal("put with wrong checksum accepted")
	}
	if _, err := env.Store.Put(ctx, key, bytes.NewReader(body), int64(len(body)),
		media.PutOptions{ContentType: "video/mp4", CacheControl: "max-age=31536000, immutable", ChecksumSHA256: sum[:]}); err != nil {
		t.Fatal(err)
	}
	rc, obj, err := env.Store.Get(ctx, key, media.GetOptions{Range: "bytes=2-4"})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	rc.Close()
	if string(b) != "234" || obj.Size != 3 || obj.ContentRange != "bytes 2-4/10" || obj.CacheControl != "max-age=31536000, immutable" {
		t.Fatalf("range get %q %+v", b, obj)
	}
	var keys []string
	for o, err := range env.Store.List(ctx, env.Tenant+"/video/9/") {
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, o.Key)
	}
	if !slices.Equal(keys, []string{key}) {
		t.Fatalf("list %v", keys)
	}
	if err := env.Store.Delete(ctx, key); err != nil {
		t.Fatal(err)
	}
	if _, err := env.Store.Head(ctx, key); !errors.Is(err, media.ErrNotFound) {
		t.Fatalf("head after delete: %v", err)
	}
	if _, _, err := env.Store.Get(ctx, key, media.GetOptions{}); !errors.Is(err, media.ErrNotFound) {
		t.Fatalf("get after delete: %v", err)
	}
}

func TestResponseContentDisposition(t *testing.T) {
	env := s3test.Open(t)
	ctx := context.Background()
	key := env.Tenant + "/gallery/2/blobs/" + media.NewUploadName()
	if _, err := env.Store.Put(ctx, key, bytes.NewReader([]byte("zip")), 3, media.PutOptions{ContentType: "application/zip"}); err != nil {
		t.Fatal(err)
	}
	want := token.Attachment("[Artist] Title (日本語).zip")
	p, err := s3.NewPresignClient(env.Store.Client()).PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(env.Store.Bucket()), Key: &key, ResponseContentDisposition: &want}, s3.WithPresignExpires(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get(p.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Disposition") != want {
		t.Fatalf("%d %q, want %q", resp.StatusCode, resp.Header.Get("Content-Disposition"), want)
	}
}

func TestMultipartPartsAreChecksumBoundAndResumable(t *testing.T) {
	env := s3test.Open(t)
	ctx := context.Background()
	key := env.Tenant + "/video/3/originals/" + media.NewUploadName()
	id, err := env.Store.CreateMultipart(ctx, key, "video/mp4")
	if err != nil {
		t.Fatal(err)
	}
	parts := [][]byte{random(t, 5<<20), random(t, 1234)}
	upload := func(key, id string, n int32, body []byte, sum []byte) *http.Response {
		p, err := env.Store.PresignPart(ctx, key, id, n, int64(len(body)), sum, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		return send(t, p, body, nil)
	}
	wrong := sha256.Sum256([]byte("wrong"))
	if r := upload(key, id, 1, parts[0], wrong[:]); r.StatusCode < 400 {
		if env.Store.Capabilities().ChecksumSHA256 {
			t.Fatalf("part with wrong checksum accepted: %d", r.StatusCode)
		}
		t.Logf("backend accepted a part not matching its checksum (%d)", r.StatusCode)
	}
	for i, b := range parts {
		sum := sha256.Sum256(b)
		if r := upload(key, id, int32(i+1), b, sum[:]); r.StatusCode != http.StatusOK {
			t.Fatalf("part %d: %d", i+1, r.StatusCode)
		}
	}
	// Resume: the server learns what landed from ListParts alone.
	listed, err := env.Store.ListParts(ctx, key, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 2 || listed[0].Size != 5<<20 || listed[1].Size != 1234 {
		t.Fatalf("parts %+v", listed)
	}
	for i, p := range listed {
		sum := sha256.Sum256(parts[i])
		if p.SHA256 != nil && !bytes.Equal(p.SHA256, sum[:]) {
			t.Fatalf("part %d checksum %x", p.Number, p.SHA256)
		}
	}
	if _, err := env.Store.CompleteMultipart(ctx, key, id, listed); err != nil {
		t.Fatal(err)
	}
	obj, err := env.Store.Head(ctx, key)
	if err != nil || obj.Size != 5<<20+1234 || obj.ContentType != "video/mp4" {
		t.Fatalf("completed %+v %v", obj, err)
	}

	abortKey := env.Tenant + "/video/3/originals/" + media.NewUploadName()
	abortID, err := env.Store.CreateMultipart(ctx, abortKey, "video/mp4")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(parts[1])
	if r := upload(abortKey, abortID, 1, parts[1], sum[:]); r.StatusCode != http.StatusOK {
		t.Fatalf("part: %d", r.StatusCode)
	}
	if err := env.Store.AbortMultipart(ctx, abortKey, abortID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.Store.ListParts(ctx, abortKey, abortID); err == nil {
		t.Fatal("aborted upload still lists parts")
	}
}

func TestBucketVersioningAndLifecycle(t *testing.T) {
	env := s3test.Open(t)
	ctx := context.Background()
	if env.Created {
		if err := env.Store.Configure(ctx, 30); err != nil {
			t.Fatal(err)
		}
	}
	status, err := env.VersioningStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status != types.BucketVersioningStatusEnabled {
		t.Skipf("bucket versioning is %q; enable it (Store.Configure) for the restore window", status)
	}
	key := env.Tenant + "/gallery/5/manifest.json"
	for _, b := range []string{"a", "b"} {
		if _, err := env.Store.Put(ctx, key, bytes.NewReader([]byte(b)), 1, media.PutOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := env.Store.Delete(ctx, key); err != nil {
		t.Fatal(err)
	}
	out, err := env.Store.Client().ListObjectVersions(ctx, &s3.ListObjectVersionsInput{Bucket: aws.String(env.Store.Bucket()), Prefix: &key})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Versions) != 2 || len(out.DeleteMarkers) != 1 {
		t.Fatalf("versions %d, delete markers %d", len(out.Versions), len(out.DeleteMarkers))
	}
	lc, err := env.Store.Client().GetBucketLifecycleConfiguration(ctx, &s3.GetBucketLifecycleConfigurationInput{Bucket: aws.String(env.Store.Bucket())})
	if err != nil {
		t.Fatal(err)
	}
	ok, abort := false, false
	for _, r := range lc.Rules {
		ok = ok || (r.NoncurrentVersionExpiration != nil && aws.ToInt32(r.NoncurrentVersionExpiration.NoncurrentDays) >= 30)
		abort = abort || (r.AbortIncompleteMultipartUpload != nil && aws.ToInt32(r.AbortIncompleteMultipartUpload.DaysAfterInitiation) == 1)
	}
	if !abort {
		// MinIO drops this rule and expires stale uploads itself; RGW keeps it.
		if s3test.Require("abort-lifecycle") {
			t.Fatal("AbortIncompleteMultipartUpload rule missing")
		}
		t.Log("backend does not store AbortIncompleteMultipartUpload")
	}
	if !ok {
		t.Fatalf("lifecycle rules %+v", lc.Rules)
	}
}

// Copy keeps the type and metadata, copies objects past the part size in
// ranged parts (5 MiB here, 5 GiB in production), and honours IfMatch.
func TestCopyInOneRequestOrInParts(t *testing.T) {
	env := s3test.Open(t)
	ctx := context.Background()
	cfg := env.Config
	cfg.CopyPartSize = 5 << 20
	store, err := mediaS3.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, size := range []int{1 << 10, 12<<20 + 7} {
		body := random(t, size)
		src := env.Tenant + "/video/9/staging/" + media.NewUploadName()
		sum := sha256.Sum256(body)
		dst := env.Tenant + "/video/9/originals/" + media.SHA256Name(sum[:])
		put, err := store.Put(ctx, src, bytes.NewReader(body), int64(size),
			media.PutOptions{ContentType: "video/mp4", CacheControl: "no-cache", Metadata: map[string]string{"of": "x"}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.Copy(ctx, src, dst, media.CopyOptions{IfMatch: `"0123456789abcdef0123456789abcdef"`}); !errors.Is(err, media.ErrPreconditionFailed) {
			t.Fatalf("%d: stale IfMatch: %v", size, err)
		}
		obj, err := store.Copy(ctx, src, dst, media.CopyOptions{IfMatch: put.ETag})
		if err != nil || obj.Size != int64(size) {
			t.Fatalf("%d: copy %+v %v", size, obj, err)
		}
		rc, got, err := store.Get(ctx, dst, media.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(rc)
		rc.Close()
		if !bytes.Equal(b, body) || got.ContentType != "video/mp4" || got.CacheControl != "no-cache" || got.Metadata["of"] != "x" {
			t.Fatalf("%d: copied %d bytes, %+v", size, len(b), got)
		}
	}
}
