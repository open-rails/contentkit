package media_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/internal/s3test"
)

// sharedLegacyGallery stores gallery n as v0.67 did with content-addressed
// blobs: a page no wider than the thumb renders thumb and high byte-identical
// into one blob, and a repeated page shares its upload's blob and its
// outputs'. It returns the shared output's bytes.
func (f *fixture) sharedLegacyGallery(n int) (*legacy, []byte) {
	f.t.Helper()
	l := &legacy{ref: f.ref("gallery", n), cover: png(n), public: map[string][]byte{}}
	l.item, _ = f.reg.Item(l.ref)
	page, other := png(500+n), png(600+n)
	small := append(webpHeader(460, 650), fmt.Sprintf("small page %d", n)...)
	thumb := append(webpHeader(460, 650), fmt.Sprintf("thumb %d", n)...)
	high := append(webpHeader(1200, 1700), fmt.Sprintf("high %d", n)...)
	upload := func(path string, b []byte, w, h int) media.File {
		return media.File{Path: path, Blob: legacyBlob(b), Type: "image/png", Size: int64(len(b)), W: w, H: h}
	}
	output := func(path, from, preset string, b []byte, w, h int) media.File {
		return media.File{Path: path, Blob: legacyBlob(b), Type: "image/webp", Size: int64(len(b)), W: w, H: h,
			From: from, Preset: preset, FP: "v067-" + preset}
	}
	files := []media.File{
		upload("originals/001.png", page, 460, 650),
		upload("originals/002.png", page, 460, 650),
		upload("originals/003.png", other, 1200, 1700),
		upload("cover.png", l.cover, 920, 1300),
		output("thumb/001.webp", "originals/001.png", "thumb", small, 460, 650),
		output("thumb/002.webp", "originals/002.png", "thumb", small, 460, 650),
		output("thumb/003.webp", "originals/003.png", "thumb", thumb, 460, 650),
		output("high/001.webp", "originals/001.png", "high", small, 460, 650),
		output("high/002.webp", "originals/002.png", "high", small, 460, 650),
		output("high/003.webp", "originals/003.png", "high", high, 1200, 1700),
	}
	for _, b := range [][]byte{page, other, l.cover, small, thumb, high} {
		key, _ := l.item.Blob(legacyBlob(b))
		f.putObject(key, b, media.PutOptions{ContentType: "application/octet-stream"})
	}
	for name, size := range map[string][2]int{"cover-230.webp": {230, 325}, "cover-460.webp": {460, 650}} {
		l.public[name] = webpHeader(size[0], size[1])
		key, _ := l.item.Public(name)
		f.putObject(key, l.public[name], media.PutOptions{ContentType: "image/webp", Metadata: map[string]string{"fp": "v067-fp"}})
	}
	f.storeLegacyManifest(l.item, false, files)
	return l, small
}

// allocations lists the item's allocation keys and whether each is live
// under incarnation.
func (f *fixture) allocations(item media.Item, incarnation string) map[string]bool {
	f.t.Helper()
	rows, err := f.env.Pool().Query(f.t.Context(), `SELECT object_key, retired_at IS NULL AND incarnation::text = $3
FROM `+pgx.Identifier{f.env.ContentSchema(), "content_media_allocations"}.Sanitize()+`
WHERE tenant_id = $1 AND folder_prefix = $2`, item.Ref().TenantID, item.Prefix(), incarnation)
	if err != nil {
		f.t.Fatal(err)
	}
	out := map[string]bool{}
	for rows.Next() {
		var key string
		var live bool
		if err := rows.Scan(&key, &live); err != nil {
			f.t.Fatal(err)
		}
		out[key] = live
	}
	if err := rows.Err(); err != nil {
		f.t.Fatal(err)
	}
	return out
}

// served fetches path's URL from a full read of ref.
func (f *fixture) served(ref contentref.ContentRef, path string) (int, string) {
	f.t.Helper()
	read, err := f.rd.Read(f.t.Context(), ref, f.editor, media.ReadOptions{})
	if err != nil {
		f.t.Fatal(err)
	}
	u, ok := urls(read)[path]
	if !ok {
		return http.StatusNotFound, ""
	}
	status, body, _ := f.fetch(u)
	return status, body
}

// v0.67 stored files with equal bytes as one blob. The upgrade adopts each
// blob once; the shared blob serves every file and outlives each one until
// nothing references it.
func TestUpgradeAdoptsSharedLegacyBlobs(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	f.visible(1)
	f.visible(2)
	l, small := f.sharedLegacyGallery(1)
	f.legacyGallery(2, false)

	st, err := f.jobs.Upgrade(ctx, 1000)
	if err != nil || !st.Done || len(st.Failures) != 0 || st.Kinds[0].Upgraded != 2 {
		t.Fatalf("upgrade: %+v %v", st, err)
	}
	m, etag, err := f.ms.Get(ctx, l.ref)
	if err != nil {
		t.Fatal(err)
	}
	shared, _ := l.item.Blob(legacyBlob(small))
	page, _ := l.item.Blob(legacyBlob(png(501)))
	want := map[string]bool{}
	for _, b := range m.Blobs() {
		key, _ := l.item.Blob(b)
		want[key] = true
	}
	if got := f.allocations(l.item, m.Incarnation); len(want) != 6 || !maps.Equal(got, want) {
		t.Fatalf("allocations %v, want each of %v live once", got, want)
	}
	for _, path := range []string{"thumb/001.webp", "high/001.webp", "thumb/002.webp", "high/002.webp"} {
		if status, body := f.served(l.ref, path); status != http.StatusOK || body != string(small) {
			t.Fatalf("%s: %d", path, status)
		}
	}

	// A rerun converges without writing.
	if _, err := f.env.Pool().Exec(ctx, "DELETE FROM "+pgx.Identifier{f.env.ContentSchema(), "content_media_upgrades"}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	if again, err := f.jobs.Upgrade(ctx, 1000); err != nil || !again.Done || again.Kinds[0].Upgraded != 0 {
		t.Fatalf("rerun: %+v %v", again, err)
	}
	if _, now, _ := f.ms.Get(ctx, l.ref); now != etag {
		t.Fatal("a rerun rewrote an upgraded manifest")
	}

	// The worker replaces one output and drops its old blob, which others use.
	body := []byte("high 001, rendered again")
	sum := sha256.Sum256(body)
	name, err := f.ms.NewBlob(ctx, l.ref, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	key, _ := l.item.Blob(name)
	f.putObject(key, body, media.PutOptions{ContentType: "image/webp"})
	if _, err := f.ms.EditExisting(ctx, l.ref, func(m *media.Manifest) error {
		out, _ := m.Get("high/001.webp")
		old := out.Blob
		out.Blob, out.Size, out.FP = name, int64(len(body)), "v068"
		if err := m.SetOutputs("originals/001.png", "high", []media.File{out}); err != nil {
			return err
		}
		return f.ms.DeleteUnreferenced(ctx, l.item, m, []string{old})
	}); err != nil {
		t.Fatal(err)
	}
	// A takedown removes a repeated page: its upload and outputs share blobs
	// with the first page's.
	f.commit(l.ref, media.Op{Op: media.OpRemove, Path: "originals/002.png", Takedown: true})
	if _, err := f.later().Sweep(ctx, l.ref); err != nil {
		t.Fatal(err)
	}
	if err := f.ms.DropUnreferenced(ctx, l.ref, media.Unreferenced{All: true}); err != nil {
		t.Fatal(err)
	}
	if !f.exists(shared) || !f.exists(page) {
		t.Fatal("a shared blob went while files still use it")
	}
	if status, got := f.served(l.ref, "thumb/001.webp"); status != http.StatusOK || got != string(small) {
		t.Fatalf("thumb/001.webp after its twins went: %d", status)
	}
	if status, got := f.served(l.ref, "high/001.webp"); status != http.StatusOK || got != string(body) {
		t.Fatalf("re-rendered high/001.webp: %d", status)
	}
	if _, err := f.ms.Edit(ctx, l.ref, func(m *media.Manifest) error { m.Meta = map[string]any{"title": "kept"}; return nil }); err != nil {
		t.Fatalf("an edit over the remaining allocations: %v", err)
	}

	// Once the last file using it goes, so does the blob.
	f.commit(l.ref, media.Op{Op: media.OpRemove, Path: "originals/001.png", Takedown: true})
	if f.exists(shared) || f.exists(page) || f.exists(key) {
		t.Fatal("a takedown kept blobs nothing references")
	}
	if status, _ := f.served(l.ref, "high/003.webp"); status != http.StatusOK {
		t.Fatalf("an unrelated page: %d", status)
	}
}

// faultyStore fails reads of one key with err while err is set.
type faultyStore struct {
	media.Store
	key string
	err atomic.Pointer[error]
}

func (s *faultyStore) fail(err error) {
	if err == nil {
		s.err.Store(nil)
		return
	}
	s.err.Store(&err)
}

func (s *faultyStore) Get(ctx context.Context, key string, o media.GetOptions) (io.ReadCloser, media.Object, error) {
	if err := s.err.Load(); err != nil && key == s.key {
		return nil, media.Object{}, *err
	}
	return s.Store.Get(ctx, key, o)
}

// objects is every object under prefix with its ETag.
func (f *fixture) objects(prefix string) map[string]string {
	f.t.Helper()
	out := map[string]string{}
	for o, err := range f.env.Store.List(f.t.Context(), prefix) {
		if err != nil {
			f.t.Fatal(err)
		}
		out[o.Key] = o.ETag
	}
	return out
}

// An item the upgrade cannot convert is recorded and left as it was; the
// River job finishes the pass over the others. After a fix a rerun upgrades
// it and clears its failure.
func TestUpgradeIsolatesBrokenItems(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	for n := 1; n <= 4; n++ {
		f.visible(n)
	}
	corrupt, _ := f.reg.Item(f.ref("gallery", 1))
	f.putObject(corrupt.ManifestKey(), []byte("not a manifest"), media.PutOptions{ContentType: "application/gzip"})
	denied := f.legacyGallery(2, false)
	shared, _ := f.sharedLegacyGallery(3)
	plain := f.legacyGallery(4, false)
	before := map[string]map[string]string{}
	for _, item := range []media.Item{corrupt, denied.item} {
		before[item.Prefix()] = f.objects(item.Prefix())
	}

	s := &faultyStore{Store: f.env.Store, key: denied.item.PublicPrefix() + "cover-230.webp"}
	s.fail(fmt.Errorf("s3 GetObject %s: api error AccessDenied: Access Denied", s.key))
	jobs, err := media.NewJobs(media.JobsConfig{Store: s, Registry: f.reg, Locker: s3test.Locker(t, s), Journal: f.journal,
		Processes: f.q, Pool: f.env.Pool()})
	if err != nil {
		t.Fatal(err)
	}
	pool := f.env.Pool()
	table := pgx.Identifier{riverHost(t, jobs, pool), "river_job"}.Sanitize()
	var state string
	var attempt int
	waitFor(t, "the upgrade job", func() bool {
		err := pool.QueryRow(ctx, "SELECT state, attempt FROM "+table+" WHERE kind = 'contentkit_media_upgrade' ORDER BY id LIMIT 1").Scan(&state, &attempt)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			t.Fatal(err)
		}
		return state == "completed" || state == "retryable" || state == "discarded"
	})
	if state != "completed" || attempt != 1 {
		t.Fatalf("upgrade job %s after %d attempts", state, attempt)
	}

	st, err := jobs.UpgradeStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	gallery := st.Kinds[slices.IndexFunc(st.Kinds, func(k media.KindUpgrade) bool { return k.Kind == "gallery" })]
	if st.Done || gallery.Finished == nil || gallery.Visited != 4 || gallery.Upgraded != 2 || gallery.Failed != 2 || len(st.Failures) != 2 {
		t.Fatalf("status %+v", st)
	}
	failed := map[string]string{}
	for _, fl := range st.Failures {
		failed[fl.Ref.ContentID] = fl.Error
	}
	if failed[cid(1)] == "" || !strings.Contains(failed[cid(2)], "AccessDenied") {
		t.Fatalf("failures %+v", st.Failures)
	}
	for _, item := range []media.Item{corrupt, denied.item} {
		if now := f.objects(item.Prefix()); !maps.Equal(now, before[item.Prefix()]) {
			t.Fatalf("a failed upgrade changed %s:\nbefore %v\nafter  %v", item.Prefix(), before[item.Prefix()], now)
		}
	}
	if _, _, err := f.ms.Get(ctx, denied.ref); !errors.Is(err, media.ErrUpgradeRequired) {
		t.Fatalf("a failed item: %v", err)
	}
	for _, ref := range []contentref.ContentRef{shared.ref, plain.ref} {
		if m, _, err := f.ms.Get(ctx, ref); err != nil || m.V != media.ManifestVersion {
			t.Fatalf("%s after the pass: %v", ref, err)
		}
	}

	// Fixed: the rerun upgrades both and clears their failures.
	s.fail(nil)
	f.storeLegacyManifest(corrupt, false, []media.File{{Path: "cover.png", Blob: legacyBlob(png(1)), Type: "image/png", Size: int64(len(png(1)))}})
	key, _ := corrupt.Blob(legacyBlob(png(1)))
	f.putObject(key, png(1), media.PutOptions{ContentType: "image/png"})
	if st, err := jobs.Upgrade(ctx, 1000); err != nil || !st.Done || len(st.Failures) != 0 {
		t.Fatalf("rerun after the fix: %+v %v", st, err)
	}
	etags := map[string]string{}
	for _, ref := range []contentref.ContentRef{f.ref("gallery", 1), denied.ref, shared.ref, plain.ref} {
		m, etag, err := f.ms.Get(ctx, ref)
		if err != nil || m.V != media.ManifestVersion {
			t.Fatalf("%s after the fix: %v", ref, err)
		}
		etags[ref.ContentID] = etag
	}
	if covers, err := f.ms.PresetImages(ctx, "cover", denied.ref); err != nil || covers[0].From != "cover.png" {
		t.Fatalf("the fixed item's cover: %+v %v", covers, err)
	}

	// Another full pass converges without writing.
	if _, err := pool.Exec(ctx, "DELETE FROM "+pgx.Identifier{f.env.ContentSchema(), "content_media_upgrades"}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	if st, err := jobs.Upgrade(ctx, 1000); err != nil || !st.Done || st.Kinds[0].Upgraded != 0 {
		t.Fatalf("converged rerun: %+v %v", st, err)
	}
	for id, etag := range etags {
		ref, _ := f.reg.Ref("gallery", id)
		if _, now, _ := f.ms.Get(ctx, ref); now != etag {
			t.Fatalf("a converged rerun rewrote %s", id)
		}
	}
}

// An unreachable store fails the run without recording the item; the next
// run resumes there.
func TestUpgradeStopsOnOutage(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	f.visible(1)
	f.visible(2)
	first := f.legacyGallery(1, false)
	second := f.legacyGallery(2, false)
	s := &faultyStore{Store: f.env.Store, key: second.item.ManifestKey()}
	s.fail(fmt.Errorf("%w: s3 GetObject %s", media.ErrUnavailable, s.key))
	jobs, err := media.NewJobs(media.JobsConfig{Store: s, Registry: f.reg, Locker: s3test.Locker(t, s), Journal: f.journal})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.Upgrade(ctx, 1000); !errors.Is(err, media.ErrUnavailable) {
		t.Fatalf("upgrade during an outage: %v", err)
	}
	st, err := jobs.UpgradeStatus(ctx)
	if err != nil || len(st.Failures) != 0 || st.Kinds[0].After != first.ref.ContentID || st.Kinds[0].Finished != nil {
		t.Fatalf("status after the outage: %+v %v", st, err)
	}
	s.fail(nil)
	if st, err := jobs.Upgrade(ctx, 1000); err != nil || !st.Done || st.Kinds[0].Upgraded != 2 {
		t.Fatalf("upgrade after the outage: %+v %v", st, err)
	}
}
