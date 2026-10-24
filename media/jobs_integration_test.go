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

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	riverhelpers "github.com/open-rails/helpers/river"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/internal/resolve"
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
	j, err := media.NewJobs(media.JobsConfig{Store: f.env.Store, Registry: f.reg, Locker: s3test.Locker(f.t, f.env.Store), Journal: f.journal,
		Processes: f.q, Now: func() time.Time { return time.Now().Add(10 * 24 * time.Hour) }})
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

// Replacements retire the old allocation as part of the edit, regardless of
// its age. Neither the old grace period nor a later sweep is needed.
func TestReplacementRetiresImmediately(t *testing.T) {
	f := newFixture(t)
	f.visible(1)
	g := f.gallery(1, 1)
	item, _ := f.reg.Item(g)
	old, _ := item.Blob(f.fileBlob(g, "originals/000.png"))
	f.put(g, "originals/000.png", "image/png", png(7))
	f.produce(g)
	if f.exists(old) {
		t.Fatal("replaced source survived its manifest edit")
	}
	m, _, err := f.ms.Get(t.Context(), g)
	if err != nil {
		t.Fatal(err)
	}
	for _, blob := range m.Blobs() {
		key, _ := item.Blob(blob)
		if !f.exists(key) {
			t.Fatalf("current source was retired: %s", key)
		}
	}
}

// A takedown leaves nothing of the upload to fetch: the blobs the commit
// dropped (its own, its outputs', the zip that bundled it), its editor view
// and its public names go at once . It touches nothing else: another
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
	earlier, _ := item.Blob(f.fileBlob(g, "originals/000.png"))
	f.put(g, "originals/000.png", "image/png", png(7))   // page 0's first version is unreferenced
	f.put(g, "originals/003.png", "image/png", png(101)) // the same bytes as page 1
	f.produce(g)
	m, _, _ := f.ms.Get(ctx, g)
	view := func(path string) string {
		key, _ := item.Blob(f.editorView(g, path))
		return key
	}
	page0, _ := item.Blob(f.fileBlob(g, "originals/000.png"))
	page1, _ := item.Blob(f.fileBlob(g, "originals/001.png"))
	view0, view2 := view("originals/000.png"), view("originals/002.png")
	unrecorded, _ := item.Blob(f.blob(g, []byte("an output a job has not recorded yet"), "image/webp"))
	f.q.take()
	m = f.commit(g, media.Op{Op: media.OpRemove, Path: "originals/000.png", Takedown: true},
		media.Op{Op: media.OpRemove, Path: "originals/001.png", Takedown: true})
	if f.exists(page0) || f.exists(view0) || m.Find("download/pages.zip") >= 0 {
		t.Fatalf("takedown: page 0 kept %v, its editor view %v; %v", f.exists(page0), f.exists(view0), paths(m))
	}
	if !f.exists(page1) || !f.exists(view2) || !f.exists(unrecorded) || f.exists(earlier) {
		t.Fatalf("wrong retention: shared blob kept %v, another upload's editor view %v, an active allocation %v, retired earlier source %v",
			f.exists(page1), f.exists(view2), f.exists(unrecorded), f.exists(earlier))
	}
	if jobs := f.q.take(); len(jobs) != 1 {
		t.Fatalf("the zip is not built again: %+v", jobs)
	}
	// On a path already gone it changes and queues nothing.
	_, before, _ := f.ms.Get(ctx, g)
	if _, err := f.up.Commit(ctx, f.editor, g, uuid.NewString(), []media.Op{{Op: media.OpRemove, Path: "originals/000.png", Takedown: true}}); code(err) != media.CodeNotFound {
		t.Fatalf("a takedown of a gone path: %v", err)
	}
	if _, after, _ := f.ms.Get(ctx, g); before != after || len(f.q.take()) != 0 || !f.exists(view2) || !f.exists(unrecorded) {
		t.Fatal("a takedown of a gone path changed or queued something")
	}
	// An exempt grant sweeps the item, on a gone path too.
	if _, err := f.up.Commit(ctx, staff, g, uuid.NewString(), []media.Op{{Op: media.OpRemove, Path: "originals/000.png", Takedown: true}}); err != nil {
		t.Fatal(err)
	}
	if !f.exists(view2) || f.exists(unrecorded) || f.exists(earlier) || !f.exists(page1) {
		t.Fatalf("an exempt takedown: editor view kept %v, unrecorded output %v, earlier version %v; referenced blob kept %v",
			f.exists(view2), f.exists(unrecorded), f.exists(earlier), f.exists(page1))
	}
	m, _, _ = f.ms.Get(ctx, g)
	for _, b := range m.Blobs() {
		if key, _ := item.Blob(b); !f.exists(key) {
			t.Fatalf("took referenced %s", key)
		}
	}
	f.commit(g, media.Op{Op: media.OpRemove, Path: "cover.png", Takedown: true})
	for _, w := range []int{230, 460} {
		if key, _ := item.Public(fmt.Sprintf("cover-%d.webp", w)); f.exists(key) {
			t.Fatalf("taken-down cover %d kept", w)
		}
	}
	if _, err := f.up.Commit(ctx, f.editor, g, uuid.NewString(), []media.Op{{Op: media.OpEdit, Path: "originals/002.png", Takedown: true}}); code(err) != media.CodeInvalid {
		t.Fatalf("takedown on an edit: %v", err)
	}
	// A staged upload taken down before it is placed.
	p, staged := f.upload(g, "originals/004.png", "image/png", png(4))
	if _, err := f.up.Commit(ctx, f.editor, g, uuid.NewString(), []media.Op{{Op: media.OpPut, Path: p, Blob: staged}}); err != nil {
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
	frameBlob := f.blob(v, []byte("frame"), "image/png")
	frame, _ := item.Blob(frameBlob)
	if _, err := f.ms.EditExisting(ctx, v, func(m *media.Manifest) error {
		i := m.Find("poster.png")
		source, _ := m.Get("source.mp4")
		m.Files[i].Blob, m.Files[i].Size, m.Files[i].Frame.Of, m.Files[i].Pending = frameBlob, 5, source.Blob, nil
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	f.produce(v)
	public, _ := item.Public(f.publicName(v, "poster-640.webp"))
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
	manifests := s3test.Manifests(t, store, f.reg, media.ManifestOptions{Journal: f.journal})
	up, err := media.NewUploads(media.UploadOptions{Store: store, Manifests: manifests})
	if err != nil {
		t.Fatal(err)
	}
	page0, _ := item.Blob(f.fileBlob(g, "originals/000.png"))
	takedown := []media.Op{{Op: media.OpRemove, Path: "originals/000.png", Takedown: true}}
	f.q.take()
	store.armed.Store(true)
	if _, err := up.Commit(ctx, f.editor, g, uuid.NewString(), takedown); err == nil || !f.exists(page0) {
		t.Fatalf("a failed delete was not reported: %v", err)
	}
	if m, _, _ := f.ms.Get(ctx, g); m.Find("originals/000.png") >= 0 || len(f.q.take()) != 0 {
		t.Fatal("the removal was not committed, or cleanup failure allowed queue settlement")
	}
	store.armed.Store(false)
	for range 2 {
		if err := manifests.Recover(ctx, g); err != nil {
			t.Fatal(err)
		}
	}
	if jobs := f.q.take(); len(jobs) != 1 || f.exists(page0) {
		t.Fatalf("recovery did not finish cleanup and queue exactly once: %+v", jobs)
	}
	if _, err := up.Commit(ctx, f.editor, g, uuid.NewString(), takedown); code(err) != media.CodeNotFound || f.exists(page0) {
		t.Fatalf("a retry without an exempt grant: %v, blob kept %v", err, f.exists(page0))
	}
	if _, err := up.Commit(ctx, access.Actor{ID: "staff", Kind: "user"}, g, uuid.NewString(), takedown); err != nil || f.exists(page0) {
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
			if _, err := f.up.Commit(ctx, access.Actor{ID: "staff", Kind: "user"}, b, uuid.NewString(), []media.Op{{Op: media.OpRemove, Path: "originals/gone.png", Takedown: true}}); err != nil {
				t.Error(err)
			}
		})
	}}
	up, err := media.NewUploads(media.UploadOptions{Store: store, Manifests: f.ms})
	if err != nil {
		t.Fatal(err)
	}
	copyOp := []media.Op{{Op: media.OpCopy, From: &media.CopyFrom{ID: cid(1), Path: "originals/000.png"}, To: "originals/copy.png"}}
	if _, err := up.Commit(ctx, f.editor, b, uuid.NewString(), copyOp); code(err) != media.CodeConflict {
		t.Fatalf("a copy over a takedown: %v", err)
	}
	if m, _, _ := f.ms.Get(ctx, b); m.Find("originals/copy.png") >= 0 {
		t.Fatal("the copy references a blob that was taken")
	}
	m, err := up.Commit(ctx, f.editor, b, uuid.NewString(), copyOp)
	if err != nil {
		t.Fatal(err)
	}
	for _, blob := range m.Blobs() {
		if key, _ := item.Blob(blob); !f.exists(key) {
			t.Fatalf("the retried copy references missing %s", key)
		}
	}
}

// A preview name the manifest stops vouching for goes with the commit, not
// when the queued job runs: a page moved out of the first N, a position
// another page took, a page cropped again or replaced.
func TestPreviewNamesGoWithTheCommit(t *testing.T) {
	f := newFixtureOn(t, s3test.Open(t), func(c *media.Config) {
		c.Kinds[0].Public = append(c.Kinds[0].Public, media.Public{Name: "preview", From: "originals/{name}", To: "preview-{n}.webp", First: 2})
	})
	f.visible(1)
	g := f.gallery(1, 3)
	item, _ := f.reg.Item(g)
	have := func() string {
		t.Helper()
		var out []string
		m, _, err := f.ms.Get(t.Context(), g)
		if err != nil {
			t.Fatal(err)
		}
		owned := item.Kind().PublicKept(m)
		for n := 1; n <= 3; n++ {
			prefix := fmt.Sprintf("preview-%d-", n)
			for obj, err := range f.env.Store.List(t.Context(), item.PublicPrefix()) {
				if err != nil {
					t.Fatal(err)
				}
				name := strings.TrimPrefix(obj.Key, item.PublicPrefix())
				if strings.HasPrefix(name, prefix) {
					if !slices.Contains(owned, name) {
						t.Fatalf("an unowned preview survived the commit: %s", obj.Key)
					}
					out = append(out, fmt.Sprint(n))
				}
			}
		}
		return strings.Join(out, ",")
	}
	if got := have(); got != "1,2" {
		t.Fatalf("previews rendered: %q", got)
	}
	// The third page moves first: positions 1 and 2 hold other pages' images.
	f.commit(g, media.Op{Op: media.OpMove, Path: "originals/002.png", Index: new(int)})
	if got := have(); got != "" {
		t.Fatalf("after a move the old images are still public at %q", got)
	}
	f.produce(g)
	// A crop of the page at position 2 (page 0 now): only its image goes.
	f.commit(g, media.Op{Op: media.OpEdit, Path: "originals/000.png", Edit: &media.Edit{Rotate: 90}})
	if got := have(); got != "1" {
		t.Fatalf("after an edit: %q", got)
	}
	f.produce(g)
	// A page moved out of the first two takes nothing with it but its own.
	last := 2
	f.commit(g, media.Op{Op: media.OpMove, Path: "originals/000.png", Index: &last})
	if got := have(); got != "1" {
		t.Fatalf("after moving the second page out: %q", got)
	}
	// A commit that changes no preview deletes nothing.
	f.produce(g)
	f.commit(g, media.Op{Op: media.OpMeta, Meta: map[string]any{"title": "T"}})
	if got := have(); got != "1,2" {
		t.Fatalf("after a meta edit: %q", got)
	}
}

// Publication changes leave declared public objects and processing untouched.
func TestExposureIsStorageIndependent(t *testing.T) {
	f := newFixture(t)
	f.visible(1)
	g := f.gallery(1, 1)
	item, _ := f.reg.Item(g)
	ctx := context.Background()
	name := f.publicName(g, "cover-460.webp")
	cover, _ := item.Public(name)
	_, before, err := f.ms.Get(ctx, g)
	if err != nil {
		t.Fatal(err)
	}
	f.q.take()
	for _, visible := range []bool{false, true, false} {
		f.res.set(cid(1), resolve.Resolution{Visible: visible})
		_, after, err := f.ms.Get(ctx, g)
		if err != nil || after != before || !f.exists(cover) {
			t.Fatalf("visibility changed media: etag=%s/%s, cover=%v, error=%v", before, after, f.exists(cover), err)
		}
		if queued := f.q.take(); len(queued) != 0 {
			t.Fatalf("visibility queued processing: %+v", queued)
		}
		if status, _, _ := f.fetch(f.reg.PublicURL(g, name)); status != 200 {
			t.Fatalf("public cover status=%d", status)
		}
	}
}

func TestDraftPublicPresetsRenderNormally(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	g := f.ref("gallery", 1)
	f.res.set(cid(1), resolve.Resolution{})
	if _, err := f.ms.Create(ctx, g); err != nil {
		t.Fatal(err)
	}
	m := f.put(g, "cover.png", "image/png", png(1))
	if c, _ := m.Get("cover.png"); !slices.Contains(c.Pending, "cover") {
		t.Fatalf("draft cover not pending: %+v", c)
	}
	f.produce(g)
	item, _ := f.reg.Item(g)
	cover, _ := item.Public(f.publicName(g, "cover-460.webp"))
	if !f.exists(cover) {
		t.Fatal("draft's declared public cover missing")
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
	if err := f.jobs.Purge(ctx, media.Deletion{Ref: a, OperationID: "purge-a"}); err != nil {
		t.Fatal(err)
	}
	item, _ := f.reg.Item(a)
	for o, err := range f.env.Store.List(ctx, item.Prefix()) {
		if err != nil || o.Key != item.ManifestKey() {
			t.Fatalf("purge kept %s %v", o.Key, err)
		}
	}
}

// hostSchema provides an empty host queue schema. Media registers no host workers.
func hostSchema(t *testing.T, jobs *media.Jobs, pool *pgxpool.Pool) string {
	t.Helper()
	schema := pgtest.EmptySchema(t, t.Context(), pool)
	if err := riverhelpers.ApplyMigrations(t.Context(), pool, schema); err != nil {
		t.Fatal(err)
	}
	return schema
}

// Both first-pass intent and exact late-write receipt preserve a new incarnation.
func TestPurgeRetirementCannotEraseResetItem(t *testing.T) {
	for _, first := range []bool{false, true} {
		t.Run(fmt.Sprint(first), func(t *testing.T) {
			f := newFixture(t)
			f.visible(1)
			ref := f.gallery(1, 1)
			item, _ := f.reg.Item(ref)
			oldKey, _ := item.Blob(f.fileBlob(ref, "originals/000.png"))
			if err := pgx.BeginFunc(t.Context(), f.env.Pool(), func(tx pgx.Tx) error { return f.jobs.RetireItemsTx(t.Context(), tx, media.Deletion{Ref: ref}) }); err != nil {
				t.Fatal(err)
			}
			if first {
				if _, err := f.jobs.PurgeRetired(t.Context(), 100); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.jobs.Purge(t.Context(), media.Deletion{Ref: ref, OperationID: "reset-after-retirement"}); err != nil {
				t.Fatal(err)
			}
			f.put(ref, "originals/new.png", "image/png", png(1))
			newKey, _ := item.Blob(f.fileBlob(ref, "originals/new.png"))
			f.object(oldKey, string(png(1)))
			if _, err := f.later().PurgeRetired(t.Context(), 100); err != nil {
				t.Fatal(err)
			}
			if !f.exists(newKey) || f.fileBlob(ref, "originals/new.png") == "" {
				t.Fatal("old retirement erased the new lifetime")
			}
			if f.exists(oldKey) {
				t.Fatal("late retired write survived content purge")
			}
		})
	}
}

// The accepted exact mutation and ingest admission precede the S3 PUT.
func TestIngestOwnsInterruptedManifest(t *testing.T) {
	for _, landed := range []bool{false, true} {
		t.Run(fmt.Sprint(landed), func(t *testing.T) {
			f := newFixture(t)
			f.visible(1)
			ref := f.ref("gallery", 1)
			item, _ := f.reg.Item(ref)
			store := &uncertainManifestStore{Store: f.env.Store, key: item.ManifestKey(), uncertain: true, applied: landed}
			ms := s3test.Manifests(t, store, f.reg, media.ManifestOptions{Journal: f.journal})
			if _, err := ms.Edit(t.Context(), ref, func(m *media.Manifest) error { m.Meta = map[string]any{"title": "admitted"}; return nil }); !errors.Is(err, media.ErrUnavailable) {
				t.Fatalf("lost response: %v", err)
			}
			var operation string
			var jobs int
			commits := pgx.Identifier{f.env.ContentSchema(), "content_media_commits"}.Sanitize()
			if err := f.env.Pool().QueryRow(t.Context(), `SELECT operation_id::text FROM `+commits+` WHERE tenant_id=$1 AND folder_prefix=$2 AND state='prepared'`, ref.TenantID, item.Prefix()).Scan(&operation); err != nil {
				t.Fatal(err)
			}
			if err := f.env.Pool().QueryRow(t.Context(), `SELECT count(*) FROM `+pgx.Identifier{f.q.Schema(), "river_job"}.Sanitize()+` WHERE kind='contentkit_media_ingest' AND args->>'commit'=$1 AND state='available'`, operation).Scan(&jobs); err != nil || jobs != 1 {
				t.Fatalf("mutation has %d runnable owners: %v", jobs, err)
			}
			status, err := f.rd.Status(t.Context(), ref, f.editor)
			if err != nil || status.State != media.StateProcessing {
				t.Fatalf("admitted status: %+v %v", status, err)
			}
			for range 2 {
				if err := ms.CompleteCommit(t.Context(), ref, operation); err != nil {
					t.Fatal(err)
				}
			}
			m, _, err := ms.Get(t.Context(), ref)
			if err != nil || m.Meta["title"] != "admitted" {
				t.Fatalf("retry lost exact mutation: %+v %v", m, err)
			}
			if _, err := f.env.Store.Put(t.Context(), item.ManifestKey(), strings.NewReader(string(store.body)), int64(len(store.body)), store.options); !errors.Is(err, media.ErrPreconditionFailed) {
				t.Fatalf("late original write was not fenced: %v", err)
			}
		})
	}
}

// The worker's relays reach the host's hooks through its media queue:
// ItemReady in a host transaction once the item settles; the host deletes
// items from its own transaction.
func TestProcessorCompletionTransaction(t *testing.T) {
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
	jobs, err := media.NewJobs(media.JobsConfig{Store: env.Store, Registry: f.reg, Locker: s3test.Locker(t, env.Store), Journal: env.Journal(), Processes: f.q})
	if err != nil {
		t.Fatal(err)
	}
	hostSchema(t, jobs, pool)
	complete := func(ctx context.Context, ref contentref.ContentRef) error {
		return media.CompleteProcessing(ctx, f.ms, ref, func(ctx context.Context, tx pgx.Tx, r media.Readiness) error {
			hooks := f.reg.Config().Hooks
			if r.State == media.StateProcessing {
				if hooks.ItemProgressed != nil {
					return hooks.ItemProgressed(ctx, tx, ref)
				}
				return nil
			}
			return hooks.ItemReady(ctx, tx, ref, r)
		})
	}
	f.visible(1)
	g := f.ref("gallery", 1)
	f.put(g, "originals/1.png", "image/png", png(1))
	ctx := context.Background()
	if err := complete(ctx, g); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second)
	mu.Lock()
	if len(ready) != 0 {
		t.Fatalf("a processing item was reported ready: %+v", ready)
	}
	mu.Unlock()
	f.produce(g)
	if err := complete(ctx, g); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "ItemReady", func() bool { mu.Lock(); defer mu.Unlock(); return len(ready) == 1 && ready[0].Ready() })
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error { return jobs.RetireItemsTx(ctx, tx, media.Deletion{Ref: g}) }); err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.PurgeRetired(ctx, 100); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "folder deletion", func() bool {
		_, _, err := f.ms.Get(ctx, g)
		return errors.Is(err, media.ErrNotFound)
	})
}

// A content's versions are folders within its own, which holds no item;
// every pass visits each version as its own item and takes nothing of the
// others.
func TestVersionFolderJobs(t *testing.T) {
	f := newFixtureOn(t, s3test.Open(t), func(c *media.Config) {
		c.Kinds = append(c.Kinds, media.Kind{Name: "book", Versions: true, KeepOriginals: true, Uploads: []media.Upload{
			{Path: "cover", Types: images, MaxBytes: 1 << 20}, {Path: "pages/{name}", Types: images, MaxBytes: 1 << 20}}})
	})
	f.visible(1)
	ctx := context.Background()
	if _, err := f.reg.VersionRef("book", cid(1), ""); err == nil {
		t.Fatal("a content kind's content has an item of its own")
	}
	refs := []contentref.ContentRef{}
	for _, v := range []string{cid(11), cid(12)} {
		ref, err := f.reg.VersionRef("book", cid(1), v)
		if err != nil {
			t.Fatal(err)
		}
		refs = append(refs, ref)
	}
	f.put(refs[0], "cover.png", "image/png", png(1))
	f.put(refs[0], "pages/a.png", "image/png", png(2))
	f.put(refs[1], "pages/a.png", "image/png", png(3))
	var strays, kept []string
	for i, ref := range refs {
		item, _ := f.reg.Item(ref)
		allocation := f.blob(ref, []byte(fmt.Sprintf("abandoned %d", i)), "image/png")
		stray, _ := item.Blob(allocation)
		strays = append(strays, stray)
		m, _, _ := f.ms.Get(ctx, ref)
		for _, b := range m.Blobs() {
			key, _ := item.Blob(b)
			kept = append(kept, key)
		}
	}
	if err := f.later().SweepAll(ctx); err != nil {
		t.Fatal(err)
	}
	for _, k := range strays {
		if f.exists(k) {
			t.Fatalf("stray %s kept", k)
		}
	}
	for _, k := range kept {
		if !f.exists(k) {
			t.Fatalf("swept %s", k)
		}
	}
	f.q.take()
	if n, err := f.jobs.Regenerate(ctx, "book", ""); err != nil || n != 2 {
		t.Fatalf("regenerate visited %d: %v", n, err)
	}
	rep, err := f.later().SweepOrphans(ctx, media.OrphanSweep{Kind: "book", Delete: true,
		Exists: func(_ context.Context, ids []string) (map[string]bool, error) {
			return map[string]bool{cid(1): true, cid(11): true}, nil
		}})
	if err != nil || rep.Folders != 2 || len(rep.Orphans) != 1 || rep.Orphans[0].ID != cid(12) || !rep.Orphans[0].Deleted {
		t.Fatalf("orphans %+v %v", rep, err)
	}
	for i, ref := range refs {
		m, _, err := f.ms.Get(ctx, ref)
		if gone := err == nil && m.Deleted || errors.Is(err, media.ErrNotFound); gone != (i == 1) {
			t.Fatalf("%s deleted %v (%v)", ref, gone, err)
		}
	}
}

// A video still encoding reaches ItemProgressed through the host's media
// queue once its first rendition is out, and ItemReady only once it settles.
func TestProcessorPlayableProgress(t *testing.T) {
	env := s3test.Open(t)
	pool := pgtest.Pool(t, nil)
	var mu sync.Mutex
	var progressed, ready int
	f := newFixtureOn(t, env, func(c *media.Config) {
		c.Hooks.ItemProgressed = func(ctx context.Context, tx pgx.Tx, ref contentref.ContentRef) error {
			mu.Lock()
			progressed++
			mu.Unlock()
			return nil
		}
		c.Hooks.ItemReady = func(ctx context.Context, tx pgx.Tx, ref contentref.ContentRef, r media.Readiness) error {
			mu.Lock()
			ready++
			mu.Unlock()
			return nil
		}
	})
	jobs, err := media.NewJobs(media.JobsConfig{Store: env.Store, Registry: f.reg, Locker: s3test.Locker(t, env.Store), Journal: env.Journal(), Processes: f.q})
	if err != nil {
		t.Fatal(err)
	}
	hostSchema(t, jobs, pool)
	complete := func(ctx context.Context, ref contentref.ContentRef) error {
		return media.CompleteProcessing(ctx, f.ms, ref, func(ctx context.Context, tx pgx.Tx, r media.Readiness) error {
			hooks := f.reg.Config().Hooks
			if r.State == media.StateProcessing {
				if hooks.ItemProgressed != nil {
					return hooks.ItemProgressed(ctx, tx, ref)
				}
				return nil
			}
			return hooks.ItemReady(ctx, tx, ref, r)
		})
	}
	f.visible(1)
	v := f.ref("video", 1)
	m := f.put(v, "source.mp4", "video/mp4", []byte("video"))
	if source, _ := m.Get("source.mp4"); len(source.Pending) == 0 {
		t.Fatalf("the source is not encoding: %+v", source)
	}
	ctx := context.Background()
	counts := func() (int, int) { mu.Lock(); defer mu.Unlock(); return progressed, ready }
	if err := complete(ctx, v); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second)
	if p, r := counts(); p != 1 || r != 0 {
		t.Fatalf("a video with nothing to play was reported: progressed %d, ready %d", p, r)
	}
	// The smallest rung lands while the others still encode.
	playlist := f.blob(v, []byte("#EXTM3U"), "application/vnd.apple.mpegurl")
	if _, err := f.ms.EditExisting(ctx, v, func(m *media.Manifest) error {
		m.Files = append(m.Files, media.File{Path: "hls/480/index.m3u8", Blob: playlist, Type: "application/vnd.apple.mpegurl", Size: 7, From: "source.mp4",
			Preset: "hls", Track: &media.Track{Kind: media.TrackVideo}})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := complete(ctx, v); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "ItemProgressed", func() bool { p, _ := counts(); return p == 2 })
	if _, r := counts(); r != 0 {
		t.Fatalf("a video still encoding was reported ready %d times", r)
	}
}
