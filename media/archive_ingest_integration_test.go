package media_test

import (
	"archive/zip"
	"bytes"
	"context"
	"image"
	"image/color"
	pngimage "image/png"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/internal/s3test"
)

func TestGenericArchiveIngestionReplaysAndPreservesServerMetadata(t *testing.T) {
	f, ledger := archiveFixture(t, 8<<20)
	ctx := t.Context()
	ref := f.ref("post", 1)
	f.visible(1)
	a, b := archivePNG(t, 1), archivePNG(t, 2)
	source := archiveBytes(t, map[string][]byte{"page2.png": a, "page10.png": b})
	p, blob := f.upload(ref, "zip/book", "application/zip", source)
	m := f.commit(ref, media.Op{Op: media.OpPut, Path: p, Blob: blob, Meta: map[string]any{"archive_pages_total": 999999, "admission": map[string]any{"owner": "victim"}}})
	initial, _ := m.Get(p)
	if initial.Admission == nil || initial.Admission.Owner != "owner" || initial.Admission.Target != "inline/{name}" {
		t.Fatalf("verified admission=%+v", initial.Admission)
	}
	if err := f.ms.IngestArchives(ctx, ref, ""); err != nil {
		t.Fatal(err)
	}
	m, _, err := f.ms.Get(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	first, _ := m.Get(p)
	generation := first.Admission.Generation
	if generation == "" {
		t.Fatal("source admission has no accepting commit identity")
	}
	if first.Meta["archive_completed_sha256"] == nil || archiveCount(first.Meta["archive_pages_total"]) != 2 {
		t.Fatalf("completed source=%+v", first)
	}
	pages := map[string]string{}
	for _, file := range m.Files {
		if file.IsUpload() && file.Path != p {
			pages[file.Path] = file.Blob
		}
	}
	if len(pages) != 2 {
		t.Fatalf("imported pages=%v", pages)
	}
	for path, blob := range pages {
		// Generic archive targets retain provenance through ordinary caption
		// edits, just as gallery reader pages do.
		f.commit(ref, media.Op{Op: media.OpPut, Path: path, Blob: blob, Meta: map[string]any{"caption": "edited page"}})
		break
	}
	used, pending, err := ledger.Usage(ctx, ref.TenantID, "owner")
	if err != nil || used != m.UploadBytes() || pending != 0 {
		t.Fatalf("usage=%d pending=%d err=%v", used, pending, err)
	}
	// An ordinary same-source metadata edit preserves server completion state.
	f.commit(ref, media.Op{Op: media.OpPut, Path: p, Blob: first.Blob, Meta: map[string]any{"caption": "changed", "archive_pages_total": 1000}})
	if err = f.ms.IngestArchives(ctx, ref, ""); err != nil {
		t.Fatal(err)
	}
	m, _, err = f.ms.Get(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	updated, _ := m.Get(p)
	if updated.Meta["archive_completed_sha256"] != first.Meta["archive_completed_sha256"] || updated.Meta["caption"] != "changed" || updated.Admission.Generation != generation {
		t.Fatalf("same-source edit lost completion: %+v", updated.Meta)
	}
	// The same ZIP bytes adopted under another path have their own completion
	// receipt, without duplicating pages or charging decoded originals again.
	second := "zip/second.zip"
	f.commit(ref, media.Op{Op: media.OpPut, Path: second, Blob: first.Blob})
	if err = f.ms.IngestArchives(ctx, ref, ""); err != nil {
		t.Fatal(err)
	}
	m, _, err = f.ms.Get(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	adopted, _ := m.Get(second)
	if adopted.Meta["archive_completed_sha256"] == nil {
		t.Fatal("second archive path stayed pending")
	}
	if len(m.Files) != 4 {
		t.Fatalf("archive alias duplicated edited pages: %+v", m.Files)
	}
	for path, blob := range pages {
		file, ok := m.Get(path)
		if !ok || file.Blob != blob {
			t.Fatal("replay changed retained page identity")
		}
	}
	now, _, err := ledger.Usage(ctx, ref.TenantID, "owner")
	if err != nil || now != m.UploadBytes() {
		t.Fatalf("replay growth=%d want=%d err=%v", now, m.UploadBytes(), err)
	}
}

func TestArchiveQuotaRefusesBeforeAnyDecodedBlobWrite(t *testing.T) {
	source := archiveBytes(t, map[string][]byte{"1.png": archivePNG(t, 3), "2.png": archivePNG(t, 4)})
	f, ledger := archiveFixture(t, max(int64(len(source)), 64<<10)+1)
	ctx := t.Context()
	ref := f.ref("post", 2)
	f.visible(2)
	m := f.put(ref, "zip/book", "application/zip", source)
	count := func() int {
		var n int
		if err := f.env.Pool().QueryRow(ctx, `SELECT count(*) FROM `+pgx.Identifier{f.env.ContentSchema(), "content_media_allocations"}.Sanitize()).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := count()
	if err := f.ms.IngestArchives(ctx, ref, ""); err != nil {
		t.Fatal(err)
	}
	m, _, err := f.ms.Get(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Files) != 1 || m.Files[0].Fail() == nil {
		t.Fatalf("quota refusal attached pages: %+v", m.Files)
	}
	if after := count(); after != before {
		t.Fatalf("quota-refused work allocated %d decoded objects", after-before)
	}
	used, pending, err := ledger.Usage(ctx, ref.TenantID, "owner")
	if err != nil || used != m.UploadBytes() || pending != 0 {
		t.Fatalf("quota refusal usage=%d/%d err=%v", used, pending, err)
	}
}

func TestArchiveDestinationRequiresExplicitUploadGrant(t *testing.T) {
	f, _ := archiveFixture(t, 8<<20)
	f.auth.archiveTo = ""
	ref := f.ref("post", 3)
	f.visible(3)
	p, blob := f.upload(ref, "zip/denied", "application/zip", archiveBytes(t, map[string][]byte{"1.png": archivePNG(t, 5)}))
	_, err := f.up.Commit(t.Context(), f.editor, ref, uuid.NewString(), []media.Op{{Op: media.OpPut, Path: p, Blob: blob}})
	if code(err) != media.CodeForbidden {
		t.Fatalf("ZIP source permission expanded destination: %v", err)
	}
}

func TestStagedArchiveProcessesBeforeSaveAndAttachesAsOneBatch(t *testing.T) {
	for _, put := range []bool{false, true} {
		name := "attach"
		if put {
			name = "put"
		}
		t.Run(name, func(t *testing.T) {
			f, _ := archiveFixture(t, 8<<20)
			ref := f.ref("post", 4)
			f.visible(4)
			p, blob := f.upload(ref, "zip/staged", "application/zip", archiveBytes(t, map[string][]byte{"1.png": archivePNG(t, 7), "2.png": archivePNG(t, 8)}))
			f.commit(ref, media.Op{Op: media.OpPut, Path: p, Blob: blob, Unattached: true})
			if err := f.ms.IngestArchives(t.Context(), ref, ""); err != nil {
				t.Fatal(err)
			}
			m, _, err := f.ms.Get(t.Context(), ref)
			if err != nil {
				t.Fatal(err)
			}
			if len(m.Files) != 3 || slices.ContainsFunc(m.Files, func(file media.File) bool { return !file.Unattached }) {
				t.Fatalf("processing exposed staged pages: %+v", m.Files)
			}
			source, _ := m.Get(p)
			if source.Meta["archive_completed_sha256"] == nil {
				t.Fatal("unattached archive was not processed")
			}
			operation := media.Op{Op: media.OpAttach, Path: p}
			if put {
				operation = media.Op{Op: media.OpPut, Path: p, Blob: source.Blob}
			}
			// Save must recheck destination authority and leave the full batch
			// staged if that authority was revoked while extraction ran.
			f.auth.archiveTo = ""
			if _, err := f.up.Commit(t.Context(), f.editor, ref, uuid.NewString(), []media.Op{operation}); code(err) != media.CodeForbidden {
				t.Fatalf("revoked destination allowed attachment: %v", err)
			}
			m, _, err = f.ms.Get(t.Context(), ref)
			if err != nil || slices.ContainsFunc(m.Files, func(file media.File) bool { return !file.Unattached }) {
				t.Fatalf("refused Save partially attached batch: %+v / %v", m, err)
			}
			f.auth.archiveTo = "inline/{name}"
			m = f.commit(ref, operation)
			if len(m.Files) != 3 || slices.ContainsFunc(m.Files, func(file media.File) bool { return file.Unattached }) {
				t.Fatalf("Save did not attach the complete batch: %+v", m.Files)
			}
		})
	}
}

func TestCancellingStagedArchivePreservesAttachedPagesAndOtherAliases(t *testing.T) {
	f, ledger := archiveFixture(t, 8<<20)
	ref := f.ref("post", 5)
	f.visible(5)
	source := archiveBytes(t, map[string][]byte{"1.png": archivePNG(t, 9), "2.png": archivePNG(t, 10)})
	p, blob := f.upload(ref, "zip/staged", "application/zip", source)
	f.commit(ref, media.Op{Op: media.OpPut, Path: p, Blob: blob, Unattached: true})
	if err := f.ms.IngestArchives(t.Context(), ref, ""); err != nil {
		t.Fatal(err)
	}
	m, _, err := f.ms.Get(t.Context(), ref)
	if err != nil {
		t.Fatal(err)
	}
	archive, _ := m.Get(p)
	page := m.Files[slices.IndexFunc(m.Files, func(file media.File) bool { return strings.HasPrefix(file.Path, "inline/") })]
	// An editor can already have adopted one page from the extracted batch.
	// Cancelling the source must preserve that page and its quota charge.
	f.commit(ref, media.Op{Op: media.OpAttach, Path: page.Path}, media.Op{Op: media.OpPut, Path: "zip/alias.zip", Blob: archive.Blob, Unattached: true})
	m = f.commit(ref, media.Op{Op: media.OpRemove, Path: p})
	if len(m.Files) != 3 {
		t.Fatalf("removing one ZIP alias removed its shared pages: %+v", m.Files)
	}
	m = f.commit(ref, media.Op{Op: media.OpRemove, Path: "zip/alias.zip"})
	if len(m.Files) != 1 || m.Files[0].Path != page.Path || m.Files[0].Unattached {
		t.Fatalf("cancelled batch did not retain exactly the adopted page: %+v", m.Files)
	}
	used, pending, err := ledger.Usage(t.Context(), ref.TenantID, "owner")
	if err != nil || used != m.UploadBytes() || pending != 0 {
		t.Fatalf("cancelled batch quota=%d pending=%d err=%v", used, pending, err)
	}
}

func TestReplacingStagedArchiveRetiresItsUnusedPages(t *testing.T) {
	f, _ := archiveFixture(t, 8<<20)
	ref := f.ref("post", 6)
	f.visible(6)
	p, blob := f.upload(ref, "zip/staged", "application/zip", archiveBytes(t, map[string][]byte{"1.png": archivePNG(t, 11), "2.png": archivePNG(t, 12)}))
	f.commit(ref, media.Op{Op: media.OpPut, Path: p, Blob: blob, Unattached: true})
	if err := f.ms.IngestArchives(t.Context(), ref, ""); err != nil {
		t.Fatal(err)
	}
	p, blob = f.upload(ref, "zip/staged", "application/zip", archiveBytes(t, map[string][]byte{"new.png": archivePNG(t, 13)}))
	m := f.commit(ref, media.Op{Op: media.OpPut, Path: p, Blob: blob, Unattached: true})
	if len(m.Files) != 1 || m.Files[0].Path != p {
		t.Fatalf("replacing staged archive retained stale pages: %+v", m.Files)
	}
	if err := f.ms.IngestArchives(t.Context(), ref, ""); err != nil {
		t.Fatal(err)
	}
	m, _, err := f.ms.Get(t.Context(), ref)
	if err != nil || len(m.Files) != 2 || slices.ContainsFunc(m.Files, func(file media.File) bool { return !file.Unattached }) {
		t.Fatalf("replacement batch=%+v err=%v", m, err)
	}
}

func TestCopiedArchiveUsesDestinationPermissionAndQuota(t *testing.T) {
	for _, limited := range []bool{false, true} {
		name, quota := "admitted", int64(8<<20)
		if limited {
			name, quota = "quota-limited", 64<<10+1
		}
		t.Run(name, func(t *testing.T) {
			f, ledger := archiveFixture(t, quota)
			from, to := f.ref("post", 7), f.ref("post", 8)
			f.visible(7)
			f.visible(8)
			sourceGrant := media.UploadGrant{Allowed: true, Exempt: true, Owner: "source-owner", ArchiveTo: "inline/{name}"}
			destinationGrant := media.UploadGrant{Allowed: true, Owner: "destination-owner"}
			f.auth.grants = map[string]media.UploadGrant{from.ContentID: sourceGrant, to.ContentID: destinationGrant}
			body := archiveBytes(t, map[string][]byte{"1.png": archivePNG(t, 14), "2.png": archivePNG(t, 15)})
			p, blob := f.upload(from, "zip/original", "application/zip", body)
			original := f.commit(from, media.Op{Op: media.OpPut, Path: p, Blob: blob})
			operation := media.Op{Op: media.OpCopy, From: &media.CopyFrom{ID: from.ContentID, Path: p}, To: "zip/copied.zip"}
			if _, err := f.up.Commit(t.Context(), f.editor, to, uuid.NewString(), []media.Op{operation}); code(err) != media.CodeForbidden {
				t.Fatalf("source admission authorized destination expansion: %v", err)
			}
			destinationGrant.ArchiveTo = "inline/{name}"
			f.auth.grants[to.ContentID] = destinationGrant
			id := uuid.NewString()
			copied, err := f.up.Commit(t.Context(), f.editor, to, id, []media.Op{operation})
			if err != nil {
				t.Fatal(err)
			}
			file, ok := copied.Get(operation.To)
			if !ok || !matchesBlob(file.Blob, body) || file.Admission == nil || file.Admission.Owner != destinationGrant.Owner || !file.Admission.Enforce || file.Admission.Generation != id {
				t.Fatalf("copy did not acquire destination admission: %+v", file)
			}
			if _, err := f.up.Commit(t.Context(), f.editor, to, id, []media.Op{operation}); err != nil {
				t.Fatalf("exact copy retry: %v", err)
			}
			if err := f.ms.IngestArchives(t.Context(), to, ""); err != nil {
				t.Fatal(err)
			}
			result, _, err := f.ms.Get(t.Context(), to)
			if err != nil {
				t.Fatal(err)
			}
			file, _ = result.Get(operation.To)
			if limited && (len(result.Files) != 1 || file.Fail() == nil) || !limited && (len(result.Files) != 3 || file.Fail() != nil) {
				t.Fatalf("destination quota was not enforced: %+v", result.Files)
			}
			for owner, expected := range map[string]int64{sourceGrant.Owner: original.UploadBytes(), destinationGrant.Owner: result.UploadBytes()} {
				used, pending, err := ledger.Usage(t.Context(), to.TenantID, owner)
				if err != nil || used != expected || pending != 0 {
					t.Fatalf("owner %s charged %d, expected %d; pending=%d err=%v", owner, used, expected, pending, err)
				}
			}
		})
	}
}

func TestRemovedArchiveCanBeReaddedFromRetainedAlias(t *testing.T) {
	f, _ := archiveFixture(t, 8<<20)
	ref := f.ref("post", 9)
	f.visible(9)
	p, blob := f.upload(ref, "zip/book", "application/zip", archiveBytes(t, map[string][]byte{"1.png": archivePNG(t, 16)}))
	f.commit(ref, media.Op{Op: media.OpPut, Path: p, Blob: blob})
	if err := f.ms.IngestArchives(t.Context(), ref, ""); err != nil {
		t.Fatal(err)
	}
	m, _, err := f.ms.Get(t.Context(), ref)
	if err != nil {
		t.Fatal(err)
	}
	first, _ := m.Get(p)
	f.commit(ref, media.Op{Op: media.OpPut, Path: "zip/alias.zip", Blob: first.Blob}, media.Op{Op: media.OpRemove, Path: p})
	m = f.commit(ref, media.Op{Op: media.OpPut, Path: p, Blob: first.Blob})
	reattached, _ := m.Get(p)
	if reattached.Admission.Generation == first.Admission.Generation {
		t.Fatal("re-adding retained bytes reused a retired source admission")
	}
	for range 2 {
		if err := f.ms.IngestArchives(t.Context(), ref, ""); err != nil {
			t.Fatal(err)
		}
	}
	m, _, err = f.ms.Get(t.Context(), ref)
	if err != nil {
		t.Fatal(err)
	}
	reattached, _ = m.Get(p)
	if reattached.Meta["archive_completed_sha256"] == nil || len(m.Files) != 3 {
		t.Fatalf("re-added source stayed pending or duplicated pages: %+v", m.Files)
	}
}

func TestCopyReplacesStagedArchiveWithoutKeepingUnusedPages(t *testing.T) {
	f, _ := archiveFixture(t, 8<<20)
	to, from := f.ref("post", 10), f.ref("post", 11)
	f.visible(10)
	f.visible(11)
	p, blob := f.upload(to, "zip/staged", "application/zip", archiveBytes(t, map[string][]byte{"1.png": archivePNG(t, 17), "2.png": archivePNG(t, 18)}))
	f.commit(to, media.Op{Op: media.OpPut, Path: p, Blob: blob, Unattached: true})
	if err := f.ms.IngestArchives(t.Context(), to, ""); err != nil {
		t.Fatal(err)
	}
	f.put(from, "zip/new", "application/zip", archiveBytes(t, map[string][]byte{"new.png": archivePNG(t, 19)}))
	m := f.commit(to, media.Op{Op: media.OpCopy, From: &media.CopyFrom{ID: from.ContentID, Path: "zip/new.zip"}, To: p})
	if len(m.Files) != 1 || m.Files[0].Path != p {
		t.Fatalf("copy replacement retained old staged pages: %+v", m.Files)
	}
	if err := f.ms.IngestArchives(t.Context(), to, ""); err != nil {
		t.Fatal(err)
	}
	m, _, err := f.ms.Get(t.Context(), to)
	if err != nil || len(m.Files) != 2 || slices.ContainsFunc(m.Files, func(file media.File) bool { return file.Unattached }) {
		t.Fatalf("copy replacement attached wrong batch: %+v / %v", m, err)
	}
}

func archiveFixture(t *testing.T, quota int64) (*fixture, *media.PGLimiter) {
	t.Helper()
	f := newFixtureOn(t, s3test.Open(t), func(c *media.Config) {
		for i := range c.Kinds {
			if c.Kinds[i].Name == "post" {
				c.Kinds[i].Uploads = append(c.Kinds[i].Uploads, media.Upload{Path: "zip/{name}", ArchiveTo: "inline/{name}", Types: []string{"application/zip"}, MaxBytes: 1 << 20})
			}
		}
	})
	f.auth.archiveTo = "inline/{name}"
	ledger, err := media.NewPGLimiter(f.env.Pool(), f.env.ContentSchema(), media.PGLimits{Quota: func(context.Context, string, string) (int64, error) { return quota, nil }})
	if err != nil {
		t.Fatal(err)
	}
	f.up, err = media.NewUploads(media.UploadOptions{Store: f.env.Store, Manifests: f.ms, Limiter: ledger})
	if err != nil {
		t.Fatal(err)
	}
	return f, ledger
}
func archivePNG(t *testing.T, n uint8) []byte {
	t.Helper()
	im := image.NewRGBA(image.Rect(0, 0, 2, 3))
	im.Set(0, 0, color.RGBA{R: n, A: 255})
	var b bytes.Buffer
	if err := pngimage.Encode(&b, im); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}
func archiveBytes(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var b bytes.Buffer
	z := zip.NewWriter(&b)
	for name, body := range files {
		f, err := z.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = f.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func archiveCount(value any) int {
	switch n := value.(type) {
	case int:
		return n
	case float64:
		return int(n)
	}
	return -1
}
