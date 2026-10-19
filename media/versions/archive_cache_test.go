package versions

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/open-rails/contentkit/media"
)

type archiveStore struct {
	media.Store
	mu     sync.Mutex
	data   map[string][]byte
	reads  map[string]int
	putErr error
}

func (s *archiveStore) Get(ctx context.Context, key string, _ media.GetOptions) (io.ReadCloser, media.Object, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, media.Object{}, err
	}
	s.reads[key]++
	body, ok := s.data[key]
	if !ok {
		return nil, media.Object{}, media.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(body)), media.Object{Size: int64(len(body))}, nil
}
func (s *archiveStore) Put(ctx context.Context, key string, r io.Reader, size int64, o media.PutOptions) (media.Object, error) {
	body, err := io.ReadAll(r)
	if err != nil {
		return media.Object{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return media.Object{}, err
	}
	if s.putErr != nil {
		return media.Object{}, s.putErr
	}
	if int64(len(body)) != size || o.IfNoneMatch != "*" || o.CacheControl != "private, no-store" {
		panic("unsafe cache publication")
	}
	if _, ok := s.data[key]; ok {
		return media.Object{}, media.ErrPreconditionFailed
	}
	s.data[key] = body
	return media.Object{Size: size}, nil
}
func newArchiveStore() *archiveStore {
	return &archiveStore{data: map[string][]byte{"png": bytes.Repeat([]byte("png"), 3000), "jpg": []byte("jpeg-original")}, reads: map[string]int{}}
}
func streamTestArchive(ctx context.Context, s *archiveStore, w io.Writer) (int64, error, error) {
	return streamCachedArchive(ctx, s, "cache", []string{"png", "jpg"}, []string{"01.png", "02.jpg"}, w)
}
func TestArchiveStreamsAndReusesOnlyCompleteCache(t *testing.T) {
	s := newArchiveStore()
	var first, second bytes.Buffer
	n, cacheErr, err := streamTestArchive(context.Background(), s, &first)
	if err != nil || cacheErr != nil || n != int64(first.Len()) {
		t.Fatalf("first %d %v %v", n, cacheErr, err)
	}
	zr, err := zip.NewReader(bytes.NewReader(first.Bytes()), int64(first.Len()))
	if err != nil || len(zr.File) != 2 {
		t.Fatalf("complete ZIP: %v", err)
	}
	for i, key := range []string{"png", "jpg"} {
		r, _ := zr.File[i].Open()
		body, err := io.ReadAll(r)
		r.Close()
		if err != nil || !bytes.Equal(body, s.data[key]) {
			t.Fatal("original format bytes changed")
		}
	}
	_, cacheErr, err = streamTestArchive(context.Background(), s, &second)
	if err != nil || cacheErr != nil || !bytes.Equal(first.Bytes(), second.Bytes()) {
		t.Fatalf("reuse: %v %v", cacheErr, err)
	}
	if s.reads["png"] != 1 || s.reads["jpg"] != 1 {
		t.Fatalf("cache hit opened sources: %v", s.reads)
	}
}

type failArchiveWriter struct{ cancel context.CancelFunc }

func (w failArchiveWriter) Write([]byte) (int, error) {
	if w.cancel != nil {
		w.cancel()
	}
	return 0, io.ErrClosedPipe
}
func TestArchiveFailuresNeverPublishCache(t *testing.T) {
	for _, mode := range []string{"missing-source", "broken-client", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			s := newArchiveStore()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var dst io.Writer = io.Discard
			switch mode {
			case "missing-source":
				delete(s.data, "jpg")
			case "broken-client":
				dst = failArchiveWriter{}
			case "cancelled":
				dst = failArchiveWriter{cancel: cancel}
			}
			_, _, err := streamTestArchive(ctx, s, dst)
			if err == nil {
				t.Fatal("failed transfer reported success")
			}
			if _, ok := s.data["cache"]; ok {
				t.Fatal("partial ZIP cached")
			}
		})
	}
}
func TestArchiveCacheWriteFailurePreservesCompleteDownload(t *testing.T) {
	s := newArchiveStore()
	s.putErr = errors.New("storage unavailable")
	var out bytes.Buffer
	_, cacheErr, err := streamTestArchive(context.Background(), s, &out)
	if err != nil || !errors.Is(cacheErr, s.putErr) {
		t.Fatalf("cache optional: %v %v", cacheErr, err)
	}
	if _, err := zip.NewReader(bytes.NewReader(out.Bytes()), int64(out.Len())); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.data["cache"]; ok {
		t.Fatal("failed storage published cache")
	}
}
func TestConcurrentArchiveMissesPublishIdenticalCompleteOutput(t *testing.T) {
	s := newArchiveStore()
	var wg sync.WaitGroup
	bodies := make(chan []byte, 8)
	for range 8 {
		wg.Go(func() {
			var out bytes.Buffer
			_, cacheErr, err := streamTestArchive(context.Background(), s, &out)
			if err != nil || cacheErr != nil {
				t.Errorf("concurrent miss: %v %v", cacheErr, err)
			}
			bodies <- out.Bytes()
		})
	}
	wg.Wait()
	close(bodies)
	for body := range bodies {
		if !bytes.Equal(body, s.data["cache"]) {
			t.Fatal("concurrent cache differs")
		}
	}
}
func TestArchiveRevisionTracksOrderExclusionsFormatsAndNames(t *testing.T) {
	original := []media.File{{Path: "originals/a.png", Blob: "hash-a", Type: "image/png"}, {Path: "originals/b.jpg", Blob: "hash-b", Type: "image/jpeg"}}
	key := archiveRevision(original)
	for _, files := range [][]media.File{
		{original[1], original[0]}, original[:1],
		{{Path: "originals/a.jpg", Blob: "hash-a", Type: "image/jpeg"}, original[1]},
		{{Path: "originals/a.png", Blob: "bounded-hash", Type: "image/png"}, original[1]},
	} {
		if key == archiveRevision(files) {
			t.Fatal("changed archive reused a key")
		}
	}
	renamed := append([]media.File(nil), original...)
	renamed[0].Path = "originals/renamed.png"
	if !reflect.DeepEqual(archiveRevision(renamed), key) {
		t.Fatal("irrelevant source name invalidates positional ZIP")
	}
}

func TestArchiveCacheBusyAndOversizedDownloadsStreamWithoutCaching(t *testing.T) {
	for _, mode := range []string{"busy", "oversized"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("TMPDIR", t.TempDir())
			budget := make(chan struct{}, 1)
			limit := int64(1 << 20)
			if mode == "busy" {
				budget <- struct{}{}
			} else {
				limit = 64
			}
			s := newArchiveStore()
			var out bytes.Buffer
			_, cacheErr, err := streamArchive(context.Background(), s, "cache", []string{"png", "jpg"}, []string{"01.png", "02.jpg"}, &out, budget, limit)
			if err != nil || cacheErr != nil {
				t.Fatalf("uncached transfer: %v %v", cacheErr, err)
			}
			if _, err := zip.NewReader(bytes.NewReader(out.Bytes()), int64(out.Len())); err != nil {
				t.Fatal(err)
			}
			if _, ok := s.data["cache"]; ok {
				t.Fatal("cache exceeded its admission/size budget")
			}
			if names, _ := filepath.Glob(filepath.Join(os.TempDir(), "contentkit-download-*")); len(names) != 0 {
				t.Fatalf("discarded spool remains: %v", names)
			}
			if mode == "oversized" && len(budget) != 0 {
				t.Fatal("size limit leaked spool admission")
			}
		})
	}
}

func TestArchiveCancelledTransferReleasesSpoolAdmission(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	budget := make(chan struct{}, 1)
	s := newArchiveStore()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, _, err := streamArchive(ctx, s, "cache", []string{"png"}, []string{"01.png"}, failArchiveWriter{cancel: cancel}, budget, 1<<20)
	if err == nil || len(budget) != 0 {
		t.Fatalf("cancelled transfer retained admission: %v %d", err, len(budget))
	}
	if names, _ := filepath.Glob(filepath.Join(os.TempDir(), "contentkit-download-*")); len(names) != 0 {
		t.Fatalf("cancelled spool remains: %v", names)
	}
	var retry bytes.Buffer
	if _, cacheErr, err := streamArchive(context.Background(), s, "cache", []string{"png"}, []string{"01.png"}, &retry, budget, 1<<20); err != nil || cacheErr != nil {
		t.Fatalf("next request blocked: %v %v", cacheErr, err)
	}
	if _, ok := s.data["cache"]; !ok {
		t.Fatal("next request could not cache")
	}
}
