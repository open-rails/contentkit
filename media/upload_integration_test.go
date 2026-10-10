package media_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/media"
)

func paths(m *media.Manifest) []string {
	var out []string
	for _, f := range m.Files {
		out = append(out, f.Path)
	}
	return out
}

func code(err error) string {
	if ue, ok := media.AsUploadError(err); ok {
		return ue.Code
	}
	return ""
}

func TestCreateOnlyPreservesExistingUploadsAndRetries(t *testing.T) {
	f := newFixture(t)
	f.visible(1)
	ctx := context.Background()
	g := f.ref("gallery", 1)
	f.put(g, "originals/001.png", "image/png", png(1))
	f.produce(g)
	before, _, _ := f.ms.Get(ctx, g)
	p, blob := f.upload(g, "originals/001 .png", "image/png", png(2))
	if p != "originals/001.png" {
		t.Fatalf("canonical path %q", p)
	}
	if _, err := f.up.Commit(ctx, f.editor, g, []media.Op{{Op: media.OpPut, Path: p, Blob: blob, CreateID: cid(100)}}); code(err) != media.CodeConflict {
		t.Fatalf("create over existing upload: %v", err)
	}
	after, _, _ := f.ms.Get(ctx, g)
	if !reflect.DeepEqual(before.Files, after.Files) {
		t.Fatal("rejected create changed the existing upload or its outputs")
	}

	p, blob = f.upload(g, "originals/002.png", "image/png", png(3))
	op := media.Op{Op: media.OpPut, Path: p, Blob: blob, CreateID: cid(101), Unattached: true}
	f.commit(g, op)
	f.produce(g)
	before = f.commit(g, op, media.Op{Op: media.OpAttach, Path: p, Meta: map[string]any{"alt": "attached"}})
	item, _ := f.reg.Item(g)
	key, _ := item.Staged(blob)
	if _, err := f.env.Store.Head(ctx, key); !errors.Is(err, media.ErrNotFound) {
		t.Fatalf("placement kept the staged object: %v", err)
	}
	after = f.commit(g, op)
	if !reflect.DeepEqual(before.Files, after.Files) {
		t.Fatal("retry after placement restored old attachment, metadata or pending work")
	}
	other := op
	other.CreateID = cid(102)
	if _, err := f.up.Commit(ctx, f.editor, g, []media.Op{other}); code(err) != media.CodeConflict {
		t.Fatalf("a different create reused the receipt: %v", err)
	}
	m := f.commit(g, media.Op{Op: media.OpCopy, From: &media.CopyFrom{ID: g.ContentID, Path: p}, To: "originals/003.png"})
	if copied, _ := m.Get("originals/003.png"); copied.CreateID != "" {
		t.Fatal("copy carried the source's create receipt")
	}
	want := m.Clone()
	want.Files[want.Find(p)].CreateID = ""
	m = f.put(g, p, "image/png", png(3))
	if !reflect.DeepEqual(want.Files, m.Files) {
		t.Fatal("an unchanged intentional put reset processing while clearing its receipt")
	}
	if _, err := f.up.Commit(ctx, f.editor, g, []media.Op{op}); code(err) != media.CodeConflict {
		t.Fatalf("old create overwrote an intentional replacement: %v", err)
	}

	p, blob = f.upload(g, "originals/004.png", "image/png", png(4))
	op = media.Op{Op: media.OpPut, Path: p, Blob: blob, CreateID: cid(103)}
	if _, err := f.up.Commit(ctx, f.editor, g, []media.Op{op}); err != nil {
		t.Fatal(err)
	}
	key, _ = item.Staged(blob)
	if err := f.env.Store.Delete(ctx, key); err != nil {
		t.Fatal(err)
	}
	before = f.place(g)
	if up, _ := before.Get(p); up.Fail() == nil || up.Fail().Code != media.CodeNotUploaded {
		t.Fatalf("missing staged upload did not fail placement: %+v", up)
	}
	after = f.commit(g, op)
	if !reflect.DeepEqual(before.Files, after.Files) {
		t.Fatal("retry of the same failed source changed its failure")
	}
	_, op.Blob = f.upload(g, p, "image/png", png(4))
	after = f.commit(g, op)
	if up, _ := after.Get(p); up.Fail() != nil || up.Blob != blobOf(png(4)) || up.Staged != "" || up.CreateID != op.CreateID {
		t.Fatalf("re-upload with the same receipt did not recover placement: %+v", up)
	}
	key, _ = item.Staged(op.Blob)
	if _, err := f.env.Store.Head(ctx, key); !errors.Is(err, media.ErrNotFound) {
		t.Fatalf("recovered placement left the new staged object: %v", err)
	}
}

func TestConcurrentCreateOnlyCommits(t *testing.T) {
	f := newFixture(t)
	f.visible(1)
	g := f.ref("gallery", 1)
	p, blob := f.upload(g, "originals/new.png", "image/png", png(5))
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := f.up.Commit(context.Background(), f.editor, g, []media.Op{{Op: media.OpPut, Path: p, Blob: blob, CreateID: cid(200 + i)}})
			results <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	var accepted, conflicts int
	for err := range results {
		switch {
		case err == nil:
			accepted++
		case code(err) == media.CodeConflict:
			conflicts++
		default:
			t.Fatalf("concurrent create: %v", err)
		}
	}
	if accepted != 1 || conflicts != 1 {
		t.Fatalf("accepted %d, conflicts %d", accepted, conflicts)
	}
}

// Uploads keep their natural order unless an op gives an index; derived
// files follow their uploads, preset by preset; ops are authorized per path.
func TestCommitOrderAndPaths(t *testing.T) {
	f := newFixture(t)
	f.visible(1)
	g := f.ref("gallery", 1)
	for _, n := range []string{"10", "2", "1"} {
		f.put(g, "originals/"+n+".png", "image/png", png(len(n)*7+int(n[0])))
	}
	m := f.put(g, "originals/0", "image/png", png(99), media.Op{Op: media.OpMove, Path: "originals/1.png", Index: new(int)})
	if want := []string{"originals/1.png", "originals/0.png", "originals/2.png", "originals/10.png"}; !reflect.DeepEqual(paths(m), want) {
		t.Fatalf("order %v, want %v", paths(m), want)
	}
	f.produce(g)
	m, _, _ = f.ms.Get(context.Background(), g)
	want := []string{"originals/1.png", "originals/0.png", "originals/2.png", "originals/10.png",
		"thumb/1.webp", "thumb/0.webp", "thumb/2.webp", "thumb/10.webp", "high/1.webp", "high/0.webp", "high/2.webp", "high/10.webp", "download/pages.zip"}
	if !reflect.DeepEqual(paths(m), want) {
		t.Fatalf("derived order %v", paths(m))
	}
	// Pending is the presets an upload feeds; producing clears it.
	f.put(g, "originals/3.png", "image/png", png(3))
	m, _, _ = f.ms.Get(context.Background(), g)
	if p, _ := m.Get("originals/3.png"); !reflect.DeepEqual(p.Pending, []string{"thumb", "high"}) {
		t.Fatalf("pending %v", p.Pending)
	}
	if r := f.reg.Config().Kinds[0]; r.Readiness(m).State != media.StateProcessing {
		t.Fatal("a pending upload is processing")
	}
	var stems []string
	for _, tg := range f.auth.targets {
		stems = append(stems, tg.Path)
	}
	if !slices.Contains(stems, "originals/10") || !slices.Contains(stems, "originals/1") {
		t.Fatalf("authorized targets %v", stems)
	}

	ctx := context.Background()
	for _, tc := range []struct {
		path, typ string
		size      int
		code      string
	}{
		{"nowhere/x.png", "image/png", 10, media.CodeNotFound},
		{"originals/x.mp4", "video/mp4", 10, media.CodeType},
		{"cover", "image/png", 11 << 20, media.CodeTooLarge},
	} {
		sum := sha256.Sum256([]byte(tc.path))
		_, err := f.up.Presign(ctx, f.editor, media.PresignRequest{Ref: g, Path: tc.path, Type: tc.typ, Size: int64(tc.size), SHA256: sum[:]})
		if code(err) != tc.code {
			t.Errorf("presign %s: %v, want %s", tc.path, err, tc.code)
		}
	}
	if _, err := f.up.Presign(ctx, access.Actor{ID: "reader"}, media.PresignRequest{Ref: g, Path: "cover", Type: "image/png", Size: 1, SHA256: make([]byte, 32)}); code(err) != media.CodeForbidden {
		t.Fatalf("a reader presigned: %v", err)
	}
	// Max caps an Upload's files.
	for i := range 3 {
		p, blob := f.upload(g, "import/a"+string(rune('0'+i))+".zip", "application/zip", []byte{byte(i), 'z'})
		_, err := f.up.Commit(ctx, f.editor, g, []media.Op{{Op: media.OpPut, Path: p, Blob: blob}})
		if i < 2 && err != nil || i == 2 && code(err) != media.CodeTooManyFiles {
			t.Fatalf("import %d: %v", i, err)
		}
	}
}

// A put to the same stem replaces the upload (its outputs follow a new
// extension); rename re-paths the outputs; remove drops them.
func TestReplaceRenameRemove(t *testing.T) {
	f := newFixture(t)
	f.visible(1)
	g := f.ref("gallery", 1)
	f.put(g, "originals/a.png", "image/png", png(1))
	f.put(g, "originals/b.png", "image/png", png(2))
	f.produce(g)
	m := f.put(g, "originals/a.jpg", "image/jpeg", png(3))
	if a, ok := m.Get("originals/a.jpg"); !ok || a.Type != "image/jpeg" || m.Find("originals/a.png") >= 0 {
		t.Fatalf("replace: %v", paths(m))
	}
	if th, _ := m.Get("thumb/a.webp"); th.From != "originals/a.jpg" {
		t.Fatalf("output from %q after a new extension", th.From)
	}
	m = f.commit(g, media.Op{Op: media.OpRename, Path: "originals/b.png", To: "originals/bee"})
	if th, ok := m.Get("thumb/bee.webp"); !ok || th.From != "originals/bee.png" || m.Find("thumb/b.webp") >= 0 {
		t.Fatalf("rename: %v", paths(m))
	}
	if _, err := f.up.Commit(context.Background(), f.editor, g, []media.Op{{Op: media.OpRename, Path: "originals/bee.png", To: "cover"}}); code(err) != media.CodeInvalid {
		t.Fatalf("rename across uploads: %v", err)
	}
	m = f.commit(g, media.Op{Op: media.OpRemove, Path: "originals/bee.png"})
	for _, p := range paths(m) {
		if strings.Contains(p, "bee") {
			t.Fatalf("remove kept %s", p)
		}
	}
	// A retried put is a no-op.
	before := f.put(g, "originals/c.png", "image/png", png(4))
	if after := f.put(g, "originals/c.png", "image/png", png(4)); !reflect.DeepEqual(paths(before), paths(after)) {
		t.Fatal("retry changed the manifest")
	}
}

// Edits are bounded by the upload's public preset: the crop is fitted to its
// aspect and checked against the measured size and MinWidth.
func TestEdit(t *testing.T) {
	f := newFixture(t)
	f.visible(1)
	g := f.ref("gallery", 1)
	f.put(g, "cover.png", "image/png", png(1))
	ctx := context.Background()
	if _, err := f.ms.EditExisting(ctx, g, func(m *media.Manifest) error {
		m.Files[m.Find("cover.png")].W, m.Files[m.Find("cover.png")].H = 920, 1300
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	m := f.commit(g, media.Op{Op: media.OpEdit, Path: "cover.png", Edit: &media.Edit{Crop: &media.Crop{X: 0, Y: 0, W: 460, H: 1}}})
	c, _ := m.Get("cover.png")
	if c.Edit == nil || c.Edit.Crop.H != 650 || !slices.Contains(c.Pending, "cover") {
		t.Fatalf("fitted edit %+v pending %v", c.Edit, c.Pending)
	}
	for _, e := range []media.Edit{
		{Crop: &media.Crop{X: 0, Y: 0, W: 50, H: 1}},        // under MinWidth
		{Crop: &media.Crop{X: 800, Y: 0, W: 460, H: 1}},     // outside the source
		{Crop: &media.Crop{X: 0, Y: 0, W: 460}, Rotate: 45}, // invalid rotation
	} {
		if _, err := f.up.Commit(ctx, f.editor, g, []media.Op{{Op: media.OpEdit, Path: "cover.png", Edit: &e}}); err == nil {
			t.Errorf("edit %+v accepted", e)
		}
	}
	m = f.commit(g, media.Op{Op: media.OpEdit, Path: "cover.png"})
	if c, _ := m.Get("cover.png"); c.Edit != nil {
		t.Fatalf("a nil edit kept %+v", c.Edit)
	}
}

// Unattached uploads are processed but not part of the item until attached.
func TestUnattachedAttach(t *testing.T) {
	f := newFixture(t)
	f.visible(1)
	g := f.ref("gallery", 1)
	f.put(g, "originals/1.png", "image/png", png(1))
	p, blob := f.upload(g, "originals/2.png", "image/png", png(2))
	m := f.commit(g, media.Op{Op: media.OpPut, Path: p, Blob: blob, Unattached: true})
	if u, _ := m.Get(p); !u.Unattached || f.reg.Config().Kinds[0].Readiness(m).Processing[0] != "originals/1.png" {
		t.Fatalf("unattached %+v", u)
	}
	f.produce(g)
	res, err := f.rd.Read(context.Background(), g, f.editor, media.ReadOptions{Prefix: "thumb/"})
	if err != nil || len(res.Files) != 1 || res.Files[0].Path != "thumb/1.webp" {
		t.Fatalf("viewers see unattached outputs: %+v %v", res, err)
	}
	m = f.commit(g, media.Op{Op: media.OpAttach, Path: p, Index: new(int), Meta: map[string]any{"alt": "first"}})
	if want := []string{"originals/2.png", "originals/1.png"}; !reflect.DeepEqual(paths(m)[:2], want) {
		t.Fatalf("attach at 0: %v", paths(m))
	}
	if u, _ := m.Get(p); u.Unattached || u.Meta["alt"] != "first" {
		t.Fatalf("attached %+v", u)
	}
}

// copy brings an upload and its current outputs from another item of the
// kind, server-side; its public presets render in the new item.
func TestCopy(t *testing.T) {
	f := newFixture(t)
	f.visible(1)
	f.visible(2)
	a, b := f.ref("gallery", 1), f.ref("gallery", 2)
	f.put(a, "cover.png", "image/png", png(1))
	f.put(a, "originals/1.png", "image/png", png(2))
	f.produce(a)
	m := f.commit(b, media.Op{Op: media.OpCopy, From: &media.CopyFrom{ID: cid(1), Path: "originals/1.png"}},
		media.Op{Op: media.OpCopy, From: &media.CopyFrom{ID: cid(1), Path: "cover.png"}})
	want := []string{"originals/1.png", "cover.png", "thumb/1.webp", "high/1.webp"}
	if !reflect.DeepEqual(paths(m), want) {
		t.Fatalf("copied %v", paths(m))
	}
	if c, _ := m.Get("cover.png"); !reflect.DeepEqual(c.Pending, []string{"cover"}) || len(c.Public) != 0 {
		t.Fatalf("copied cover has pending=%v, public ownership=%+v", c.Pending, c.Public)
	}
	if res, err := f.rd.Read(t.Context(), b, f.editor, media.ReadOptions{}); err != nil || len(res.Public) != 0 {
		t.Fatalf("copied originals advertised nonexistent public files: %+v, %v", res, err)
	}
	item, _ := f.reg.Item(b)
	for _, blob := range m.Blobs() {
		key, _ := item.Blob(blob)
		if _, err := f.env.Store.Head(context.Background(), key); err != nil {
			t.Fatalf("copied blob %s: %v", key, err)
		}
	}
	if _, err := f.up.Commit(context.Background(), f.editor, b, []media.Op{{Op: media.OpCopy, From: &media.CopyFrom{ID: cid(1), Path: "originals/9.png"}}}); code(err) != media.CodeNotFound {
		t.Fatalf("copy of a missing upload: %v", err)
	}
	m = f.commit(b, media.Op{Op: media.OpCopy, From: &media.CopyFrom{ID: cid(1), Path: "originals/1.png"}, To: "cover"})
	before := m.Clone()
	m = f.commit(b, media.Op{Op: media.OpCopy, From: &media.CopyFrom{ID: cid(2), Path: "originals/1.png"}, To: "cover"},
		media.Op{Op: media.OpEdit, Path: "cover.png"})
	page, _ := m.Get("originals/1.png")
	cover, _ := m.Get("cover.png")
	if cover.Blob != page.Blob || cover.Edit != nil || !reflect.DeepEqual(cover.Pending, []string{"cover"}) {
		t.Fatalf("copied page cover: %+v", cover)
	}
	for _, path := range []string{"originals/1.png", "thumb/1.webp", "high/1.webp"} {
		got, _ := m.Get(path)
		want, _ := before.Get(path)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("cover copy changed source %q: %+v", path, got)
		}
	}
	ctx := context.Background()
	for _, ops := range [][]media.Op{
		{{Op: media.OpRemove, Path: page.Path}, {Op: media.OpCopy, From: &media.CopyFrom{ID: cid(2), Path: page.Path}, To: "cover"}},
		{{Op: media.OpCopy, From: &media.CopyFrom{ID: cid(2), Path: "originals/missing.png"}, To: "cover"}},
	} {
		if _, err := f.up.Commit(ctx, f.editor, b, ops); code(err) != media.CodeNotFound {
			t.Fatalf("copy must use current manifest state: %v", err)
		}
	}
	f.put(b, "import/book.zip", "application/zip", []byte("zip"))
	if _, err := f.up.Commit(ctx, f.editor, b, []media.Op{{Op: media.OpCopy, From: &media.CopyFrom{ID: cid(2), Path: "import/book.zip"}, To: "cover"}}); code(err) != media.CodeType {
		t.Fatalf("copy bypassed cover types: %v", err)
	}
	if _, err := f.up.Commit(ctx, f.editor, b, []media.Op{{Op: media.OpCopy, From: &media.CopyFrom{ID: cid(2), Path: page.Path}, To: "import/book"}}); code(err) != media.CodeType {
		t.Fatalf("copy bypassed archive types: %v", err)
	}
	if _, err := f.ms.EditExisting(ctx, a, func(m *media.Manifest) error {
		m.Files[m.Find(page.Path)].Gone = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	m = f.commit(b, media.Op{Op: media.OpCopy, From: &media.CopyFrom{ID: cid(1), Path: page.Path}, To: "originals/2"})
	if copied, _ := m.Get("originals/2.png"); !copied.Gone || copied.Blob != page.Blob {
		t.Fatalf("copy of retained outputs: %+v", copied)
	}
	if _, err := f.up.Commit(ctx, f.editor, b, []media.Op{{Op: media.OpCopy, From: &media.CopyFrom{ID: cid(1), Path: page.Path}, To: "cover"}}); code(err) != media.CodeNotFound {
		t.Fatalf("cover copy without its original: %v", err)
	}
}

// A copy is reserved once, like an upload of its source, before any byte
// moves (its outputs' blobs are not reserved): past the uploader's rate
// limit nothing is copied, so a refused or failed commit leaves no
// unmetered blobs. An identical blob the sweep may take first is copied
// again (refreshed) rather than reused.
func TestCopyIsMetered(t *testing.T) {
	f := newFixture(t)
	f.visible(1)
	f.visible(2)
	ctx := context.Background()
	a, b := f.ref("gallery", 1), f.ref("gallery", 2)
	f.put(a, "originals/1.png", "image/png", png(1))
	f.put(a, "originals/2.png", "image/png", png(2))
	f.produce(a)
	// A thumb of its own blob: copying page 1 moves two blobs.
	src, _ := f.reg.Item(a)
	thumb := []byte("thumb one")
	key, _ := src.Blob(blobOf(thumb))
	f.object(key, string(thumb))
	if _, err := f.ms.EditExisting(ctx, a, func(m *media.Manifest) error {
		m.Files[m.Find("thumb/1.webp")].Blob = blobOf(thumb)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	limiter, err := media.NewPGLimiter(f.env.Pool(), f.env.ContentSchema(), media.PGLimits{FilesPerHour: 1})
	if err != nil {
		t.Fatal(err)
	}
	up, err := media.NewUploads(media.UploadOptions{Store: f.env.Store, Manifests: f.ms, Queue: f.q, Limiter: limiter, Grace: 8 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	copyOf := func(path string) []media.Op {
		return []media.Op{{Op: media.OpCopy, From: &media.CopyFrom{ID: cid(1), Path: path}}}
	}
	item, _ := f.reg.Item(b)
	one, _ := item.Blob(blobOf(png(1)))
	two, _ := item.Blob(blobOf(png(2)))
	if _, err := up.Commit(ctx, f.editor, b, copyOf("originals/1.png")); err != nil {
		t.Fatalf("one copy, two blobs, one reservation: %v", err)
	}
	if copied, _ := item.Blob(blobOf(thumb)); !f.exists(copied) {
		t.Fatal("the thumb was not copied")
	}
	if _, err := up.Commit(ctx, f.editor, b, copyOf("originals/2.png")); code(err) != media.CodeRate || f.exists(two) {
		t.Fatalf("a copy past the rate limit: %v, copied %v", err, f.exists(two))
	}

	// Unreferenced and older than the grace period: copied again.
	f.commit(b, media.Op{Op: media.OpRemove, Path: "originals/1.png"})
	old, err := f.env.Store.Head(ctx, one)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(9 * time.Second)
	unlimited, err := media.NewUploads(media.UploadOptions{Store: f.env.Store, Manifests: f.ms, Queue: f.q, Grace: 8 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	// Nor is it committed again by name: a blob never outlives its sweep unmetered.
	if p, err := unlimited.Presign(ctx, f.editor, media.PresignRequest{Ref: b, Path: "originals/again.png", Type: "image/png",
		Size: int64(len(png(1))), SHA256: mustSum(blobOf(png(1)))}); err != nil || p.Exists || p.Put == nil {
		t.Fatalf("presign offered a blob due for cleanup: %+v %v", p, err)
	}
	if _, err := unlimited.Commit(ctx, f.editor, b, []media.Op{{Op: media.OpPut, Path: "originals/again.png", Blob: blobOf(png(1))}}); code(err) != media.CodeNotUploaded {
		t.Fatalf("a put naming a blob due for cleanup: %v", err)
	}
	if _, err := unlimited.Commit(ctx, f.editor, b, copyOf("originals/1.png")); err != nil {
		t.Fatal(err)
	}
	if obj, err := f.env.Store.Head(ctx, one); err != nil || !obj.LastModified.After(old.LastModified) {
		t.Fatalf("an unprotected blob was reused: %v %v", obj.LastModified, err)
	}
}

// An editor view is named by its source, not its bytes: no upload may name
// it, through presign's "exists" or a put.
func TestEditorViewIsNotAnUpload(t *testing.T) {
	f := newFixture(t)
	f.visible(1)
	ctx := context.Background()
	g := f.ref("gallery", 1)
	m := f.put(g, "cover.png", "image/png", png(1))
	cover, _ := m.Get("cover.png")
	view := f.reg.EditorView(cover)
	item, _ := f.reg.Item(g)
	key, _ := item.Blob(view)
	if _, err := f.env.Store.Put(ctx, key, strings.NewReader("view"), 4, media.PutOptions{ContentType: "image/webp"}); err != nil {
		t.Fatal(err)
	}
	p, err := f.up.Presign(ctx, f.editor, media.PresignRequest{Ref: g, Path: "originals/v.webp", Type: "image/webp", Size: 4, SHA256: mustSum(view)})
	if err != nil || p.Exists || p.Blob == view {
		t.Fatalf("presign offered an editor view: %+v %v", p, err)
	}
	if _, err := f.up.Commit(ctx, f.editor, g, []media.Op{{Op: media.OpPut, Path: "originals/v.webp", Blob: view}}); code(err) != media.CodeNotUploaded {
		t.Fatalf("a put naming an editor view: %v", err)
	}
}

// frame fills an upload from its Frames video; a new video grabs again.
func TestFrame(t *testing.T) {
	f := newFixture(t)
	f.visible(1)
	v := f.ref("video", 1)
	ctx := context.Background()
	if _, err := f.up.Commit(ctx, f.editor, v, []media.Op{{Op: media.OpFrame, Path: "poster", Auto: true}}); code(err) != media.CodeNotFound {
		t.Fatalf("frame without a video: %v", err)
	}
	f.put(v, "source.mp4", "video/mp4", []byte("video one"))
	m := f.commit(v, media.Op{Op: media.OpFrame, Path: "poster", T: ptr(3.5)})
	p, _ := m.Get("poster.png")
	if p.Frame == nil || p.Frame.T != 3.5 || p.Blob != "" || !reflect.DeepEqual(p.Pending, []string{"poster"}) {
		t.Fatalf("frame placeholder %+v", p)
	}
	if r := f.reg.Config().Kinds[2].Readiness(m); !slices.Contains(r.Processing, "poster.png") {
		t.Fatalf("readiness %+v", r)
	}
	// The worker grabs it; a new video resets it.
	if _, err := f.ms.EditExisting(ctx, v, func(m *media.Manifest) error {
		i := m.Find("poster.png")
		m.Files[i].Blob, m.Files[i].Size, m.Files[i].Frame.Of = blobOf([]byte("frame")), 5, blobOf([]byte("video one"))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	m = f.put(v, "source.mp4", "video/mp4", []byte("video two"))
	if p, _ := m.Get("poster.png"); p.Blob != "" || p.Frame.T != 3.5 || p.Frame.Of != "" {
		t.Fatalf("not grabbed again: %+v", p)
	}
	if _, err := f.up.Commit(ctx, f.editor, v, []media.Op{{Op: media.OpFrame, Path: "source", Auto: true}}); code(err) != media.CodeInvalid {
		t.Fatalf("frame into an upload without Frames: %v", err)
	}
	// An unattached video is not part of the item yet: no frame from it.
	u := f.ref("video", 2)
	f.visible(2)
	vp, vb := f.upload(u, "source.mp4", "video/mp4", []byte("video three"))
	f.commit(u, media.Op{Op: media.OpPut, Path: vp, Blob: vb, Unattached: true})
	if _, err := f.up.Commit(ctx, f.editor, u, []media.Op{{Op: media.OpFrame, Path: "poster", Auto: true}}); code(err) != media.CodeConflict {
		t.Fatalf("frame from an unattached video: %v", err)
	}
}

// Commits are rate limited per uploader (audit); exempt grants are not.
func TestCommitRateLimit(t *testing.T) {
	f := newFixture(t)
	f.visible(1)
	g := f.ref("gallery", 1)
	ctx := context.Background()
	up, err := media.NewUploads(media.UploadOptions{Store: f.env.Store, Manifests: f.ms, Queue: f.q, Commits: media.RateLimit{PerSecond: 0.01, Burst: 2}})
	if err != nil {
		t.Fatal(err)
	}
	meta := func(i int) []media.Op {
		return []media.Op{{Op: media.OpMeta, Meta: map[string]any{"title": fmt.Sprint(i)}}}
	}
	for i := range 3 {
		_, err := up.Commit(ctx, f.editor, g, meta(i))
		if i < 2 && err != nil || i == 2 && code(err) != media.CodeRate {
			t.Fatalf("commit %d: %v", i, err)
		}
	}
	if ue, _ := media.AsUploadError(func() error { _, err := up.Commit(ctx, f.editor, g, meta(3)); return err }()); ue == nil || ue.RetryAfter <= 0 {
		t.Fatalf("no retry-after: %+v", ue)
	}
	for i := range 3 {
		if _, err := up.Commit(ctx, access.Actor{ID: "staff"}, g, meta(10+i)); err != nil {
			t.Fatalf("exempt commit %d: %v", i, err)
		}
	}
}

func ptr[T any](v T) *T { return &v }

// meta fills download names; regenerate asks the worker for a preset.
func TestMetaAndRegenerate(t *testing.T) {
	f := newFixture(t)
	f.visible(1)
	g := f.ref("gallery", 1)
	f.put(g, "originals/1.png", "image/png", png(1))
	f.produce(g)
	ctx := context.Background()
	if _, err := f.ms.EditExisting(ctx, g, func(m *media.Manifest) error {
		return m.SetOutputs("high/", "zip", []media.File{{Path: "download/pages.zip", Blob: blobOf([]byte("zip")), Type: "application/zip", FP: "x"}})
	}); err != nil {
		t.Fatal(err)
	}
	m := f.commit(g, media.Op{Op: media.OpMeta, Meta: map[string]any{"title": "[Artist] Name / Part 1"}})
	if z, _ := m.Get("download/pages.zip"); z.Download != "[Artist] Name  Part 1.zip" {
		t.Fatalf("download name %q", z.Download)
	}
	f.q.take()
	// Forcing rewrites and purges everything: exempt (staff) grants only.
	if _, err := f.up.Commit(ctx, f.editor, g, []media.Op{{Op: media.OpRegenerate, Preset: "thumb", Force: true}}); code(err) != media.CodeForbidden {
		t.Fatalf("a writer forced regeneration: %v", err)
	}
	if _, err := f.up.Commit(ctx, access.Actor{ID: "staff"}, g, []media.Op{{Op: media.OpRegenerate, Preset: "thumb", Force: true}}); err != nil {
		t.Fatal(err)
	}
	if jobs := f.q.take(); len(jobs) != 1 || jobs[0].Preset != "thumb" || !jobs[0].Force {
		t.Fatalf("regenerate enqueued %+v", jobs)
	}
	if _, err := f.up.Commit(ctx, f.editor, g, []media.Op{{Op: media.OpRegenerate, Preset: "nope"}}); code(err) != media.CodeNotFound {
		t.Fatalf("an unknown preset: %v", err)
	}
}

// Commits HEAD-check what they name: a staged upload or blob that is not
// there is not_uploaded; an identical blob already in the folder needs no
// upload.
func TestCommitVerifiesBlobs(t *testing.T) {
	f := newFixture(t)
	f.visible(1)
	g := f.ref("gallery", 1)
	ctx := context.Background()
	missing := blobOf([]byte("never uploaded"))
	staged := media.NewStaged()
	_, err := f.up.Commit(ctx, f.editor, g, []media.Op{{Op: media.OpPut, Path: "originals/1.png", Blob: missing},
		{Op: media.OpPut, Path: "originals/2.png", Blob: staged}})
	var ue *media.UploadError
	if !errors.As(err, &ue) || ue.Code != media.CodeNotUploaded || !reflect.DeepEqual(ue.Blobs, []string{missing, staged}) {
		t.Fatalf("not uploaded: %v", err)
	}
	if _, err := f.up.Commit(ctx, f.editor, g, []media.Op{{Op: media.OpPut, Path: "originals/1.png", Blob: "u-not-a-uuid"}}); code(err) != media.CodeInvalid {
		t.Fatalf("a malformed staged name: %v", err)
	}
	f.put(g, "originals/1.png", "image/png", png(1))
	sum := sha256.Sum256(png(1))
	p, err := f.up.Presign(ctx, f.editor, media.PresignRequest{Ref: g, Path: "originals/again.png", Type: "image/png", Size: int64(len(png(1))), SHA256: sum[:]})
	if err != nil || !p.Exists || p.Put != nil || p.Blob != blobOf(png(1)) {
		t.Fatalf("exists: %+v %v", p, err)
	}
	if m := f.commit(g, media.Op{Op: media.OpPut, Path: p.Path, Blob: p.Blob}); m.Find(p.Path) < 0 {
		t.Fatalf("existing blob not committed: %v", paths(m))
	}
}

// A Named upload is named by the server; a new item starts hidden when
// anonymous viewers cannot see it, so its public presets are not pending.
func TestNamedAndHiddenNewItem(t *testing.T) {
	f := newFixture(t)
	f.res.set(cid(1), access.Resolution{Visible: false})
	post := f.ref("post", 1)
	p, blob := f.upload(post, "inline/whatever.png", "image/png", png(1))
	if !strings.HasPrefix(p, "inline/"+media.NamedPrefix) || !strings.HasSuffix(p, ".png") || !media.ValidNamed(strings.TrimSuffix(strings.TrimPrefix(p, "inline/"), ".png")) {
		t.Fatalf("named path %q", p)
	}
	m := f.commit(post, media.Op{Op: media.OpPut, Path: p, Blob: blob})
	if u, _ := m.Get(p); !m.Hidden || u.Pending != nil {
		t.Fatalf("hidden new item: hidden %v pending %v", m.Hidden, u.Pending)
	}
	if _, err := f.up.Commit(context.Background(), f.editor, post, []media.Op{{Op: media.OpPut, Path: "inline/mine.png", Blob: blobOf(png(1))}}); code(err) != media.CodeInvalid {
		t.Fatalf("a client-chosen name: %v", err)
	}
}

// Ingest streams a server-side import to a staged upload, placed like any other.
func TestIngest(t *testing.T) {
	f := newFixture(t)
	f.visible(1)
	g := f.ref("gallery", 1)
	ctx := context.Background()
	body := bytes.Repeat([]byte("0123456789abcdef"), (11<<20)/16+3)
	var resumed media.IngestUpload
	res, err := f.up.Ingest(ctx, f.editor, media.IngestRequest{Ref: g, Path: "import/legacy.zip", Type: "application/zip",
		Body: bytes.NewReader(body), Size: int64(len(body)), PartSize: 5 << 20,
		OnUpload: func(u media.IngestUpload) error { resumed = u; return nil }})
	if err != nil {
		t.Fatal(err)
	}
	if res.Size != int64(len(body)) || resumed.Temp != res.Staged {
		t.Fatalf("ingest %+v %+v", res, resumed)
	}
	if u, ok := res.Manifest.Get("import/legacy.zip"); !ok || u.Staged != res.Staged || u.Blob != "" {
		t.Fatalf("committed %v", paths(res.Manifest))
	}
	small, err := f.up.Ingest(ctx, f.editor, media.IngestRequest{Ref: g, Path: "originals/1.png", Type: "image/png", Body: bytes.NewReader(png(1))})
	if err != nil || small.Staged == "" {
		t.Fatalf("single ingest %+v %v", small, err)
	}
	m := f.place(g)
	if u, _ := m.Get("import/legacy.zip"); u.Blob != blobOf(body) || u.Staged != "" {
		t.Fatalf("placed %+v", u)
	}
	if u, _ := m.Get("originals/1.png"); u.Blob != blobOf(png(1)) {
		t.Fatalf("placed %+v", u)
	}
	item, _ := f.reg.Item(g)
	for o, err := range f.env.Store.List(ctx, item.TempPrefix()) {
		t.Fatalf("temp/ kept %s %v", o.Key, err)
	}
}
