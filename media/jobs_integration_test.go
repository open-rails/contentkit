package media_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	riverhelpers "github.com/open-rails/helpers/river"
	"github.com/riverqueue/river"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/internal/s3test"
)

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(45 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// later is the fixture's jobs at a clock past the grace and temp periods.
func (f *fixture) later() *media.Jobs {
	f.t.Helper()
	j, err := media.NewJobs(media.JobsConfig{Store: f.env.Store, Registry: f.reg, Locker: s3test.Locker(f.t, f.env.Store),
		Processes: f.q, Now: func() time.Time { return time.Now().Add(72 * time.Hour) }})
	if err != nil {
		f.t.Fatal(err)
	}
	return j
}

func (f *fixture) exists(key string) bool {
	f.t.Helper()
	_, err := f.env.Store.Head(context.Background(), key)
	if err != nil && !errors.Is(err, media.ErrNotFound) {
		f.t.Fatal(err)
	}
	return err == nil
}

func (f *fixture) object(key, body string) {
	f.t.Helper()
	if _, err := f.env.Store.Put(context.Background(), key, strings.NewReader(body), int64(len(body)), media.PutOptions{}); err != nil {
		f.t.Fatal(err)
	}
}

// The sweep collects by manifest reference: unexpected public names at once
// (purged), unreferenced blobs and temp/ once past their periods, nothing
// the manifest keeps.
func TestSweep(t *testing.T) {
	f := newFixture(t)
	f.visible(1)
	g := f.gallery(1, 2)
	item, _ := f.reg.Item(g)
	ctx := context.Background()
	old, _ := item.Blob(blobOf(png(100)))
	f.put(g, "originals/000.png", "image/png", png(7)) // replaces page 0: its old blob is unreferenced
	f.produce(g)
	stray, _ := item.Blob(blobOf([]byte("stray")))
	f.object(stray, "stray")
	stale, _ := item.Public("cover-999.webp")
	f.object(stale, "stale")
	f.object(item.TempPrefix()+"u-1", "temp")
	cover, _ := item.Public("cover-230.webp")

	res, err := f.jobs.Sweep(ctx, g)
	if err != nil {
		t.Fatal(err)
	}
	if res.Wait <= 0 || !slices.Equal(res.Deleted, []string{stale}) || !f.exists(old) || !f.exists(cover) {
		t.Fatalf("within grace: %+v", res)
	}
	if urls := f.purges(); len(urls) != 1 || !strings.HasSuffix(urls[0], "/public/cover-999.webp") || !strings.HasPrefix(urls[0], "https://"+mediaHost+"/v1/") {
		t.Fatalf("purged %v", urls)
	}
	res, err = f.later().Sweep(ctx, g)
	if err != nil {
		t.Fatal(err)
	}
	if res.Wait != 0 || f.exists(old) || f.exists(stray) || f.exists(item.TempPrefix()+"u-1") || !f.exists(cover) {
		t.Fatalf("past grace: %+v", res)
	}
	m, _, _ := f.ms.Get(ctx, g)
	for _, b := range m.Blobs() {
		if key, _ := item.Blob(b); !f.exists(key) {
			t.Fatalf("swept referenced %s", key)
		}
	}
	// Uploads never committed are swept once past the temp period; a staged
	// upload the manifest references stays until placed.
	other := f.ref("gallery", 2)
	otherItem, _ := f.reg.Item(other)
	p, committed := f.upload(other, "originals/1.png", "image/png", png(1))
	_, abandoned := f.upload(other, "originals/2.png", "image/png", png(2))
	if _, err := f.up.Commit(ctx, f.editor, other, []media.Op{{Op: media.OpPut, Path: p, Blob: committed}}); err != nil {
		t.Fatal(err)
	}
	kept, _ := otherItem.Staged(committed)
	gone, _ := otherItem.Staged(abandoned)
	if _, err := f.later().Sweep(ctx, other); err != nil || f.exists(gone) || !f.exists(kept) {
		t.Fatalf("staged uploads after the temp period: %v", err)
	}
}

// The sweep deletes an unreferenced blob by the blob's own age, however
// lately the item was edited (audit: an item edited daily was never swept);
// a young one waits out its grace period.
func TestSweepProgressesUnderEdits(t *testing.T) {
	f := newFixture(t)
	f.visible(1)
	g := f.gallery(1, 1)
	item, _ := f.reg.Item(g)
	ctx := context.Background()
	const grace = 6 * time.Second
	jobs, err := media.NewJobs(media.JobsConfig{Store: f.env.Store, Registry: f.reg, Locker: s3test.Locker(t, f.env.Store), Processes: f.q, Grace: grace})
	if err != nil {
		t.Fatal(err)
	}
	old, _ := item.Blob(blobOf(png(100)))
	time.Sleep(grace + time.Second)
	f.put(g, "originals/000.png", "image/png", png(7)) // drops the old blob and edits the manifest just now
	f.produce(g)
	if res, err := jobs.Sweep(ctx, g); err != nil || f.exists(old) || !slices.Contains(res.Deleted, old) {
		t.Fatalf("an old blob survived a fresh edit: %+v %v", res, err)
	}
	young, _ := item.Blob(blobOf(png(7)))
	f.put(g, "originals/000.png", "image/png", png(8))
	f.produce(g)
	if res, err := jobs.Sweep(ctx, g); err != nil || !f.exists(young) || res.Wait <= 0 || res.Wait > grace+time.Second {
		t.Fatalf("a young blob: %+v %v", res, err)
	}
	waitFor(t, "the young blob's sweep", func() bool {
		res, err := jobs.Sweep(ctx, g)
		if err != nil {
			t.Fatal(err)
		}
		return res.Wait == 0
	})
	if f.exists(young) {
		t.Fatal("the young blob was never swept")
	}
	m, _, _ := f.ms.Get(ctx, g)
	for _, b := range m.Blobs() {
		if key, _ := item.Blob(b); !f.exists(key) {
			t.Fatalf("swept referenced %s", key)
		}
	}
}

// A takedown leaves nothing of the upload to fetch: the blobs the commit
// dropped (its own, its outputs', the zip that bundled it), its editor view
// and its public names go at once (purged). It touches nothing else: another
// upload's editor view, an output a job has not recorded yet, an earlier
// version and a blob another file still names all stay. Only an exempt
// grant sweeps the whole item.
func TestTakedown(t *testing.T) {
	f := newFixture(t)
	f.visible(1)
	g := f.gallery(1, 3)
	item, _ := f.reg.Item(g)
	ctx := context.Background()
	staff := access.Actor{ID: "staff", Kind: "user"}
	f.put(g, "originals/000.png", "image/png", png(7))   // page 0's first version is unreferenced
	f.put(g, "originals/003.png", "image/png", png(101)) // the same bytes as page 1
	f.produce(g)
	m, _, _ := f.ms.Get(ctx, g)
	blob := func(body []byte) string { key, _ := item.Blob(blobOf(body)); return key }
	view := func(path string) string {
		u, _ := m.Get(path)
		key, _ := item.Blob(f.reg.EditorView(u))
		f.object(key, "editor view")
		return key
	}
	earlier, page0, page1 := blob(png(100)), blob(png(7)), blob(png(101))
	view0, view2 := view("originals/000.png"), view("originals/002.png")
	unrecorded := blob([]byte("an output a job has not recorded yet"))
	f.object(unrecorded, "unrecorded")
	f.q.take()
	m = f.commit(g, media.Op{Op: media.OpRemove, Path: "originals/000.png", Takedown: true},
		media.Op{Op: media.OpRemove, Path: "originals/001.png", Takedown: true})
	if f.exists(page0) || f.exists(view0) || m.Find("download/pages.zip") >= 0 {
		t.Fatalf("takedown: page 0 kept %v, its editor view %v; %v", f.exists(page0), f.exists(view0), paths(m))
	}
	if !f.exists(page1) || !f.exists(view2) || !f.exists(unrecorded) || !f.exists(earlier) {
		t.Fatalf("takedown took more than it dropped: shared blob kept %v, another upload's editor view %v, an unrecorded output %v, an earlier version %v",
			f.exists(page1), f.exists(view2), f.exists(unrecorded), f.exists(earlier))
	}
	if jobs := f.q.take(); len(jobs) != 1 {
		t.Fatalf("the zip is not built again: %+v", jobs)
	}
	// On a path already gone it changes and queues nothing.
	_, before, _ := f.ms.Get(ctx, g)
	if _, err := f.up.Commit(ctx, f.editor, g, []media.Op{{Op: media.OpRemove, Path: "originals/000.png", Takedown: true}}); code(err) != media.CodeNotFound {
		t.Fatalf("a takedown of a gone path: %v", err)
	}
	if _, after, _ := f.ms.Get(ctx, g); before != after || len(f.q.take()) != 0 || !f.exists(view2) || !f.exists(unrecorded) {
		t.Fatal("a takedown of a gone path changed or queued something")
	}
	// An exempt grant sweeps the item, on a gone path too.
	if _, err := f.up.Commit(ctx, staff, g, []media.Op{{Op: media.OpRemove, Path: "originals/000.png", Takedown: true}}); err != nil {
		t.Fatal(err)
	}
	if f.exists(view2) || f.exists(unrecorded) || f.exists(earlier) || !f.exists(page1) {
		t.Fatalf("an exempt takedown: editor view kept %v, unrecorded output %v, earlier version %v; referenced blob kept %v",
			f.exists(view2), f.exists(unrecorded), f.exists(earlier), f.exists(page1))
	}
	m, _, _ = f.ms.Get(ctx, g)
	for _, b := range m.Blobs() {
		if key, _ := item.Blob(b); !f.exists(key) {
			t.Fatalf("took referenced %s", key)
		}
	}
	for len(f.purged) > 0 {
		<-f.purged
	}
	f.commit(g, media.Op{Op: media.OpRemove, Path: "cover.png", Takedown: true})
	for _, w := range []int{230, 460} {
		if key, _ := item.Public(fmt.Sprintf("cover-%d.webp", w)); f.exists(key) {
			t.Fatalf("taken-down cover %d kept", w)
		}
	}
	if urls := f.purges(); len(urls) != 2 {
		t.Fatalf("purged %v", urls)
	}
	if _, err := f.up.Commit(ctx, f.editor, g, []media.Op{{Op: media.OpEdit, Path: "originals/002.png", Takedown: true}}); code(err) != media.CodeInvalid {
		t.Fatalf("takedown on an edit: %v", err)
	}
	// A staged upload taken down before it is placed.
	p, staged := f.upload(g, "originals/004.png", "image/png", png(4))
	if _, err := f.up.Commit(ctx, f.editor, g, []media.Op{{Op: media.OpPut, Path: p, Blob: staged}}); err != nil {
		t.Fatal(err)
	}
	f.commit(g, media.Op{Op: media.OpRemove, Path: p, Takedown: true})
	if key, _ := item.Staged(staged); f.exists(key) {
		t.Fatal("a taken-down staged upload kept")
	}
}

// A video's takedown takes the frames grabbed from it, their blobs and
// their public files.
func TestTakedownFrames(t *testing.T) {
	f := newFixture(t)
	f.visible(1)
	v := f.ref("video", 1)
	item, _ := f.reg.Item(v)
	ctx := context.Background()
	f.put(v, "source.mp4", "video/mp4", []byte("video one"))
	f.commit(v, media.Op{Op: media.OpFrame, Path: "poster", T: ptr(3.5)})
	frame, _ := item.Blob(blobOf([]byte("frame")))
	f.object(frame, "frame")
	public, _ := item.Public("poster-640.webp")
	f.object(public, "poster")
	if _, err := f.ms.EditExisting(ctx, v, func(m *media.Manifest) error {
		i := m.Find("poster.png")
		m.Files[i].Blob, m.Files[i].Size, m.Files[i].Frame.Of, m.Files[i].Pending = blobOf([]byte("frame")), 5, blobOf([]byte("video one")), nil
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	m := f.commit(v, media.Op{Op: media.OpRemove, Path: "source.mp4", Takedown: true})
	if len(m.Files) != 0 || f.exists(frame) || f.exists(public) {
		t.Fatalf("a grabbed frame survived its video: %v, blob %v, public %v", paths(m), f.exists(frame), f.exists(public))
	}
	// An uploaded poster is not of the video: it stays.
	f.put(v, "source.mp4", "video/mp4", []byte("video two"))
	f.put(v, "poster.png", "image/png", png(1))
	if m := f.commit(v, media.Op{Op: media.OpRemove, Path: "source.mp4", Takedown: true}); m.Find("poster.png") < 0 {
		t.Fatalf("an uploaded poster went with the video: %v", paths(m))
	}
}

// failingStore fails deletions under private/ while armed.
type failingStore struct {
	media.Store
	armed *atomic.Bool
}

func (s failingStore) Delete(ctx context.Context, key string) error {
	if s.armed.Load() && strings.Contains(key, "/private/") {
		return errors.New("delete failed")
	}
	return s.Store.Delete(ctx, key)
}

// A takedown whose deletes fail says so, with the manifest already
// committed and processing queued. An exempt grant completes it by sending
// it again, the path now gone; for anyone else the leftovers go at the
// sweep.
func TestTakedownRetry(t *testing.T) {
	f := newFixture(t)
	f.visible(1)
	g := f.gallery(1, 2)
	item, _ := f.reg.Item(g)
	ctx := context.Background()
	store := failingStore{Store: f.env.Store, armed: &atomic.Bool{}}
	up, err := media.NewUploads(media.UploadOptions{Store: store, Manifests: s3test.Manifests(t, store, f.reg, media.ManifestOptions{}), Queue: f.q})
	if err != nil {
		t.Fatal(err)
	}
	page0, _ := item.Blob(blobOf(png(100)))
	takedown := []media.Op{{Op: media.OpRemove, Path: "originals/000.png", Takedown: true}}
	f.q.take()
	store.armed.Store(true)
	if _, err := up.Commit(ctx, f.editor, g, takedown); err == nil || !f.exists(page0) {
		t.Fatalf("a failed delete was not reported: %v", err)
	}
	if m, _, _ := f.ms.Get(ctx, g); m.Find("originals/000.png") >= 0 || len(f.q.take()) != 1 {
		t.Fatal("the removal was not committed and queued before the deletes")
	}
	store.armed.Store(false)
	if _, err := up.Commit(ctx, f.editor, g, takedown); code(err) != media.CodeNotFound || !f.exists(page0) {
		t.Fatalf("a retry without an exempt grant: %v, blob kept %v", err, f.exists(page0))
	}
	if _, err := up.Commit(ctx, access.Actor{ID: "staff", Kind: "user"}, g, takedown); err != nil || f.exists(page0) {
		t.Fatalf("the exempt retry: %v, blob kept %v", err, f.exists(page0))
	}
}

// takedownStore runs a takedown of the destination right after a copy's
// blobs land, before the copy's manifest edit.
type takedownStore struct {
	media.Store
	after func()
}

func (s takedownStore) Copy(ctx context.Context, src, dst string, o media.CopyOptions) (media.Object, error) {
	obj, err := s.Store.Copy(ctx, src, dst, o)
	if err == nil {
		s.after()
	}
	return obj, err
}

// A copy never references a blob a takedown took meanwhile: it conflicts,
// and the retry copies again.
func TestCopyDuringTakedown(t *testing.T) {
	f := newFixture(t)
	f.visible(1)
	f.visible(2)
	f.gallery(1, 1)
	b := f.gallery(2, 1)
	item, _ := f.reg.Item(b)
	ctx := context.Background()
	var once sync.Once
	store := takedownStore{Store: f.env.Store, after: func() {
		once.Do(func() {
			// An exempt takedown sweeps every blob the item does not reference.
			if _, err := f.up.Commit(ctx, access.Actor{ID: "staff", Kind: "user"}, b, []media.Op{{Op: media.OpRemove, Path: "originals/gone.png", Takedown: true}}); err != nil {
				t.Error(err)
			}
		})
	}}
	up, err := media.NewUploads(media.UploadOptions{Store: store, Manifests: f.ms, Queue: f.q})
	if err != nil {
		t.Fatal(err)
	}
	copyOp := []media.Op{{Op: media.OpCopy, From: &media.CopyFrom{ID: cid(1), Path: "originals/000.png"}, To: "originals/copy.png"}}
	if _, err := up.Commit(ctx, f.editor, b, copyOp); code(err) != media.CodeConflict {
		t.Fatalf("a copy over a takedown: %v", err)
	}
	if m, _, _ := f.ms.Get(ctx, b); m.Find("originals/copy.png") >= 0 {
		t.Fatal("the copy references a blob that was taken")
	}
	m, err := up.Commit(ctx, f.editor, b, copyOp)
	if err != nil {
		t.Fatal(err)
	}
	for _, blob := range m.Blobs() {
		if key, _ := item.Blob(blob); !f.exists(key) {
			t.Fatalf("the retried copy references missing %s", key)
		}
	}
}

// Expose hides an item (public/ deleted and purged at once, nothing public
// pending) and unhides it (public presets pending, the worker asked).
func TestExpose(t *testing.T) {
	f := newFixture(t)
	f.visible(1)
	g := f.gallery(1, 1)
	item, _ := f.reg.Item(g)
	ctx := context.Background()
	cover, _ := item.Public("cover-460.webp")
	if !f.exists(cover) {
		t.Fatal("no public cover")
	}
	if status, body, _ := f.fetch(f.reg.PublicURL(g, "cover-460.webp")); status != 200 || body != string(png(199)) {
		t.Fatalf("public cover %d %q", status, body)
	}
	f.res.set(cid(1), access.Resolution{})
	if err := f.jobs.Expose(ctx, g); err != nil {
		t.Fatal(err)
	}
	m, _, _ := f.ms.Get(ctx, g)
	if !m.Hidden || f.exists(cover) {
		t.Fatalf("hidden %v, cover kept %v", m.Hidden, f.exists(cover))
	}
	if urls := f.purges(); len(urls) != 2 {
		t.Fatalf("purged %v", urls)
	}
	f.q.take()
	f.visible(1)
	if err := f.jobs.Expose(ctx, g); err != nil {
		t.Fatal(err)
	}
	m, _, _ = f.ms.Get(ctx, g)
	if c, _ := m.Get("cover.png"); m.Hidden || !slices.Equal(c.Pending, []string{"cover"}) {
		t.Fatalf("unhidden %v pending %v", m.Hidden, c.Pending)
	}
	if jobs := f.q.take(); len(jobs) != 1 || jobs[0].Ref != g {
		t.Fatalf("render asked %+v", jobs)
	}
	if err := f.jobs.Expose(ctx, f.ref("gallery", 2)); err != nil {
		t.Fatalf("an item without a manifest: %v", err)
	}
}

// A new item exposes nothing public before its first visibility decision
// (audit): Create writes a hidden manifest, and the first commit resolves it
// anonymously, so a draft's cover renders nothing public until Expose
// unhides it; a visible item's first commit unhides it.
func TestNewItemStartsHidden(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	g := f.ref("gallery", 1)
	item, _ := f.reg.Item(g)
	cover, _ := item.Public("cover-460.webp")
	f.res.set(cid(1), access.Resolution{}) // a draft
	if m, err := f.ms.Create(ctx, g); err != nil || !m.Hidden {
		t.Fatalf("created %+v %v", m, err)
	}
	m := f.put(g, "cover.png", "image/png", png(1))
	if c, _ := m.Get("cover.png"); !m.Hidden || c.Pending != nil {
		t.Fatalf("draft's first commit: hidden %v pending %v", m.Hidden, c.Pending)
	}
	f.produce(g)
	if f.exists(cover) {
		t.Fatal("a draft's cover is public")
	}
	f.visible(1)
	if err := f.jobs.Expose(ctx, g); err != nil {
		t.Fatal(err)
	}
	f.produce(g)
	if !f.exists(cover) {
		t.Fatal("the published item's cover is not public")
	}

	v := f.ref("gallery", 2)
	f.visible(2)
	if _, err := f.ms.Create(ctx, v); err != nil {
		t.Fatal(err)
	}
	if m := f.put(v, "cover.png", "image/png", png(2)); m.Hidden {
		t.Fatal("a visible item's first commit kept it hidden")
	}
}

// Purge deletes an item's folder now; Regenerate visits every item of a
// kind; SweepOrphans finds folders the host no longer has.
func TestPurgeRegenerateOrphans(t *testing.T) {
	f := newFixture(t)
	f.visible(1)
	f.visible(2)
	a, b := f.gallery(1, 1), f.gallery(2, 1)
	ctx := context.Background()
	f.q.take()
	if n, err := f.jobs.Regenerate(ctx, "gallery", "thumb"); err != nil || n != 2 {
		t.Fatalf("regenerate %d %v", n, err)
	}
	if jobs := f.q.take(); len(jobs) != 2 || jobs[0].Preset != "thumb" {
		t.Fatalf("regenerate jobs %+v", jobs)
	}
	rep, err := f.later().SweepOrphans(ctx, media.OrphanSweep{Kind: "gallery", Delete: true,
		Exists: func(_ context.Context, ids []string) (map[string]bool, error) {
			return map[string]bool{cid(1): true}, nil
		}})
	if err != nil || rep.Folders != 2 || len(rep.Orphans) != 1 || rep.Orphans[0].ID != cid(2) || !rep.Orphans[0].Deleted {
		t.Fatalf("orphans %+v %v", rep, err)
	}
	if _, _, err := f.ms.Get(ctx, b); !errors.Is(err, media.ErrNotFound) {
		t.Fatalf("orphan kept: %v", err)
	}
	f.purges() // the orphan's public cover
	if err := f.jobs.Purge(ctx, media.Deletion{Ref: a}); err != nil {
		t.Fatal(err)
	}
	item, _ := f.reg.Item(a)
	for o, err := range f.env.Store.List(ctx, item.Prefix()) {
		t.Fatalf("purge kept %s %v", o.Key, err)
	}
	if urls := f.purges(); len(urls) != 2 {
		t.Fatalf("purged %v", urls)
	}
}

// riverHost composes media's jobs into a host River client on a fresh
// schema and starts it.
func riverHost(t *testing.T, jobs *media.Jobs, pool *pgxpool.Pool) string {
	t.Helper()
	ctx := context.Background()
	schema := pgtest.EmptySchema(t, ctx, pool)
	if err := riverhelpers.ApplyMigrations(ctx, pool, schema); err != nil {
		t.Fatal(err)
	}
	cfg := &river.Config{Schema: schema, FetchPollInterval: 100 * time.Millisecond, FetchCooldown: 50 * time.Millisecond}
	client, err := riverhelpers.New(ctx, pool, cfg, jobs.RiverJobs())
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = client.StopAndCancel(ctx)
	})
	return schema
}

// The worker's relays reach the host's hooks through its media queue:
// ItemReady in a host transaction once the item settles, PurgePublic with
// URLs; the host deletes items from its own transaction.
func TestHostRelays(t *testing.T) {
	env := s3test.Open(t)
	pool := pgtest.Pool(t, nil)
	var mu sync.Mutex
	var ready []media.Readiness
	f := newFixtureOn(t, env, func(c *media.Config) {
		c.Hooks.ItemReady = func(ctx context.Context, tx pgx.Tx, ref contentref.ContentRef, r media.Readiness) error {
			var one int
			if err := tx.QueryRow(ctx, "SELECT 1").Scan(&one); err != nil {
				return err
			}
			mu.Lock()
			ready = append(ready, r)
			mu.Unlock()
			return nil
		}
	})
	jobs, err := media.NewJobs(media.JobsConfig{Store: env.Store, Registry: f.reg, Locker: s3test.Locker(t, env.Store), Pool: pool, Processes: f.q})
	if err != nil {
		t.Fatal(err)
	}
	schema := riverHost(t, jobs, pool)
	host, err := media.NewHostQueue(pool, f.reg, schema, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	f.visible(1)
	g := f.ref("gallery", 1)
	f.put(g, "originals/1.png", "image/png", png(1))
	ctx := context.Background()
	if err := host.Ready(ctx, g); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second)
	mu.Lock()
	if len(ready) != 0 {
		t.Fatalf("a processing item was reported ready: %+v", ready)
	}
	mu.Unlock()
	f.produce(g)
	if err := host.Ready(ctx, g); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "ItemReady", func() bool { mu.Lock(); defer mu.Unlock(); return len(ready) == 1 && ready[0].Ready() })
	item, _ := f.reg.Item(g)
	key, _ := item.Public("cover-460.webp")
	if err := host.Purge(ctx, []string{key}); err != nil {
		t.Fatal(err)
	}
	if urls := f.purges(); len(urls) != 1 || urls[0] != f.reg.PublicURL(g, "cover-460.webp") {
		t.Fatalf("purge relay %v", urls)
	}
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error { return jobs.DeleteItemsTx(ctx, tx, media.Deletion{Ref: g}) }); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "folder deletion", func() bool {
		_, _, err := f.ms.Get(ctx, g)
		return errors.Is(err, media.ErrNotFound)
	})
}
