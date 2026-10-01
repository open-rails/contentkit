package media_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/internal/s3test"
)

// A manifest never grows past what readers load (audit H2). The audit's PoC,
// a 10 MiB meta value, is refused where ops set meta; puts with meta at its
// bound grow the manifest until a commit would pass MaxCommitBytes, which is
// refused with the manifest unchanged. The item then still reads, hides and
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
	if _, _, err := fresh.Get(ctx, g); !errors.Is(err, media.ErrManifestTooLarge) {
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
		if extra == 0 && err != nil || extra == 1 && !errors.Is(err, media.ErrManifestTooLarge) {
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
