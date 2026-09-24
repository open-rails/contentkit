package media_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/internal/s3test"
)

func blobName(s string) string {
	sum := sha256.Sum256([]byte(s))
	return media.SHA256Name(sum[:])
}

// concurrentInserts runs n editors, each appending its own file, and requires
// every insert to land exactly once.
func concurrentInserts(t *testing.T, ms []*media.Manifests, ref contentref.ContentRef, n int) {
	t.Helper()
	ctx := context.Background()
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			name := fmt.Sprintf("%03d.png", i)
			_, err := ms[i%len(ms)].Edit(ctx, ref, func(m *media.Manifest) error {
				m.Files = append(m.Files, media.File{Name: name, Original: blobName(name), Type: "image/png"})
				return nil
			})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	man, _, err := ms[0].Get(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	if len(man.Files) != n {
		t.Fatalf("%d of %d concurrent inserts landed", len(man.Files), n)
	}
	for i := range n {
		if man.File(fmt.Sprintf("%03d.png", i)) < 0 {
			t.Fatalf("insert %d lost", i)
		}
	}
}

func TestManifestEditCASUnderConcurrency(t *testing.T) {
	env := s3test.Open(t)
	if !env.Store.Capabilities().ConditionalPut {
		t.Skip("backend lacks conditional PUT; see the advisory-lock test")
	}
	r := registry(t)
	// Two processes, each with its own cache, editing the same manifest.
	a, err := media.NewManifests(env.Store, r, media.ManifestOptions{})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := media.NewManifests(env.Store, r, media.ManifestOptions{})
	ref := contentref.NewVersion(env.Tenant, "gallery", "1", "v1")
	concurrentInserts(t, []*media.Manifests{a, b}, ref, 12)
}

func TestManifestEditAdvisoryLockFallback(t *testing.T) {
	env := s3test.Open(t)
	pool := pgtest.Pool(t, nil)
	r := registry(t)
	store := env.WithCapabilities(t, media.Capabilities{})
	if _, err := media.NewManifests(store, r, media.ManifestOptions{}); err == nil {
		t.Fatal("no conditional PUT and no Locker must be refused")
	}
	var ms []*media.Manifests
	for range 2 {
		m, err := media.NewManifests(store, r, media.ManifestOptions{Locker: media.PGLocker(pool)})
		if err != nil {
			t.Fatal(err)
		}
		ms = append(ms, m)
	}
	concurrentInserts(t, ms, contentref.New(env.Tenant, "post", "7"), 12)
}

func TestManifestCacheRevalidatesAndEditsAreValidated(t *testing.T) {
	env := s3test.Open(t)
	if !env.Store.Capabilities().ConditionalPut {
		t.Skip("backend lacks conditional PUT")
	}
	ctx := context.Background()
	r := registry(t)
	reader, _ := media.NewManifests(env.Store, r, media.ManifestOptions{})
	writer, _ := media.NewManifests(env.Store, r, media.ManifestOptions{})
	ref := contentref.New(env.Tenant, "post", "501")

	if _, _, err := reader.Get(ctx, ref); !errors.Is(err, media.ErrNotFound) {
		t.Fatalf("missing manifest: %v", err)
	}
	video := media.NewUploadName()
	if _, err := writer.Edit(ctx, ref, func(m *media.Manifest) error {
		m.Files = []media.File{
			{Name: "teaser", Original: blobName("t"), Type: "image/jpeg", Meta: map[string]any{"teaser": true},
				Variants: map[string]media.Variant{"blurred": {Blob: blobName("tb"), Spec: "e2f0"}}},
			{Name: "clip.mp4", Original: video, HLS: &media.HLS{Source: video, Video: []media.Rendition{{Rung: 720, Width: 1280, Height: 720,
				Bandwidth: 1, Codecs: "avc1.64001f", Blob: blobName("r720"), Segments: []media.Segment{{0, 1843212, 4}, {1843212, 1790021, 3.5}}}}}},
		}
		m.Downloads = map[string]media.Download{"720p": {Blob: blobName("d720"), Type: "video/mp4"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	first, etag1, err := reader.Get(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Files[0].Teaser() || first.Files[1].HLS.Video[0].Segments[1] != (media.Segment{1843212, 1790021, 3.5}) {
		t.Fatalf("round trip %+v", first)
	}
	raw, _ := json.Marshal(first.Files[1].HLS.Video[0].Segments[0])
	if string(raw) != "[0,1843212,4]" {
		t.Fatalf("segment encoding %s", raw)
	}
	if got := first.Blobs(); len(got) != 3 {
		t.Fatalf("blobs %v", got)
	}

	// A cached copy is served only after revalidation: another writer's edit shows.
	if _, err := writer.Edit(ctx, ref, func(m *media.Manifest) error {
		m.Files[0], m.Files[1] = m.Files[1], m.Files[0]
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	second, etag2, err := reader.Get(ctx, ref)
	if err != nil || etag2 == etag1 || second.Files[0].Name != "clip.mp4" {
		t.Fatalf("stale read: %v %s %s", err, etag1, etag2)
	}
	// Mutating a returned manifest never leaks into the cache.
	second.Files = nil
	third, etag3, _ := reader.Get(ctx, ref)
	if etag3 != etag2 || len(third.Files) != 2 {
		t.Fatal("cache was mutated through a returned manifest")
	}

	// An unchanged edit writes nothing.
	if _, err := writer.Edit(ctx, ref, func(*media.Manifest) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, etag, _ := reader.Get(ctx, ref); etag != etag2 {
		t.Fatal("no-op edit rewrote the manifest")
	}

	for name, bad := range map[string]func(*media.Manifest) error{
		"duplicate name": func(m *media.Manifest) error { m.Files = append(m.Files, m.Files[0]); return nil },
		"foreign key":    func(m *media.Manifest) error { m.Files[0].Original = "d/post/9/originals/" + blobName("x"); return nil },
		"empty variant":  func(m *media.Manifest) error { m.Files[1].Variants["x"] = media.Variant{}; return nil },
		"fn error":       func(*media.Manifest) error { return errors.New("abort") },
	} {
		if _, err := writer.Edit(ctx, ref, bad); err == nil {
			t.Errorf("%s: edit accepted", name)
		}
	}
	if _, etag, _ := reader.Get(ctx, ref); etag != etag2 {
		t.Fatal("rejected edit was written")
	}

	if _, err := writer.Edit(ctx, contentref.New(env.Tenant, "gallery", "1"), func(*media.Manifest) error { return nil }); err == nil {
		t.Fatal("versioned kind edited without a version")
	}
}
