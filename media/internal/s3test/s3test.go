// Package s3test opens the test bucket from the environment:
//
//	CONTENTKIT_TEST_S3_ENDPOINT   http://localhost:9000 (MinIO) or an RGW endpoint; unset skips
//	CONTENTKIT_TEST_S3_ACCESS_KEY, CONTENTKIT_TEST_S3_SECRET_KEY
//	CONTENTKIT_TEST_S3_REGION     default us-east-1
//	CONTENTKIT_TEST_S3_BUCKET     existing bucket; unset creates (and removes) one per test
//	CONTENTKIT_TEST_S3_REQUIRE    comma list that must hold: conditional,checksum,abort-lifecycle
//
// A backend without conditional PUT (Ceph RGW) needs a Postgres PGLocker for
// manifest edits, taken from CONTENTKIT_TEST_URL (see Manifests).
//
// Every test writes under its own tenant prefix, removed with all versions on cleanup.
package s3test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/google/uuid"

	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/media"
	mediaS3 "github.com/open-rails/contentkit/media/s3"
)

// Env is an opened test bucket.
type Env struct {
	Store   *mediaS3.Store
	Config  mediaS3.Config
	Tenant  string // unique per test; all keys live under "{Tenant}/"
	Created bool   // the bucket was created for this test
}

// Require reports whether CONTENTKIT_TEST_S3_REQUIRE lists capability.
func Require(capability string) bool {
	for _, c := range strings.Split(os.Getenv("CONTENTKIT_TEST_S3_REQUIRE"), ",") {
		if strings.TrimSpace(c) == capability {
			return true
		}
	}
	return false
}

// Open probes the backend and returns a store configured with its capabilities.
func Open(t testing.TB) *Env {
	t.Helper()
	endpoint := os.Getenv("CONTENTKIT_TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("CONTENTKIT_TEST_S3_ENDPOINT not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cfg := mediaS3.Config{
		Bucket:          os.Getenv("CONTENTKIT_TEST_S3_BUCKET"),
		Region:          os.Getenv("CONTENTKIT_TEST_S3_REGION"),
		Endpoint:        endpoint,
		AccessKeyID:     os.Getenv("CONTENTKIT_TEST_S3_ACCESS_KEY"),
		SecretAccessKey: os.Getenv("CONTENTKIT_TEST_S3_SECRET_KEY"),
		UsePathStyle:    true,
	}
	env := &Env{Tenant: "t" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]}
	if cfg.Bucket == "" {
		cfg.Bucket = "ck-media-" + env.Tenant[1:]
		env.Created = true
	}
	store, err := mediaS3.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if env.Created {
		if _, err := store.Client().CreateBucket(ctx, &s3.CreateBucketInput{Bucket: &cfg.Bucket}); err != nil {
			t.Fatalf("create bucket: %v", err)
		}
	}
	t.Cleanup(func() { env.cleanup(t) })
	caps, err := media.Probe(ctx, store, env.Tenant+"/")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("backend %s capabilities: %+v", endpoint, caps)
	if Require("conditional") && !caps.ConditionalPut {
		t.Fatal("backend lacks required conditional PUT")
	}
	if Require("checksum") && !caps.ChecksumSHA256 {
		t.Fatal("backend lacks required x-amz-checksum-sha256 enforcement")
	}
	cfg.Capabilities = &caps
	if env.Store, err = mediaS3.New(cfg); err != nil {
		t.Fatal(err)
	}
	env.Config = cfg
	return env
}

// WithCapabilities returns a store over the same bucket claiming caps.
func (e *Env) WithCapabilities(t testing.TB, caps media.Capabilities) *mediaS3.Store {
	cfg := e.Config
	cfg.Capabilities = &caps
	s, err := mediaS3.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// WithoutConditionalPut returns the env over a store claiming the probed
// capabilities minus conditional PUT, as on Ceph RGW.
func (e *Env) WithoutConditionalPut(t testing.TB) *Env {
	caps := e.Store.Capabilities()
	caps.ConditionalPut = false
	c := *e
	c.Store = e.WithCapabilities(t, caps)
	c.Config.Capabilities = &caps
	return &c
}

// Locker is what a host wires: a PGLocker on CONTENTKIT_TEST_URL (skipping
// the test when it is unset).
func Locker(t testing.TB, store media.Store) media.Locker {
	t.Helper()
	if os.Getenv("CONTENTKIT_TEST_URL") == "" {
		t.Skip("manifest edits need a Postgres PGLocker: set CONTENTKIT_TEST_URL")
	}
	return media.PGLocker(pgtest.Pool(t, nil))
}

// Manifests opens Manifests over store with opts and the Locker it needs.
func Manifests(t testing.TB, store media.Store, kinds *media.Registry, opts media.ManifestOptions) *media.Manifests {
	t.Helper()
	opts.Locker = Locker(t, store)
	ms, err := media.NewManifests(store, kinds, opts)
	if err != nil {
		t.Fatal(err)
	}
	return ms
}

func (e *Env) cleanup(t testing.TB) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	c, bucket := e.Store.Client(), e.Store.Bucket()
	prefix := e.Tenant + "/"
	if e.Created {
		prefix = ""
	}
	uploads, err := c.ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{Bucket: &bucket, Prefix: &prefix})
	if err == nil {
		for _, u := range uploads.Uploads {
			_, _ = c.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{Bucket: &bucket, Key: u.Key, UploadId: u.UploadId})
		}
	}
	p := s3.NewListObjectVersionsPaginator(c, &s3.ListObjectVersionsInput{Bucket: &bucket, Prefix: &prefix})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			t.Logf("cleanup list versions: %v", err)
			break
		}
		del := func(key, version *string) {
			if _, err := c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &bucket, Key: key, VersionId: version}); err != nil {
				t.Logf("cleanup delete %s: %v", aws.ToString(key), err)
			}
		}
		for _, v := range page.Versions {
			del(v.Key, v.VersionId)
		}
		for _, m := range page.DeleteMarkers {
			del(m.Key, m.VersionId)
		}
	}
	if e.Created {
		if _, err := c.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: &bucket}); err != nil {
			t.Logf("cleanup delete bucket: %v", err)
		}
	}
}

// VersioningStatus reports the bucket's versioning status.
func (e *Env) VersioningStatus(ctx context.Context) (types.BucketVersioningStatus, error) {
	out, err := e.Store.Client().GetBucketVersioning(ctx, &s3.GetBucketVersioningInput{Bucket: aws.String(e.Store.Bucket())})
	if err != nil {
		return "", fmt.Errorf("get versioning: %w", err)
	}
	return out.Status, nil
}
