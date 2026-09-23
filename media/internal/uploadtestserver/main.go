// Command uploadtestserver serves media.UploadHandler over a fresh MinIO/RGW
// bucket for the browser SDK's integration tests (sdk/upload/test). It reads
// the CONTENTKIT_TEST_S3_* variables, presigns for -public (a proxy the test
// controls), prints "READY <url>" and runs until stdin closes.
//
//	POST /upload/...          the upload API; X-Test-Actor names the caller
//	GET  /object?kind&id&version&name   {"size","sha256"} of a stored original
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media"
	mediaS3 "github.com/open-rails/contentkit/media/s3"
	"github.com/open-rails/contentkit/media/token"
)

const tenant = "sdk"

type allow struct{}

func (allow) CanUpload(_ context.Context, a access.Actor, _ contentref.ContentRef) (media.UploadGrant, error) {
	return media.UploadGrant{Allowed: a.ID != "reader", Owner: "owner"}, nil
}

func main() {
	public := flag.String("public", "", "presign endpoint the client reaches (default: the S3 endpoint)")
	grace := flag.Duration("grace", 0, "sweep grace; short values let tests see originals go stale")
	flag.Parse()
	ctx := context.Background()

	suffix := make([]byte, 6)
	_, _ = rand.Read(suffix)
	cfg := mediaS3.Config{
		Bucket:          "ck-sdk-" + hex.EncodeToString(suffix),
		Region:          os.Getenv("CONTENTKIT_TEST_S3_REGION"),
		Endpoint:        os.Getenv("CONTENTKIT_TEST_S3_ENDPOINT"),
		PublicEndpoint:  *public,
		AccessKeyID:     os.Getenv("CONTENTKIT_TEST_S3_ACCESS_KEY"),
		SecretAccessKey: os.Getenv("CONTENTKIT_TEST_S3_SECRET_KEY"),
		UsePathStyle:    true,
	}
	store, err := mediaS3.New(cfg)
	must(err)
	_, err = store.Client().CreateBucket(ctx, &s3.CreateBucketInput{Bucket: &cfg.Bucket})
	must(err)
	defer drop(store)
	cfg.Capabilities, err = media.Probe(ctx, store, "probe/")
	must(err)
	store, err = mediaS3.New(cfg)
	must(err)

	kinds, err := media.NewRegistry(
		media.Kind{Name: "gallery", Versioned: true, Types: []string{"image/png", "image/jpeg"}, MaxBytes: 10 << 20,
			Slots: map[string]media.Slot{"cover": {Outputs: map[string]media.Spec{"cover": {Width: 460}}}}},
		media.Kind{Name: "video", Types: []string{"video/mp4"}, MaxBytes: 1 << 30},
	)
	must(err)
	manifests, err := media.NewManifests(store, kinds, media.ManifestOptions{})
	must(err)
	ring, err := token.NewRing(token.Key{ID: "k1", Secret: bytes.Repeat([]byte("s"), 32)}, nil)
	must(err)
	uploads, err := media.NewUploads(media.UploadOptions{Store: store, Kinds: kinds, Manifests: manifests, Tickets: &ring, Authorizer: allow{}, Grace: *grace})
	must(err)

	mux := http.NewServeMux()
	mux.Handle("/upload/", http.StripPrefix("/upload", media.UploadHandler(uploads, media.UploadHandlerOptions{
		Tenant: tenant,
		Actor: func(r *http.Request) (access.Actor, bool) {
			id := r.Header.Get("X-Test-Actor")
			return access.Actor{ID: id, Kind: "user"}, id != ""
		},
	})))
	mux.HandleFunc("GET /object", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		item, err := kinds.Item(contentref.NewVersion(tenant, q.Get("kind"), q.Get("id"), q.Get("version")))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		key, err := item.Original(q.Get("name"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		rc, _, err := store.Get(r.Context(), key, media.GetOptions{})
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		defer rc.Close()
		h := sha256.New()
		n, err := io.Copy(h, rc)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"size": n, "sha256": hex.EncodeToString(h.Sum(nil))})
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	must(err)
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	fmt.Printf("READY http://%s\n", ln.Addr())

	// Exit when the test runner closes stdin (or dies), or after an hour.
	done := make(chan struct{})
	go func() { _, _ = io.Copy(io.Discard, os.Stdin); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Hour):
	}
	_ = srv.Close()
}

// drop removes the bucket with its objects and unfinished uploads.
func drop(store *mediaS3.Store) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	c, bucket := store.Client(), store.Bucket()
	if ups, err := c.ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{Bucket: &bucket}); err == nil {
		for _, u := range ups.Uploads {
			_, _ = c.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{Bucket: &bucket, Key: u.Key, UploadId: u.UploadId})
		}
	}
	p := s3.NewListObjectsV2Paginator(c, &s3.ListObjectsV2Input{Bucket: &bucket})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			break
		}
		for _, o := range page.Contents {
			_, _ = c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &bucket, Key: o.Key})
		}
	}
	if _, err := c.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: &bucket}); err != nil {
		log.Printf("drop bucket %s: %v", bucket, err)
	}
}

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
