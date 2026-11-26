package s3_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"

	"github.com/open-rails/contentkit/media"
	medias3 "github.com/open-rails/contentkit/media/s3"
)

// bucket answers S3 requests for one object store, recording each.
type bucket struct {
	mu       sync.Mutex
	objects  map[string]bool
	requests []string
	down     bool
}

func (b *bucket) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.requests = append(b.requests, r.Method+" "+r.URL.Path)
	switch {
	case b.down:
		w.WriteHeader(http.StatusServiceUnavailable)
	case r.Method == http.MethodHead && b.objects[r.URL.Path]:
		w.Header().Set("ETag", `"d41d8cd98f00b204e9800998ecf8427e"`)
		w.Header().Set("Content-Length", "0")
	case r.Method == http.MethodHead:
		w.WriteHeader(http.StatusNotFound)
	case r.Method == http.MethodPut:
		b.objects[r.URL.Path] = true
		w.Header().Set("ETag", `"d41d8cd98f00b204e9800998ecf8427e"`)
	default:
		w.WriteHeader(http.StatusNotImplemented)
	}
}

func (b *bucket) seen() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := b.requests
	b.requests = nil
	return out
}

func TestCheckReadsOneObjectNeverAListing(t *testing.T) {
	b := &bucket{objects: map[string]bool{}}
	server := httptest.NewServer(b)
	defer server.Close()
	store, err := medias3.New(medias3.Config{Bucket: "media", Endpoint: server.URL, AccessKeyID: "key", SecretAccessKey: "secret",
		UsePathStyle: true, ProbePrefix: "_probe/app/", Capabilities: &media.Capabilities{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Check(t.Context()); err != nil {
		t.Fatalf("first check: %v", err)
	}
	if got, want := b.seen(), []string{"HEAD /media/_probe/app/_health", "PUT /media/_probe/app/_health"}; !slices.Equal(got, want) {
		t.Fatalf("first check sent %q, want %q: the marker is written once", got, want)
	}
	if err := store.Check(t.Context()); err != nil {
		t.Fatalf("second check: %v", err)
	}
	if got, want := b.seen(), []string{"HEAD /media/_probe/app/_health"}; !slices.Equal(got, want) {
		t.Fatalf("second check sent %q, want %q", got, want)
	}

	b.mu.Lock()
	b.down = true
	b.mu.Unlock()
	err = store.Check(t.Context())
	if !errors.Is(err, media.ErrUnavailable) {
		t.Fatalf("a bucket answering 503 is %v, want media.ErrUnavailable", err)
	}
	for _, r := range b.seen() {
		if r != "HEAD /media/_probe/app/_health" {
			t.Fatalf("an unavailable bucket was sent %q", r)
		}
	}
}
