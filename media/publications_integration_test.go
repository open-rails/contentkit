package media_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/internal/s3test"
	"github.com/open-rails/contentkit/media/layout"
)

// queryCounter counts the statements that read the publication projection.
type queryCounter struct{ n *atomic.Int32 }

func (c queryCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	if strings.Contains(d.SQL, "content_media_publications") {
		c.n.Add(1)
	}
	return ctx
}

func (queryCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// readCounter counts the bucket reads a lookup makes.
type readCounter struct {
	media.Store
	n atomic.Int32
}

func (s *readCounter) Get(ctx context.Context, key string, o media.GetOptions) (io.ReadCloser, media.Object, error) {
	s.n.Add(1)
	return s.Store.Get(ctx, key, o)
}

func (s *readCounter) Head(ctx context.Context, key string) (media.Object, error) {
	s.n.Add(1)
	return s.Store.Head(ctx, key)
}

// lookups is a host's Manifests over the fixture's database and bucket,
// counting projection queries and bucket reads.
func (f *fixture) lookups() (*media.Manifests, *atomic.Int32, *readCounter) {
	f.t.Helper()
	queries := &atomic.Int32{}
	pool := pgtest.Pool(f.t, func(c *pgxpool.Config) { c.ConnConfig.Tracer = queryCounter{queries} })
	journal, err := media.NewPGJournal(pool, f.env.ContentSchema(), nil)
	if err != nil {
		f.t.Fatal(err)
	}
	store := &readCounter{Store: f.env.Store}
	return s3test.Manifests(f.t, store, f.reg, media.ManifestOptions{Journal: journal}), queries, store
}

// current is ref's current image of preset as its manifest says.
func (f *fixture) current(ref contentref.ContentRef, path, preset string) media.PublicImage {
	f.t.Helper()
	m, _, err := f.ms.Get(f.t.Context(), ref)
	if err != nil {
		f.t.Fatal(err)
	}
	file, _ := m.Get(path)
	pub, ok := file.Current(preset)
	if !ok {
		f.t.Fatalf("%s %s has no current %s", ref, path, preset)
	}
	image := media.PublicImage{From: path, Preset: preset}
	for i, name := range pub.NamesOnDisk() {
		image.Renditions = append(image.Renditions, media.PublicRendition{URL: f.reg.PublicURL(ref, name), W: pub.Dims[i].W, H: pub.Dims[i].H})
	}
	return image
}

// republish stands in for a reprocess that publishes a new generation.
func (f *fixture) republish(ms *media.Manifests, ref contentref.ContentRef, path, preset string) (media.Publication, error) {
	f.t.Helper()
	ctx := f.t.Context()
	item, _ := f.reg.Item(ref)
	m, _, err := ms.Get(ctx, ref)
	if err != nil {
		f.t.Fatal(err)
	}
	file, _ := m.Get(path)
	names := item.Kind().PublicNames(m, item.Kind().PublicFor(path)[0], path)
	pub := media.Publication{Preset: preset, Source: file.Key(), FP: "republished", Generation: uuid.NewString(),
		Names: names, Dims: make([]media.Dims, len(names)), State: media.PublicationReady}
	src, _ := item.Blob(file.Blob)
	for i, name := range pub.NamesOnDisk() {
		pub.Dims[i] = media.Dims{W: 2, H: 3}
		key, _ := item.Public(name)
		if _, err := f.env.Store.Copy(ctx, src, key, media.CopyOptions{}); err != nil {
			f.t.Fatal(err)
		}
	}
	_, err = ms.EditExisting(ctx, ref, func(m *media.Manifest) error {
		m.SetPublication(path, pub)
		return nil
	})
	return pub, err
}

func urlsOf(images ...media.PublicImage) []string {
	var out []string
	for _, image := range images {
		for _, r := range image.Renditions {
			out = append(out, r.URL)
		}
	}
	return out
}

// A list page resolves every card's cover, every avatar and every inline
// image it shows with one indexed query and no bucket read. Items without a
// publication get their preset's default.
func TestPublicImagesBatch(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	var galleries []contentref.ContentRef
	for n := 1; n <= 12; n++ {
		f.visible(n)
		ref := f.ref("gallery", n)
		f.put(ref, "cover.png", "image/png", png(n))
		if n%4 != 0 {
			f.produce(ref)
		}
		galleries = append(galleries, ref)
	}
	f.visible(20)
	post := f.ref("post", 20)
	var inline []string
	for i := range 3 {
		path, blob := f.upload(post, "inline/x.png", "image/png", png(200+i))
		f.commit(post, media.Op{Op: media.OpPut, Path: path, Blob: blob})
		item, _ := f.reg.Item(post)
		inline = append(inline, item.Kind().NameOf(path))
	}
	f.produce(post)

	ms, queries, reads := f.lookups()
	queries.Store(0)
	covers, err := ms.PresetImages(ctx, "cover", galleries...)
	if err != nil {
		t.Fatal(err)
	}
	if q, r := queries.Load(), reads.n.Load(); q != 1 || r != 0 {
		t.Fatalf("12 covers took %d projection queries and %d bucket reads", q, r)
	}
	for i, ref := range galleries {
		if (i+1)%4 == 0 {
			def, ok := f.reg.DefaultImage(ref, "cover")
			if !ok || !reflect.DeepEqual(covers[i], def) || covers[i].From != "" || len(def.Renditions) != 2 || def.Renditions[0].H != 325 {
				t.Fatalf("unpublished %s: %+v, default %+v", ref, covers[i], def)
			}
			continue
		}
		if got := f.current(ref, "cover.png", "cover"); !reflect.DeepEqual(covers[i], got) {
			t.Fatalf("cover of %s: %+v, manifest %+v", ref, covers[i], got)
		}
	}
	if status, body, _ := f.fetch(covers[0].Renditions[0].URL); status != http.StatusOK || body != string(png(1)) {
		t.Fatalf("a looked-up cover: %d", status)
	}

	// Inline images by name, with a whole item and a missing name, in one query.
	queries.Store(0)
	found, err := ms.Images(ctx, media.ImageQuery{Ref: post, Name: inline[2]}, media.ImageQuery{Ref: galleries[0]},
		media.ImageQuery{Ref: post, Name: inline[0], Preset: "inline"}, media.ImageQuery{Ref: post, Name: "i-" + uuid.NewString()})
	if err != nil || queries.Load() != 1 {
		t.Fatalf("mixed lookup: %d queries, %v", queries.Load(), err)
	}
	m, _, _ := f.ms.Get(ctx, post)
	pathOf := func(name string) string {
		for _, file := range m.Files {
			if file.IsUpload() && strings.Contains(file.Path, name) {
				return file.Path
			}
		}
		return ""
	}
	if len(found[0]) != 1 || !reflect.DeepEqual(found[0][0], f.current(post, pathOf(inline[2]), "inline")) ||
		len(found[1]) != 1 || found[1][0].Preset != "cover" ||
		len(found[2]) != 1 || found[2][0].From != pathOf(inline[0]) || len(found[3]) != 0 {
		t.Fatalf("named lookup: %+v", found)
	}
	all, err := ms.PublicImages(ctx, post)
	if err != nil || len(all) != 3 {
		t.Fatalf("an item's images: %+v %v", all, err)
	}
	read, err := f.rd.Read(ctx, galleries[0], f.editor, media.ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if single, err := ms.PublicImages(ctx, galleries[0]); err != nil || !reflect.DeepEqual(single, read.Public) {
		t.Fatalf("lookup %+v, read API %+v, %v", single, read.Public, err)
	}
	if _, err := ms.Images(ctx, media.ImageQuery{Ref: post, Preset: "cover"}); err == nil {
		t.Fatal("a preset the kind lacks was accepted")
	}
}

// A manifest write whose response was lost leaves the projection behind until
// recovery settles it; cleanup recovers first, so it retires the old
// generation only once the projection names the new one.
func TestRecoveryKeepsProjection(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	f.visible(1)
	ref := f.ref("gallery", 1)
	f.put(ref, "cover.png", "image/png", png(1))
	f.produce(ref)
	before := f.current(ref, "cover.png", "cover")
	lost := errors.New("response lost after the manifest PUT")
	item, _ := f.reg.Item(ref)
	s := &lossyStore{Store: f.env.Store, key: item.ManifestKey(), land: true, err: lost}
	if _, err := f.republish(s3test.Manifests(t, s, f.reg, media.ManifestOptions{Journal: f.journal}), ref, "cover.png", "cover"); !errors.Is(err, lost) {
		t.Fatalf("interrupted republish: %v", err)
	}
	if got, err := f.ms.PresetImages(ctx, "cover", ref); err != nil || !reflect.DeepEqual(got[0], before) {
		t.Fatalf("an unsettled write moved the projection: %+v %v", got, err)
	}
	keys, err := f.ms.SyncPublic(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	after := f.current(ref, "cover.png", "cover")
	if got, err := f.ms.PresetImages(ctx, "cover", ref); err != nil || !reflect.DeepEqual(got[0], after) || reflect.DeepEqual(after, before) {
		t.Fatalf("recovered projection %+v, manifest %+v, %v", got, after, err)
	}
	for _, u := range urlsOf(before) {
		key := strings.TrimPrefix(u, "https://"+mediaHost+layout.URLPrefix)
		if !slices.Contains(keys, key) || f.exists(key) {
			t.Fatalf("old generation %s not retired: %v", key, keys)
		}
	}
	for _, u := range urlsOf(after) {
		if status, _, _ := f.fetch(u); status != http.StatusOK {
			t.Fatalf("current %s: %d", u, status)
		}
	}
	// The pending recovery job finds nothing left, and the projection holds.
	if err := f.ms.RecoverPending(ctx, 10); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.ms.PresetImages(ctx, "cover", ref); !reflect.DeepEqual(got[0], after) {
		t.Fatalf("projection after recovery: %+v", got)
	}
}

// lossyStore fails the PUTs of key: after landing it (a lost response), or
// instead of it, until it has failed times times (once by default).
type lossyStore struct {
	media.Store
	key    string
	land   bool
	err    error
	failed atomic.Int32
	times  int32
}

func (s *lossyStore) Put(ctx context.Context, key string, body io.Reader, size int64, o media.PutOptions) (media.Object, error) {
	if key != s.key || s.failed.Load() >= max(s.times, 1) {
		return s.Store.Put(ctx, key, body, size, o)
	}
	s.failed.Add(1)
	if s.land {
		if _, err := s.Store.Put(ctx, key, body, size, o); err != nil {
			return media.Object{}, err
		}
	}
	return media.Object{}, s.err
}
