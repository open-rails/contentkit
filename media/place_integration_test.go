package media_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/internal/s3test"
	"github.com/open-rails/contentkit/media/layout"
)

// Nothing a client sends is written under a blob name (audit H1). The
// audit's PoC, a multipart upload declaring an existing blob's SHA-256 with
// other bytes, is staged, and placing it names it by its own bytes: the blob
// and the URLs serving it are unchanged, and a genuine upload over 64 MiB is
// placed whole. A single PUT whose bytes the store did not checksum is
// placed by its bytes too; manifest writes still require conditional PUT.
func TestPlaceNeverOverwritesABlob(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  func(*testing.T) *s3test.Env
	}{
		{"probed", func(t *testing.T) *s3test.Env { return s3test.Open(t) }},
		{"no-checksum", func(t *testing.T) *s3test.Env {
			e := s3test.Open(t)
			caps := media.Capabilities{ConditionalPut: true}
			c := *e
			c.Store, c.Config.Capabilities = e.WithCapabilities(t, caps), &caps
			return &c
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixtureOn(t, tc.env(t), nil)
			f.visible(1)
			ctx := context.Background()
			g := f.ref("gallery", 1)
			item, _ := f.reg.Item(g)
			orig := png(100)
			x := blobOf(orig)
			f.put(g, "originals/000.png", "image/png", orig)
			f.produce(g)
			key, _ := item.Blob(x)
			res, err := f.rd.Read(ctx, g, f.editor, media.ReadOptions{Prefix: "thumb/"})
			if err != nil || len(res.Files) == 0 || !strings.Contains(res.Files[0].URL, x) {
				t.Fatalf("read thumb: %+v %v", res, err)
			}
			thumb := res.Files[0].URL

			evil := bytes.Repeat([]byte{0xAB}, media.MaxSinglePut+1)
			copy(evil, "SECAUDIT-101-POISON")
			p, err := f.up.Presign(ctx, f.editor, media.PresignRequest{Ref: g, Path: "import/evil.zip", Type: "application/zip",
				Size: int64(len(evil)), SHA256: mustSum(x)})
			if err != nil || p.Exists || p.Multipart == nil || !layout.ValidStagedName(p.Blob) {
				t.Fatalf("presign over %s: %+v %v", x, p, err)
			}
			if done := putMultipart(t, f, p.Multipart.Ticket, evil); done.Blob != p.Blob || done.Size != int64(len(evil)) {
				t.Fatalf("complete %+v", done)
			}
			small := []byte("arbitrary bytes that do not hash to X")
			q, err := f.up.Presign(ctx, f.editor, media.PresignRequest{Ref: g, Path: "originals/evil.png", Type: "image/png",
				Size: int64(len(small)), SHA256: mustSum(x)})
			if err != nil || q.Put == nil || !strings.Contains(q.Put.URL, "/temp/"+q.Blob) {
				t.Fatalf("presign put over %s: %+v %v", x, q, err)
			}
			staged, _ := item.Staged(q.Blob)
			if _, err := f.env.Store.Put(ctx, staged, bytes.NewReader(small), int64(len(small)), media.PutOptions{ContentType: "image/png"}); err != nil {
				t.Fatal(err)
			}
			if obj, err := f.env.Store.Head(ctx, key); err != nil || obj.Size != int64(len(orig)) {
				t.Fatalf("blob %s before placing: size %d, %v", x, obj.Size, err)
			}

			m := f.commit(g, media.Op{Op: media.OpPut, Path: p.Path, Blob: p.Blob}, media.Op{Op: media.OpPut, Path: q.Path, Blob: q.Blob})
			if u, _ := m.Get(p.Path); u.Blob != blobOf(evil) || u.Staged != "" {
				t.Fatalf("multipart placed as %+v", u)
			}
			if u, _ := m.Get(q.Path); u.Blob != blobOf(small) {
				t.Fatalf("single PUT placed as %+v", u)
			}
			if obj, err := f.env.Store.Head(ctx, key); err != nil || obj.Size != int64(len(orig)) {
				t.Fatalf("blob %s overwritten: size %d, %v", x, obj.Size, err)
			}
			if st, body, _ := f.fetch(thumb); st != http.StatusOK || body != string(orig) {
				t.Fatalf("thumb serves %d %.20q", st, body)
			}
			big, _ := item.Blob(blobOf(evil))
			if obj, err := f.env.Store.Head(ctx, big); err != nil || obj.Size != int64(len(evil)) || obj.ContentType != "application/zip" {
				t.Fatalf("placed multipart %+v %v", obj, err)
			}
			rc, _, err := f.env.Store.Get(ctx, big, media.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			h := sha256.New()
			_, err = io.Copy(h, rc)
			rc.Close()
			if err != nil || layout.SHA256Name(h.Sum(nil)) != blobOf(evil) {
				t.Fatalf("placed bytes do not hash to their name: %v", err)
			}
			for o, err := range f.env.Store.List(ctx, item.TempPrefix()) {
				t.Fatalf("temp/ kept %s %v", o.Key, err)
			}
		})
	}
}

// A commit names a staged upload; until placed the file has no blob and the
// item is processing, and the worker is asked to place first. A staged
// upload gone before placing fails its file with not_uploaded.
func TestPlaceStagedUpload(t *testing.T) {
	f := newFixture(t)
	f.visible(1)
	ctx := context.Background()
	g := f.ref("gallery", 1)
	item, _ := f.reg.Item(g)
	k, _ := f.reg.Kind("gallery")
	p, staged := f.upload(g, "originals/1.png", "image/png", png(1))
	gone, lost := f.upload(g, "originals/2.png", "image/png", png(2))
	m, err := f.up.Commit(ctx, f.editor, g, []media.Op{{Op: media.OpPut, Path: p, Blob: staged}, {Op: media.OpPut, Path: gone, Blob: lost}})
	if err != nil {
		t.Fatal(err)
	}
	if u, _ := m.Get(p); u.Staged != staged || u.Blob != "" || u.Size != int64(len(png(1))) || k.Readiness(m).State != media.StateProcessing {
		t.Fatalf("staged %+v", u)
	}
	if jobs := f.q.take(); len(jobs) != 1 || !jobs[0].Place {
		t.Fatalf("enqueued %+v", jobs)
	}
	res, err := f.rd.Read(ctx, g, f.editor, media.ReadOptions{Editor: true, Prefix: "originals/"})
	if err != nil || len(res.Files) != 2 || !res.Files[0].Staged || res.Files[0].URL != "" {
		t.Fatalf("editor read of staged uploads %+v %v", res, err)
	}
	lostKey, _ := item.Staged(lost)
	if err := f.env.Store.Delete(ctx, lostKey); err != nil {
		t.Fatal(err)
	}
	if n, err := f.ms.Place(ctx, g); err != nil || n != 1 {
		t.Fatalf("placed %d: %v", n, err)
	}
	m, _, _ = f.ms.Get(ctx, g)
	if u, _ := m.Get(p); u.Blob != blobOf(png(1)) || u.Staged != "" {
		t.Fatalf("placed %+v", u)
	}
	if res, err = f.rd.Read(ctx, g, f.editor, media.ReadOptions{Editor: true, Prefix: p}); err != nil || len(res.Files) != 1 || res.Files[0].Staged {
		t.Fatalf("editor read of a placed upload %+v %v", res, err)
	}
	if u, _ := m.Get(gone); u.Fail() == nil || u.Fail().Code != media.CodeNotUploaded || u.Pending != nil {
		t.Fatalf("gone %+v", u)
	}
	if n, err := f.ms.Place(ctx, g); err != nil || n != 0 {
		t.Fatalf("placed again %d: %v", n, err)
	}
	// Uploaded again, it is placed.
	again, name := f.upload(g, "originals/2.png", "image/png", png(2))
	m = f.commit(g, media.Op{Op: media.OpPut, Path: again, Blob: name})
	if u, _ := m.Get(gone); u.Fail() != nil || u.Blob != blobOf(png(2)) {
		t.Fatalf("re-uploaded %+v", u)
	}
}

func partPlan(size int) [][2]int {
	var out [][2]int
	for off := 0; off < size; off += media.MaxPartSize {
		n := min(media.MaxPartSize, size-off)
		out = append(out, [2]int{off, n})
	}
	return out
}

func putMultipart(t *testing.T, f *fixture, ticket string, body []byte) media.UploadedBlob {
	t.Helper()
	ctx := context.Background()
	plan := partPlan(len(body))
	reqs := make([]media.PartRequest, len(plan))
	for i, pr := range plan {
		sum := sha256.Sum256(body[pr[0] : pr[0]+pr[1]])
		reqs[i] = media.PartRequest{Number: int32(i + 1), Size: int64(pr[1]), SHA256: sum[:]}
	}
	signed, err := f.up.PresignParts(ctx, f.editor, ticket, reqs)
	if err != nil {
		t.Fatalf("PresignParts: %v", err)
	}
	for i, sp := range signed {
		pr := plan[i]
		req, _ := http.NewRequest(sp.Method, sp.URL, bytes.NewReader(body[pr[0]:pr[0]+pr[1]]))
		req.Header = sp.Header.Clone()
		req.ContentLength = int64(pr[1])
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("PUT part %d: %v", sp.Number, err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("PUT part %d: %d %s", sp.Number, resp.StatusCode, b)
		}
	}
	done, err := f.up.Complete(ctx, f.editor, ticket)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	return done
}

func mustSum(blob string) []byte {
	sum, ok := layout.ParseSHA256Name(blob)
	if !ok {
		panic("bad blob name " + blob)
	}
	return sum
}

// deletingStore deletes a manifest right after a blob lands: the item is
// deleted while a place job runs.
type deletingStore struct {
	media.Store
	manifest string
}

func (s deletingStore) Put(ctx context.Context, key string, body io.Reader, size int64, o media.PutOptions) (media.Object, error) {
	obj, err := s.Store.Put(ctx, key, body, size, o)
	if err == nil && strings.Contains(key, "/private/") {
		err = s.Store.Delete(ctx, s.manifest)
	}
	return obj, err
}

// A place job that finds its item deleted removes the blob it just wrote
// and the staged upload, as the folder deletion before it could not.
func TestPlaceAfterDeletion(t *testing.T) {
	f := newFixture(t)
	f.visible(1)
	ctx := context.Background()
	g := f.ref("gallery", 1)
	item, _ := f.reg.Item(g)
	p, staged := f.upload(g, "originals/1.png", "image/png", png(1))
	if _, err := f.up.Commit(ctx, f.editor, g, []media.Op{{Op: media.OpPut, Path: p, Blob: staged}}); err != nil {
		t.Fatal(err)
	}
	ms := s3test.Manifests(t, deletingStore{Store: f.env.Store, manifest: item.ManifestKey()}, f.reg, media.ManifestOptions{Journal: f.env.Journal()})
	if n, err := ms.Place(ctx, g); err != nil || n != 0 {
		t.Fatalf("placed %d: %v", n, err)
	}
	for o, err := range f.env.Store.List(ctx, item.Prefix()) {
		t.Fatalf("left %s %v", o.Key, err)
	}
}

// afterBlob runs after once a blob lands in private/.
type afterBlob struct {
	media.Store
	after func()
}

func (s afterBlob) Put(ctx context.Context, key string, body io.Reader, size int64, o media.PutOptions) (media.Object, error) {
	obj, err := s.Store.Put(ctx, key, body, size, o)
	if err == nil && strings.Contains(key, "/private/") {
		s.after()
	}
	return obj, err
}

// A place job that finds its upload taken down removes the blob it just
// wrote: the upload's bytes do not come back under their name.
func TestPlaceAfterTakedown(t *testing.T) {
	f := newFixture(t)
	f.visible(1)
	ctx := context.Background()
	g := f.gallery(1, 1)
	item, _ := f.reg.Item(g)
	p, staged := f.upload(g, "originals/1.png", "image/png", png(1))
	if _, err := f.up.Commit(ctx, f.editor, g, []media.Op{{Op: media.OpPut, Path: p, Blob: staged}}); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	ms := s3test.Manifests(t, afterBlob{Store: f.env.Store, after: func() {
		once.Do(func() {
			if _, err := f.up.Commit(ctx, f.editor, g, []media.Op{{Op: media.OpRemove, Path: p, Takedown: true}}); err != nil {
				t.Error(err)
			}
		})
	}}, f.reg, media.ManifestOptions{Journal: f.env.Journal()})
	if n, err := ms.Place(ctx, g); err != nil || n != 0 {
		t.Fatalf("placed %d: %v", n, err)
	}
	blob, _ := item.Blob(blobOf(png(1)))
	if key, _ := item.Staged(staged); f.exists(blob) || f.exists(key) {
		t.Fatalf("a taken-down upload's blob kept %v, its staged object %v", f.exists(blob), f.exists(key))
	}
	m, _, _ := f.ms.Get(ctx, g)
	for _, b := range m.Blobs() {
		if key, _ := item.Blob(b); !f.exists(key) {
			t.Fatalf("took referenced %s", key)
		}
	}
}
