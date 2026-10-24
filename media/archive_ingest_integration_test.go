package media_test

import (
	"archive/zip"
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
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
	used, pending, err := ledger.Usage(ctx, ref.TenantID, "owner")
	if err != nil || used != int64(len(source)+len(a)+len(b)) || pending != 0 {
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
	if updated.Meta["archive_completed_sha256"] != first.Meta["archive_completed_sha256"] || updated.Meta["caption"] != "changed" {
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
	for path, blob := range pages {
		file, ok := m.Get(path)
		if !ok || file.Blob != blob {
			t.Fatal("replay changed retained page identity")
		}
	}
	now, _, err := ledger.Usage(ctx, ref.TenantID, "owner")
	if err != nil || now != used+int64(len(source)) {
		t.Fatalf("replay growth=%d want=%d err=%v", now, used+int64(len(source)), err)
	}
}

func TestArchiveQuotaRefusesBeforeAnyDecodedBlobWrite(t *testing.T) {
	source := archiveBytes(t, map[string][]byte{"1.png": archivePNG(t, 3), "2.png": archivePNG(t, 4)})
	f, ledger := archiveFixture(t, int64(len(source)+1))
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
	if err != nil || used != int64(len(source)) || pending != 0 {
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
	if err := png.Encode(&b, im); err != nil {
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
