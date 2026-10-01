package media_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/internal/s3test"
)

// A manifest never grows past what readers load (audit H2). The audit's PoC,
// a 10 MiB meta value, is refused where ops set meta; puts with meta at its
// bound grow the manifest until a commit would pass the bound once
// processed, which is refused with the manifest unchanged. The item then still reads, hides and
// deletes from a fresh process, and deletion releases every upload's charge
// (at least 64 KiB each).
func TestManifestCaps(t *testing.T) {
	f := newFixture(t)
	f.visible(1)
	ctx := context.Background()
	g := f.ref("gallery", 1)
	item, _ := f.reg.Item(g)
	f.put(g, "cover.png", "image/png", png(2))
	f.produce(g)
	cover, _ := item.Public("cover-460.webp")
	if !f.exists(cover) {
		t.Fatal("no public cover")
	}
	seed := blobOf(png(2))
	for _, op := range []media.Op{
		{Op: media.OpPut, Path: "originals/x.png", Blob: seed, Meta: map[string]any{"x": strings.Repeat("A", 10<<20)}},
		{Op: media.OpMeta, Meta: map[string]any{"title": strings.Repeat("A", media.MaxItemMetaBytes)}},
	} {
		if _, err := f.up.Commit(ctx, f.editor, g, []media.Op{op}); code(err) != media.CodeTooLarge {
			t.Fatalf("%s with oversized meta: %v", op.Op, err)
		}
	}

	meta := map[string]any{"x": strings.Repeat("A", media.MaxMetaBytes-16)}
	var last *media.Manifest
	for batch := 0; ; batch++ {
		if batch == 8 {
			t.Fatal("commits never reached the cap")
		}
		ops := make([]media.Op, 1000)
		for i := range ops {
			ops[i] = media.Op{Op: media.OpPut, Path: fmt.Sprintf("originals/%d-%04d.png", batch, i), Blob: seed, Meta: meta}
		}
		m, err := f.up.Commit(ctx, f.editor, g, ops)
		if err != nil {
			if code(err) != media.CodeTooLarge || last == nil {
				t.Fatalf("batch %d: %v", batch, err)
			}
			break
		}
		last = m
	}
	fresh := s3test.Manifests(t, f.env.Store, f.reg, media.ManifestOptions{})
	m, _, err := fresh.Get(ctx, g)
	if err != nil || len(m.Files) != len(last.Files) {
		t.Fatalf("fresh read after the refused commit: %v", err)
	}

	pool := pgtest.Pool(t, nil)
	limiter, err := media.NewPGLimiter(pool, pgtest.Schema(t, ctx, pool), media.PGLimits{})
	if err != nil {
		t.Fatal(err)
	}
	charged := m.UploadBytes()
	if charged != int64(len(m.Files))*(64<<10) { // tiny uploads, each at the floor
		t.Fatalf("charged %d for %d uploads", charged, len(m.Files))
	}
	if err := limiter.Settle(ctx, media.Settlement{Tenant: f.ns, Owner: "owner", Delta: charged + 100}); err != nil {
		t.Fatal(err)
	}
	jobs, err := media.NewJobs(media.JobsConfig{Store: f.env.Store, Registry: f.reg, Locker: s3test.Locker(t, f.env.Store), Limiter: limiter})
	if err != nil {
		t.Fatal(err)
	}
	f.res.set(cid(1), access.Resolution{})
	if err := jobs.Expose(ctx, g); err != nil || f.exists(cover) {
		t.Fatalf("hide: %v, cover kept %v", err, f.exists(cover))
	}
	if err := jobs.Purge(ctx, media.Deletion{Ref: g, Owner: "owner", OperationID: "h2"}); err != nil {
		t.Fatal(err)
	}
	if used, _, err := limiter.Usage(ctx, f.ns, "owner"); err != nil || used != 100 {
		t.Fatalf("usage after deletion %d %v", used, err)
	}
}

// A manifest over MaxManifestBytes (written before the bound) does not load,
// yet the item still hides and deletes: hiding needs no manifest and the
// quota release falls back to nothing.
func TestOversizedManifest(t *testing.T) {
	f := newFixture(t)
	f.visible(1)
	ctx := context.Background()
	g := f.ref("gallery", 1)
	item, _ := f.reg.Item(g)
	f.put(g, "cover.png", "image/png", png(2))
	f.produce(g)
	cover, _ := item.Public("cover-460.webp")
	body := oversizedManifest(t)
	if _, err := f.env.Store.Put(ctx, item.ManifestKey(), bytes.NewReader(body), int64(len(body)), media.PutOptions{ContentType: "application/gzip"}); err != nil {
		t.Fatal(err)
	}
	fresh := s3test.Manifests(t, f.env.Store, f.reg, media.ManifestOptions{})
	if _, _, err := fresh.Get(ctx, g); !errors.Is(err, media.ErrManifestUnreadable) {
		t.Fatalf("oversized read: %v", err)
	}
	pool := pgtest.Pool(t, nil)
	limiter, err := media.NewPGLimiter(pool, pgtest.Schema(t, ctx, pool), media.PGLimits{})
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := media.NewJobs(media.JobsConfig{Store: f.env.Store, Registry: f.reg, Locker: s3test.Locker(t, f.env.Store), Limiter: limiter})
	if err != nil {
		t.Fatal(err)
	}
	f.res.set(cid(1), access.Resolution{})
	if err := jobs.Expose(ctx, g); err != nil || f.exists(cover) {
		t.Fatalf("hide: %v, cover kept %v", err, f.exists(cover))
	}
	if err := jobs.Purge(ctx, media.Deletion{Ref: g, Owner: "owner", OperationID: "big"}); err != nil {
		t.Fatal(err)
	}
	for o, err := range f.env.Store.List(ctx, item.Prefix()) {
		t.Fatalf("kept %s %v", o.Key, err)
	}
}

// A manifest of exactly MaxManifestBytes of JSON loads; one byte more does not.
func TestManifestBound(t *testing.T) {
	for _, extra := range []int{0, 1} {
		head, tail := `{"v":2,"meta":{"x":"`, `"},"files":[]}`
		pad := media.MaxManifestBytes - len(head) - len(tail) + extra
		var b bytes.Buffer
		zw := gzip.NewWriter(&b)
		zw.Write([]byte(head))
		zw.Write(bytes.Repeat([]byte("A"), pad))
		zw.Write([]byte(tail))
		zw.Close()
		_, err := media.DecodeManifest(b.Bytes())
		if extra == 0 && err != nil || extra == 1 && !errors.Is(err, media.ErrManifestUnreadable) {
			t.Fatalf("%d bytes over: %v", extra, err)
		}
	}
}

// oversizedManifest is a gzip manifest of more than MaxManifestBytes of JSON.
func oversizedManifest(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	zw.Write([]byte(`{"v":2,"meta":{"x":"`))
	zw.Write(bytes.Repeat([]byte("A"), media.MaxManifestBytes))
	zw.Write([]byte(`"},"files":[]}`))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// editLimit is where commits and the workers' records stop: 4 KiB short of
// MaxManifestBytes, so a hide always fits.
const editLimit = media.MaxManifestBytes - 4<<10

// padded is meta padding m (as fn receives it) to a manifest of size bytes of
// JSON, for a kind whose meta fills no download names.
func padded(t *testing.T, m *media.Manifest, size int) map[string]any {
	t.Helper()
	c := *m
	c.Meta = map[string]any{"pad": ""}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(&c); err != nil {
		t.Fatal(err)
	}
	return map[string]any{"pad": strings.Repeat("A", size-b.Len())}
}

// The largest manifest an edit may write stays in the cache, the host's
// (Jobs.Manifests) and a fresh one alike: reads never decode it again.
func TestLargeManifestIsCached(t *testing.T) {
	f := newFixture(t)
	f.visible(1)
	ctx := context.Background()
	post := f.ref("post", 1)
	f.put(post, "inline/a.png", "image/png", png(1))
	if _, err := f.ms.EditExisting(ctx, post, func(m *media.Manifest) error {
		m.Meta = padded(t, m, editLimit)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	fresh := s3test.Manifests(t, f.env.Store, f.reg, media.ManifestOptions{})
	for _, ms := range []*media.Manifests{f.ms, fresh} {
		a, _, err := ms.Get(ctx, post)
		if err != nil {
			t.Fatal(err)
		}
		if b, _, _ := ms.Get(ctx, post); a != b {
			t.Fatal("an admissible manifest was not cached")
		}
	}
}

// A commit is refused only when it grows the item past the bound once
// processed: an item whose outputs would not fit refuses a new page, yet
// removing one or shortening its meta still lands (no freeze).
func TestFullItemStillShrinks(t *testing.T) {
	f := newFixture(t)
	f.visible(1)
	ctx := context.Background()
	g := f.ref("gallery", 1)
	f.put(g, "originals/seed.png", "image/png", png(1))
	seed := blobOf(png(1))
	meta := map[string]any{"x": strings.Repeat("A", media.MaxMetaBytes-16)}
	if _, err := f.ms.EditExisting(ctx, g, func(m *media.Manifest) error {
		for i := range 1800 {
			m.Files = append(m.Files, media.File{Path: fmt.Sprintf("originals/p%04d.png", i), Blob: seed, Type: "image/png",
				Size: int64(len(png(1))), Meta: meta, Pending: []string{"thumb", "high"}})
		}
		m.Meta = map[string]any{"title": strings.Repeat("T", 100)}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.up.Commit(ctx, f.editor, g, []media.Op{{Op: media.OpPut, Path: "originals/new.png", Blob: seed}}); code(err) != media.CodeTooLarge {
		t.Fatalf("a page whose outputs would not fit: %v", err)
	}
	f.commit(g, media.Op{Op: media.OpRemove, Path: "originals/p0000.png"})
	f.commit(g, media.Op{Op: media.OpMeta, Meta: map[string]any{"title": "short"}})
}

// The commit counts the outputs its uploads will get, with their escaped
// names: pages whose processed entries would pass the bound are refused
// while the manifest is still small. Names are written unescaped.
func TestCommitProjectsOutputs(t *testing.T) {
	f := newFixture(t)
	f.visible(1)
	ctx := context.Background()
	g := f.ref("gallery", 1)
	item, _ := f.reg.Item(g)
	f.put(g, "originals/seed.png", "image/png", png(1))
	seed := blobOf(png(1))
	name := strings.Repeat("<", 190)
	var last *media.Manifest
	for batch := 0; ; batch++ {
		if batch == 10 {
			t.Fatal("commits never reached the bound")
		}
		ops := make([]media.Op, 1000)
		for i := range ops {
			ops[i] = media.Op{Op: media.OpPut, Path: fmt.Sprintf("originals/%s%d-%04d", name, batch, i), Blob: seed}
		}
		m, err := f.up.Commit(ctx, f.editor, g, ops)
		if err != nil {
			if code(err) != media.CodeTooLarge || last == nil {
				t.Fatalf("batch %d: %v", batch, err)
			}
			break
		}
		last = m
	}
	var size bytes.Buffer
	enc := json.NewEncoder(&size)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(last); err != nil || size.Len() > media.MaxManifestBytes/2 {
		t.Fatalf("refused only at %d bytes: outputs were not projected (%v)", size.Len(), err)
	}
	rc, _, err := f.env.Store.Get(ctx, item.ManifestKey(), media.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	zr, err := gzip.NewReader(rc)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(zr)
	if !bytes.Contains(raw, []byte(name)) || bytes.Contains(raw, []byte(`\u003c`)) {
		t.Fatal("names are written escaped")
	}
}

// A hide always fits: an item at the edit limit hides; a stored manifest that
// decodes but leaves no room for the flag is refused and the error returned
// (the job retries), never taken for hidden.
func TestHideFits(t *testing.T) {
	f := newFixture(t)
	f.visible(1)
	f.visible(2)
	ctx := context.Background()
	full := f.ref("post", 1)
	f.put(full, "inline/a.png", "image/png", png(1))
	if _, err := f.ms.EditExisting(ctx, full, func(m *media.Manifest) error {
		m.Meta = padded(t, m, editLimit)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	f.res.set(cid(1), access.Resolution{})
	if err := f.jobs.Expose(ctx, full); err != nil {
		t.Fatal(err)
	}
	if m, _, _ := f.ms.Get(ctx, full); !m.Hidden {
		t.Fatal("an item at the edit limit did not hide")
	}

	tight := f.ref("post", 2)
	f.put(tight, "inline/b.png", "image/png", png(2))
	f.produce(tight) // nothing pending: the hide only adds its flag
	m, _, _ := f.ms.Get(ctx, tight)
	c := *m
	c.Meta = padded(t, m, media.MaxManifestBytes-5)
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	enc := json.NewEncoder(zw)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(&c); err != nil {
		t.Fatal(err)
	}
	zw.Close()
	item, _ := f.reg.Item(tight)
	if _, err := f.env.Store.Put(ctx, item.ManifestKey(), bytes.NewReader(b.Bytes()), int64(b.Len()), media.PutOptions{ContentType: "application/gzip"}); err != nil {
		t.Fatal(err)
	}
	f.res.set(cid(2), access.Resolution{})
	if err := f.jobs.Expose(ctx, tight); !errors.Is(err, media.ErrManifestTooLarge) {
		t.Fatalf("a hide that does not fit: %v", err)
	}
	if m, _, err := f.ms.Get(ctx, tight); err != nil || m.Hidden {
		t.Fatalf("hidden %v %v", m != nil && m.Hidden, err)
	}
}

// A Full item asks the worker for nothing: commits that do not shrink it
// (a regenerate, a meta edit as long) enqueue no work and leave it Full; one
// that shrinks it clears Full and processing resumes. Readiness and editor
// reads say so.
func TestFullStopsUntilShrunk(t *testing.T) {
	f := newFixture(t)
	f.visible(1)
	ctx := context.Background()
	g := f.gallery(1, 2)
	f.commit(g, media.Op{Op: media.OpMeta, Meta: map[string]any{"title": "abc"}})
	f.put(g, "originals/002.png", "image/png", png(102))
	if err := f.ms.SetFull(ctx, g); err != nil {
		t.Fatal(err)
	}
	f.q.take()
	k, _ := f.reg.Kind("gallery")
	m := f.commit(g, media.Op{Op: media.OpRegenerate}, media.Op{Op: media.OpMeta, Meta: map[string]any{"title": "xyz"}})
	if jobs := f.q.take(); !m.Full || len(jobs) != 0 || k.Readiness(m).State != media.StateFull {
		t.Fatalf("full %v, enqueued %+v, state %s", m.Full, jobs, k.Readiness(m).State)
	}
	res, err := f.rd.Read(ctx, g, f.editor, media.ReadOptions{Editor: true})
	if err != nil || !res.Full || res.State != media.StateFull {
		t.Fatalf("editor read %+v %v", res, err)
	}
	f.q.take() // the read asks for its editor views
	m = f.commit(g, media.Op{Op: media.OpRemove, Path: "originals/000.png"})
	if jobs := f.q.take(); m.Full || len(jobs) != 1 {
		t.Fatalf("a shrinking commit: full %v, enqueued %+v", m.Full, jobs)
	}
}
