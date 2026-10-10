package media_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
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

// legacy is an item a v0.67 host stored: a version 2 manifest, private blobs
// named by digest alone, and public files at their template names.
type legacy struct {
	ref     contentref.ContentRef
	item    media.Item
	cover   []byte            // the cover upload's bytes
	thumb   []byte            // a page's private output
	public  map[string][]byte // public name -> bytes
	objects map[string]string // every key -> ETag, as stored
}

func webpHeader(w, h int) []byte {
	b := make([]byte, 40)
	copy(b, "RIFF")
	binary.LittleEndian.PutUint32(b[4:], 32)
	copy(b[8:], "WEBPVP8L")
	binary.LittleEndian.PutUint32(b[16:], 20)
	b[20] = 0x2f
	binary.LittleEndian.PutUint32(b[21:], uint32(w-1)|uint32(h-1)<<14)
	return b
}

func legacyBlob(b []byte) string {
	sum := sha256.Sum256(b)
	return layout.SHA256Name(sum[:])
}

func (f *fixture) putObject(key string, body []byte, o media.PutOptions) {
	f.t.Helper()
	if _, err := f.env.Store.Put(f.t.Context(), key, bytes.NewReader(body), int64(len(body)), o); err != nil {
		f.t.Fatal(err)
	}
}

// legacyGallery stores gallery n as v0.67 did: a cover with its two
// renditions, a page with its thumb, and a stray public file.
func (f *fixture) legacyGallery(n int, hidden bool) *legacy {
	f.t.Helper()
	l := &legacy{ref: f.ref("gallery", n), cover: png(n), thumb: png(1000 + n), public: map[string][]byte{}}
	l.item, _ = f.reg.Item(l.ref)
	page := png(500 + n)
	files := []media.File{
		{Path: "originals/001.png", Blob: legacyBlob(page), Type: "image/png", Size: int64(len(page)), W: 920, H: 1300},
		{Path: "cover.png", Blob: legacyBlob(l.cover), Type: "image/png", Size: int64(len(l.cover)), W: 920, H: 1300},
		{Path: "thumb/001.webp", Blob: legacyBlob(l.thumb), Type: "image/webp", Size: int64(len(l.thumb)), From: "originals/001.png", Preset: "thumb", FP: "v067"},
	}
	for _, b := range [][]byte{page, l.cover, l.thumb} {
		key, _ := l.item.Blob(legacyBlob(b))
		f.putObject(key, b, media.PutOptions{ContentType: "image/png"})
	}
	for name, size := range map[string][2]int{"cover-230.webp": {230, 325}, "cover-460.webp": {460, 650}, "cover-920.webp": {920, 1300}} {
		l.public[name] = webpHeader(size[0], size[1])
		key, _ := l.item.Public(name)
		f.putObject(key, l.public[name], media.PutOptions{ContentType: "image/webp", Metadata: map[string]string{"from": "cover.png", "fp": "v067-fp"}})
	}
	f.storeLegacyManifest(l.item, hidden, files)
	return l
}

func (f *fixture) storeLegacyManifest(item media.Item, hidden bool, files []media.File) {
	f.t.Helper()
	body, err := json.Marshal(media.Manifest{V: 2, Hidden: hidden, Files: files})
	if err != nil {
		f.t.Fatal(err)
	}
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	zw.Write(body)
	zw.Close()
	f.putObject(item.ManifestKey(), b.Bytes(), media.PutOptions{ContentType: "application/gzip", Metadata: map[string]string{"upload-bytes": "0"}})
}

// snapshot is every object under the test's namespace with its ETag.
func (f *fixture) snapshot() map[string]string {
	f.t.Helper()
	out := map[string]string{}
	for o, err := range f.env.Store.List(f.t.Context(), f.ns+"/") {
		if err != nil {
			f.t.Fatal(err)
		}
		out[o.Key] = o.ETag
	}
	return out
}

// Before the upgrade a v0.67 item answers a distinct error everywhere and
// nothing deletes any of its files. The upgrade converts it in place: its
// blobs stay, its template-named covers become its current publication, and
// their URLs keep working through cleanup until a reprocess publishes a
// generation.
func TestLegacyItemUpgrade(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	f.visible(1)
	f.visible(2)
	l := f.legacyGallery(1, false)
	hidden := f.legacyGallery(2, true)
	post := f.ref("post", 3)
	f.visible(3)
	postItem, _ := f.reg.Item(post)
	inline := "i-" + uuid.NewString()
	art := png(300)
	artKey, _ := postItem.Blob(legacyBlob(art))
	f.putObject(artKey, art, media.PutOptions{ContentType: "image/png"})
	artPublic, _ := postItem.Public(inline + ".webp")
	f.putObject(artPublic, webpHeader(64, 48), media.PutOptions{ContentType: "image/webp"})
	f.storeLegacyManifest(postItem, false, []media.File{{Path: "inline/" + inline + ".png", Blob: legacyBlob(art), Type: "image/png", Size: int64(len(art))}})
	stored := f.snapshot()

	if _, _, err := f.ms.Get(ctx, l.ref); !errors.Is(err, media.ErrUpgradeRequired) {
		t.Fatalf("a v2 manifest read: %v", err)
	}
	srv := httptest.NewServer(f.rd.Handler(media.HandlerOptions{Identity: identity{f.editor}, Limit: media.RateLimit{Disabled: true}}))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/gallery/" + cid(1))
	if err != nil {
		t.Fatal(err)
	}
	var reply media.ErrorReply
	_ = json.NewDecoder(resp.Body).Decode(&reply)
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable || reply.Code != "upgrade_required" {
		t.Fatalf("read API before the upgrade: %d %+v", resp.StatusCode, reply)
	}
	if _, err := f.up.Commit(ctx, f.editor, l.ref, uuid.NewString(), []media.Op{{Op: media.OpMeta, Meta: map[string]any{"title": "x"}}}); !errors.Is(err, media.ErrUpgradeRequired) {
		t.Fatalf("a commit before the upgrade: %v", err)
	}
	for name, run := range map[string]func() error{
		"sync":    func() error { _, err := f.ms.SyncPublic(ctx, l.ref); return err },
		"sweep":   func() error { _, err := f.later().Sweep(ctx, l.ref); return err },
		"drop":    func() error { return f.ms.DropUnreferenced(ctx, l.ref, media.Unreferenced{All: true}) },
		"pass":    func() error { return f.later().SweepAll(ctx) },
		"recover": func() error { return f.ms.RecoverPending(ctx, 10) },
	} {
		if err := run(); err != nil && !errors.Is(err, media.ErrUpgradeRequired) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if now := f.snapshot(); !reflect.DeepEqual(now, stored) {
		t.Fatalf("cleanup touched a v2 item:\nbefore %v\nafter  %v", stored, now)
	}

	st, err := f.jobs.Upgrade(ctx, 1000)
	if err != nil || !st.Done || len(st.Failures) != 0 {
		t.Fatalf("upgrade: %+v %v", st, err)
	}
	for _, k := range st.Kinds {
		want := map[string]int64{"gallery": 2, "post": 1}[k.Kind]
		if k.Upgraded != want || k.Finished == nil {
			t.Fatalf("kind %s: %+v", k.Kind, k)
		}
	}
	m, etag, err := f.ms.Get(ctx, l.ref)
	if err != nil {
		t.Fatal(err)
	}
	cover, _ := m.Get("cover.png")
	pub, ok := cover.Current("cover")
	if m.V != media.ManifestVersion || !ok || pub.Generation != "" || pub.FP != "v067-fp" || cover.Blob != legacyBlob(l.cover) ||
		!reflect.DeepEqual(pub.Dims, []media.Dims{{W: 230, H: 325}, {W: 460, H: 650}}) {
		t.Fatalf("upgraded cover %+v", cover)
	}
	want := media.PublicImage{From: "cover.png", Preset: "cover", Renditions: []media.PublicRendition{
		{URL: f.reg.PublicURL(l.ref, "cover-230.webp"), W: 230, H: 325}, {URL: f.reg.PublicURL(l.ref, "cover-460.webp"), W: 460, H: 650}}}
	covers, err := f.ms.PresetImages(ctx, "cover", l.ref, hidden.ref)
	if err != nil || !reflect.DeepEqual(covers[0], want) || covers[1].From != "" {
		t.Fatalf("projected covers %+v %v", covers, err)
	}
	if found, err := f.ms.Images(ctx, media.ImageQuery{Ref: post, Name: inline}); err != nil || len(found[0]) != 1 ||
		found[0][0].Renditions[0] != (media.PublicRendition{URL: f.reg.PublicURL(post, inline+".webp"), W: 64, H: 48}) {
		t.Fatalf("legacy inline image: %+v %v", found, err)
	}
	for _, key := range []string{l.item.PublicPrefix() + "cover-920.webp", hidden.item.PublicPrefix() + "cover-230.webp"} {
		if f.exists(key) {
			t.Fatalf("%s is not current and was kept", key)
		}
	}
	for _, r := range want.Renditions {
		if status, body, _ := f.fetch(r.URL); status != http.StatusOK || body != string(l.public[strings.TrimPrefix(r.URL, f.reg.PublicURL(l.ref, ""))]) {
			t.Fatalf("legacy cover %s: %d", r.URL, status)
		}
	}
	read, err := f.rd.Read(ctx, l.ref, f.editor, media.ReadOptions{})
	if err != nil || !reflect.DeepEqual(read.Public, []media.PublicImage{want}) {
		t.Fatalf("read API after the upgrade: %+v %v", read, err)
	}
	thumb := urls(read)["thumb/001.webp"]
	if status, body, _ := f.fetch(thumb); status != http.StatusOK || body != string(l.thumb) {
		t.Fatalf("a legacy private blob: %d", status)
	}
	if keys, err := f.ms.SyncPublic(ctx, l.ref); err != nil || len(keys) != 0 {
		t.Fatalf("cleanup retired current legacy files: %v %v", keys, err)
	}
	if res, err := f.later().Sweep(ctx, l.ref); err != nil || slices.ContainsFunc(res.Deleted, func(k string) bool {
		return strings.Contains(k, "/public/") || strings.HasSuffix(k, legacyBlob(l.cover)) || strings.HasSuffix(k, legacyBlob(l.thumb))
	}) {
		t.Fatalf("a sweep deleted current files: %+v %v", res, err)
	}
	if _, err := f.ms.Edit(ctx, l.ref, func(m *media.Manifest) error { m.Meta = map[string]any{"title": "kept"}; return nil }); err != nil {
		t.Fatalf("an edit after the upgrade: %v", err)
	}

	// A fresh pass over upgraded items writes nothing and keeps the projection.
	_, etag, _ = f.ms.Get(ctx, l.ref)
	if _, err := f.env.Pool().Exec(ctx, "DELETE FROM "+pgx.Identifier{f.env.ContentSchema(), "content_media_upgrades"}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	if again, err := f.jobs.Upgrade(ctx, 1000); err != nil || !again.Done || again.Kinds[0].Upgraded != 0 || again.Kinds[0].Visited != 2 {
		t.Fatalf("rerun: %+v %v", again, err)
	}
	if _, now, _ := f.ms.Get(ctx, l.ref); now != etag {
		t.Fatal("a rerun rewrote an upgraded manifest")
	}
	if got, _ := f.ms.PresetImages(ctx, "cover", l.ref); !reflect.DeepEqual(got[0], want) {
		t.Fatalf("projection after a rerun: %+v", got)
	}

	// A reprocess publishes a generation; only then do the legacy names go.
	if _, err := f.republish(f.ms, l.ref, "cover.png", "cover"); err != nil {
		t.Fatal(err)
	}
	next := f.current(l.ref, "cover.png", "cover")
	if got, _ := f.ms.PresetImages(ctx, "cover", l.ref); !reflect.DeepEqual(got[0], next) {
		t.Fatalf("projection after the reprocess: %+v", got)
	}
	if _, err := f.ms.SyncPublic(ctx, l.ref); err != nil {
		t.Fatal(err)
	}
	for _, r := range want.Renditions {
		if f.exists(strings.TrimPrefix(r.URL, "https://"+mediaHost+layout.URLPrefix)) {
			t.Fatalf("superseded legacy file %s kept", r.URL)
		}
	}
}

// A crash at the upgrade's manifest write leaves the item version 2 or
// version 3 with a pending receipt; the next run converges either way.
func TestUpgradeConvergesAfterCrash(t *testing.T) {
	for _, land := range []bool{false, true} {
		t.Run(map[bool]string{false: "not landed", true: "response lost"}[land], func(t *testing.T) {
			f := newFixture(t)
			ctx := t.Context()
			f.visible(1)
			l := f.legacyGallery(1, false)
			crash := errors.New("crashed at the manifest write")
			s := &lossyStore{Store: f.env.Store, key: l.item.ManifestKey(), land: land, err: crash}
			jobs, err := media.NewJobs(media.JobsConfig{Store: s, Registry: f.reg, Locker: s3test.Locker(t, s), Journal: f.journal})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := jobs.Upgrade(ctx, 1000); !errors.Is(err, crash) {
				t.Fatalf("crashed upgrade: %v", err)
			}
			_, _, err = f.ms.Get(ctx, l.ref)
			if land != (err == nil) || !land && !errors.Is(err, media.ErrUpgradeRequired) {
				t.Fatalf("after the crash: %v", err)
			}
			if st, err := f.jobs.Upgrade(ctx, 1000); err != nil || !st.Done {
				t.Fatalf("rerun: %+v %v", st, err)
			}
			covers, err := f.ms.PresetImages(ctx, "cover", l.ref)
			if err != nil || covers[0].From != "cover.png" || covers[0].Renditions[0].URL != f.reg.PublicURL(l.ref, "cover-230.webp") {
				t.Fatalf("projection after the rerun: %+v %v", covers, err)
			}
			// Its blobs are owned by the manifest's incarnation: edits validate them.
			if _, err := f.ms.Edit(ctx, l.ref, func(m *media.Manifest) error { m.Meta = map[string]any{"title": "x"}; return nil }); err != nil {
				t.Fatalf("an edit after convergence: %v", err)
			}
			if status, _, _ := f.fetch(covers[0].Renditions[1].URL); status != http.StatusOK {
				t.Fatalf("legacy cover after convergence: %d", status)
			}
		})
	}
}
