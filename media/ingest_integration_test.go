package media_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/internal/s3test"
)

const ingestPart = 5 << 20

// faultStore fails PutPart for listed part numbers the given number of times
// and counts the parts actually sent, over the real bucket.
type faultStore struct {
	media.Store
	mu   sync.Mutex
	fail map[int32]int
	sent map[int32]int
}

func (s *faultStore) PutPart(ctx context.Context, key, id string, n int32, body io.Reader, size int64, sum []byte) (media.Part, error) {
	s.mu.Lock()
	if s.fail[n] > 0 {
		s.fail[n]--
		s.mu.Unlock()
		return media.Part{}, errors.New("injected part failure")
	}
	s.sent[n]++
	s.mu.Unlock()
	return s.Store.PutPart(ctx, key, id, n, body, size, sum)
}

type ingestEnv struct {
	*s3test.Env
	store     *faultStore
	uploads   *media.Uploads
	manifests *media.Manifests
	queue     *queue
	ref       contentref.ContentRef
}

var ingestAdmin = access.Actor{ID: "admin", Kind: "service"}

func newIngestEnv(t *testing.T) *ingestEnv {
	t.Helper()
	env := s3test.Open(t)
	if !env.Store.Capabilities().ConditionalPut {
		t.Skip("backend lacks conditional PUT")
	}
	store := &faultStore{Store: env.Store, fail: map[int32]int{}, sent: map[int32]int{}}
	kinds, err := media.NewRegistry(media.Kind{Name: "video", Versioned: true, Types: []string{"video/mp4"}, MaxBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	manifests := s3test.Manifests(t, store, kinds, media.ManifestOptions{})
	q := &queue{}
	u, err := media.NewUploads(media.UploadOptions{Store: store, Kinds: kinds, Manifests: manifests, Queue: q,
		Authorizer: grants{"admin": {Allowed: true, Exempt: true}}})
	if err != nil {
		t.Fatal(err)
	}
	return &ingestEnv{Env: env, store: store, uploads: u, manifests: manifests, queue: q,
		ref: contentref.NewVersion(env.Tenant, "video", cid(88), "v1")}
}

// stream hides any Seeker/ReaderAt: ingest reads a pipe-like body.
type stream struct{ r io.Reader }

func (s stream) Read(p []byte) (int, error) { return s.r.Read(p) }

// cut fails after n bytes, like a dropped SSH stream.
type cut struct {
	r io.Reader
	n int
}

func (c *cut) Read(p []byte) (int, error) {
	if c.n <= 0 {
		return 0, errors.New("connection reset")
	}
	p = p[:min(len(p), c.n)]
	k, err := c.r.Read(p)
	c.n -= k
	return k, err
}

func (e *ingestEnv) stored(t *testing.T, original string) []byte {
	t.Helper()
	item, _ := media.NewRegistry(media.Kind{Name: "video", Versioned: true})
	it, err := item.Item(e.ref)
	if err != nil {
		t.Fatal(err)
	}
	key, _ := it.Original(original)
	rc, _, err := e.Env.Store.Get(context.Background(), key, media.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestIngestMultipartUnknownSizeCommitsAndEnqueues(t *testing.T) {
	e := newIngestEnv(t)
	body := data(1, 2*ingestPart+ingestPart/2) // 3 parts, the last short
	e.store.fail[2] = 2                        // part 2 succeeds on its third attempt
	res, err := e.uploads.Ingest(context.Background(), ingestAdmin, media.IngestRequest{
		Ref: e.ref, Name: "source", Type: "video/mp4", Body: stream{bytes.NewReader(body)},
		Meta: map[string]any{"legacy_id": "123"}, PartSize: ingestPart, Concurrency: 2})
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	if !strings.HasPrefix(res.Original, "u-") || res.Size != int64(len(body)) || !bytes.Equal(res.SHA256, sum[:]) {
		t.Fatalf("result %s size %d: want a u- original of %d bytes with the body's SHA-256", res.Original, res.Size, len(body))
	}
	if e.store.sent[1] != 1 || e.store.sent[2] != 1 || e.store.sent[3] != 1 || len(e.store.sent) != 3 {
		t.Fatalf("parts sent %v: want 1-3 once each after retries", e.store.sent)
	}
	if got := e.stored(t, res.Original); !bytes.Equal(got, body) {
		t.Fatalf("stored %d bytes differ from the %d ingested", len(got), len(body))
	}
	m, _, err := e.manifests.Get(context.Background(), e.ref)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Files) != 1 || m.Files[0].Name != "source" || m.Files[0].Original != res.Original ||
		m.Files[0].Size != int64(len(body)) || m.Files[0].Type != "video/mp4" || m.Files[0].Meta["legacy_id"] != "123" {
		t.Fatalf("manifest %+v", m.Files)
	}
	if e.queue.count() != 1 || e.queue.jobs[0].Ref != e.ref {
		t.Fatalf("queued %+v: want one processing job for %v", e.queue.jobs, e.ref)
	}
}

func TestIngestSmallFileIsOneHashNamedPut(t *testing.T) {
	e := newIngestEnv(t)
	body := data(2, ingestPart-1)
	res, err := e.uploads.Ingest(context.Background(), ingestAdmin, media.IngestRequest{
		Ref: e.ref, Name: "source", Type: "video/mp4", Body: stream{bytes.NewReader(body)}, Size: int64(len(body)), PartSize: ingestPart})
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	if res.Original != media.SHA256Name(sum[:]) || len(e.store.sent) != 0 {
		t.Fatalf("original %s, parts %v: want the hash name and no multipart", res.Original, e.store.sent)
	}
	if got := e.stored(t, res.Original); !bytes.Equal(got, body) {
		t.Fatal("stored bytes differ")
	}
}

func TestIngestResumesAfterBrokenStream(t *testing.T) {
	e := newIngestEnv(t)
	body := data(3, 3*ingestPart+100)
	var saved media.IngestUpload
	req := media.IngestRequest{Ref: e.ref, Name: "source", Type: "video/mp4", Size: int64(len(body)), PartSize: ingestPart,
		OnUpload: func(u media.IngestUpload) error { saved = u; return nil }}

	req.Body = &cut{r: bytes.NewReader(body), n: 2*ingestPart + 10}
	if _, err := e.uploads.Ingest(context.Background(), ingestAdmin, req); err == nil || !strings.Contains(err.Error(), "connection reset") {
		t.Fatalf("broken stream: err %v", err)
	}
	if saved.UploadID == "" || e.queue.count() != 0 {
		t.Fatalf("after the break: upload %+v, queued %d; want a kept upload and nothing committed", saved, e.queue.count())
	}
	if e.store.sent[1] != 1 || e.store.sent[2] != 1 {
		t.Fatalf("parts sent before the break %v: want 1 and 2", e.store.sent)
	}

	req.Body = stream{bytes.NewReader(body)}
	req.Resume = &saved
	res, err := e.uploads.Ingest(context.Background(), ingestAdmin, req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Original != saved.Original || e.store.sent[1] != 1 || e.store.sent[2] != 1 || e.store.sent[3] != 1 || e.store.sent[4] != 1 {
		t.Fatalf("resume: original %s (saved %s), parts sent %v; want parts 1-2 reused, 3-4 sent once", res.Original, saved.Original, e.store.sent)
	}
	if got := e.stored(t, res.Original); !bytes.Equal(got, body) {
		t.Fatal("resumed object differs from the body")
	}
	if e.queue.count() != 1 {
		t.Fatalf("queued %d after resume", e.queue.count())
	}

	// A resume whose stored parts hold other bytes re-sends them.
	e2 := newIngestEnv(t)
	other := data(4, len(body))
	req2 := media.IngestRequest{Ref: e2.ref, Name: "source", Type: "video/mp4", PartSize: ingestPart,
		OnUpload: func(u media.IngestUpload) error { saved = u; return nil }, Body: &cut{r: bytes.NewReader(other), n: ingestPart + 1}}
	if _, err := e2.uploads.Ingest(context.Background(), ingestAdmin, req2); err == nil {
		t.Fatal("broken stream succeeded")
	}
	req2.Body, req2.Resume = stream{bytes.NewReader(body)}, &saved
	res, err = e2.uploads.Ingest(context.Background(), ingestAdmin, req2)
	if err != nil {
		t.Fatal(err)
	}
	if e2.store.sent[1] != 2 {
		t.Fatalf("part 1 sent %d times: a mismatching stored part must be replaced", e2.store.sent[1])
	}
	if got := e2.stored(t, res.Original); !bytes.Equal(got, body) {
		t.Fatal("object assembled from stale parts")
	}
}

func TestIngestRefusesBodiesOverTheirBounds(t *testing.T) {
	e := newIngestEnv(t)
	body := data(5, 2*ingestPart+1)
	var saved media.IngestUpload
	_, err := e.uploads.Ingest(context.Background(), ingestAdmin, media.IngestRequest{Ref: e.ref, Name: "source", Type: "video/mp4",
		Body: stream{bytes.NewReader(body)}, Size: int64(len(body) - 1), PartSize: ingestPart,
		OnUpload: func(u media.IngestUpload) error { saved = u; return nil }})
	if ue, ok := media.AsUploadError(err); !ok || ue.Code != media.CodeInvalid {
		t.Fatalf("over the declared size: err %v", err)
	}
	it, _ := media.NewRegistry(media.Kind{Name: "video", Versioned: true})
	item, _ := it.Item(e.ref)
	key, _ := item.Original(saved.Original)
	if _, err := e.Env.Store.ListParts(context.Background(), key, saved.UploadID); !errors.Is(err, media.ErrNotFound) {
		t.Fatalf("an invalid body must abort its upload even with OnUpload; ListParts err %v", err)
	}

	_, err = e.uploads.Ingest(context.Background(), ingestAdmin, media.IngestRequest{Ref: e.ref, Name: "source", Type: "image/png",
		Body: stream{bytes.NewReader(body)}, PartSize: ingestPart})
	if ue, ok := media.AsUploadError(err); !ok || ue.Code != media.CodeType {
		t.Fatalf("wrong type: err %v", err)
	}
	_, err = e.uploads.Ingest(context.Background(), access.Actor{ID: "reader"}, media.IngestRequest{Ref: e.ref, Name: "source",
		Type: "video/mp4", Body: stream{bytes.NewReader(body)}, PartSize: ingestPart})
	if ue, ok := media.AsUploadError(err); !ok || ue.Code != media.CodeForbidden {
		t.Fatalf("unauthorized actor: err %v", err)
	}
	if e.queue.count() != 0 {
		t.Fatalf("refused ingests queued %d jobs", e.queue.count())
	}
}
