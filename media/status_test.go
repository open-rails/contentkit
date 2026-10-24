package media

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/open-rails/contentkit/media/layout"
)

func statusBlob(value string) string {
	sum := sha256.Sum256([]byte(value))
	return layout.BlobName(sum[:], "00000000-0000-4000-8000-000000000001")
}

// The unit fixtures use only path/preset selection, so compile the upload
// patterns that NewRegistry would normally install before status reads.
func compileStatusKind(t *testing.T, k *Kind) {
	t.Helper()
	k.patterns = make([]pattern, len(k.Uploads))
	for i, u := range k.Uploads {
		p, ok := parsePattern(u.Path)
		if !ok {
			t.Fatalf("invalid fixture upload pattern: %s", u.Path)
		}
		k.patterns[i] = p
	}
}

func TestStatusCountsWholeManifestAndArchiveEntries(t *testing.T) {
	k := &Kind{Publish: &Publish{Pages: "pages/{name}", Import: "import/{name}"}, Uploads: []Upload{{Path: "pages/{name}"}, {Path: "import/{name}"}}, Private: []Private{{Name: "read", From: "pages/{name}"}}, Public: []Public{{Name: "thumb", From: "pages/{name}"}}}
	compileStatusKind(t, k)
	zip := File{Path: "import/book.zip", Type: "application/zip", Blob: statusBlob("zip"), Meta: map[string]any{"archive_pages_total": float64(4)}}
	digest, _ := layout.BlobDigest(zip.Blob)
	meta := func() map[string]any {
		return map[string]any{"page": map[string]any{"archive_sha256": hex.EncodeToString(digest)}}
	}
	first := File{Path: "pages/1.png", Type: "image/png", Blob: statusBlob("first"), Meta: meta(), Public: []Publication{{Preset: "thumb", State: PublicationReady}}}
	second := File{Path: "pages/2.png", Type: "image/png", Blob: statusBlob("second"), Meta: meta(), Pending: []string{"read", "thumb"}}
	failed := File{Path: "pages/3.png", Type: "image/png", Blob: statusBlob("failed"), Meta: meta()}
	failed.Failed = NewFailure(failed, &ImageError{Code: CodeImageUnreadable, Message: "secret worker URL: https://private.example/?token=secret"})
	ignored := File{Path: "pages/unused.png", Type: "image/png", Unattached: true, Pending: []string{"read", "thumb"}}
	m := &Manifest{Files: []File{zip, first, second, failed, ignored, {Path: "read/1.webp", From: first.Path, Preset: "read", Blob: statusBlob("read")}}}
	m.Files[1].Public[0].Source = first.Key()
	m.Files[1].Public[0].Names = k.PublicNames(m, &k.Public[0], first.Path)
	got := statusOf(k, m, EncodeStatus{})
	if got.State != StateProcessing || got.Uploads != (ProcessingCount{Completed: 3, Total: 5, Failed: 1}) {
		t.Fatalf("uploads: %+v", got)
	}
	if got.Archives.Total != 1 || got.Archives.Completed != 0 || got.Archives.Pages == nil || *got.Archives.Pages != (ProcessingCount{Completed: 3, Total: 4}) {
		t.Fatalf("archive progress: %+v", got.Archives)
	}
	if len(got.Presets) != 2 || got.Presets[0].Name != "read" || got.Presets[1].Name != "thumb" {
		t.Fatalf("presets: %+v", got.Presets)
	}
	for _, preset := range got.Presets {
		total := 3
		if preset.Name == "read" {
			total = 4
		}
		if preset.ProcessingCount != (ProcessingCount{Completed: 1, Total: total, Failed: 1}) {
			t.Fatalf("preset counts: %+v", preset)
		}
	}
	body, _ := json.Marshal(got)
	if got.FailureCount != 1 || len(got.Failures) != 1 || strings.Contains(string(body), "secret") || strings.Contains(string(body), "private.example") {
		t.Fatalf("unsanitized failure: %s", body)
	}
	m.Hidden = true
	hidden := statusOf(k, m, EncodeStatus{})
	if len(hidden.Presets) != 1 || hidden.Presets[0].Name != "read" {
		t.Fatalf("hidden public preset counted: %+v", hidden.Presets)
	}
}

func TestStatusBoundsFailuresAndKeepsRealEncodingProgress(t *testing.T) {
	k := &Kind{Uploads: []Upload{{Path: "video"}}, Private: []Private{{Name: "stream", From: "video"}}}
	compileStatusKind(t, k)
	m := &Manifest{}
	for range 40 {
		f := File{Path: "video.mp4", Type: "video/mp4", Blob: statusBlob("failed")}
		f.Failed = NewFailure(f, &ImageError{Code: CodeVideoTooLarge, Message: "decoder internals"})
		m.Files = append(m.Files, f)
	}
	got := statusOf(k, m, EncodeStatus{})
	if got.State != StateFailed || got.FailureCount != 40 || len(got.Failures) != 32 {
		t.Fatalf("failure bounds: %+v", got)
	}
	m.Files = []File{{Path: "video.mp4", Type: "video/mp4", Blob: statusBlob("video"), Pending: []string{"stream"}}}
	checkpoint := EncodeProgress{Phase: PhaseEncoding, SegmentsDone: 7, SegmentsTotal: 30, Percent: 23}
	got = statusOf(k, m, EncodeStatus{Files: map[string]EncodeProgress{"video.mp4": checkpoint}})
	if got.State != StateProcessing || got.Encoding == nil || *got.Encoding != checkpoint {
		t.Fatalf("checkpoint changed: %+v", got)
	}
	m.Full = true
	if full := statusOf(k, m, EncodeStatus{}); full.State != StateFull {
		t.Fatalf("full processing state lost: %+v", full)
	}
}

func TestArchiveStatusKeepsCompletedExtractionAndCountsExcludedPagesOnce(t *testing.T) {
	k := &Kind{Publish: &Publish{Pages: "pages/{name}", Import: "import/{name}"}, Uploads: []Upload{{Path: "pages/{name}"}, {Path: "import/{name}"}}}
	compileStatusKind(t, k)
	archive := File{Path: "import/book.zip", Blob: statusBlob("zip"), Meta: map[string]any{"archive_pages_total": 3}}
	raw, _ := layout.BlobDigest(archive.Blob)
	sum := hex.EncodeToString(raw)
	page := func(path string, index int, unattached bool) File {
		return File{Path: path, Blob: statusBlob(path), Unattached: unattached, Meta: map[string]any{"page": map[string]any{"archive_sha256": sum, "source_index": index}}}
	}
	m := &Manifest{Files: []File{archive, page("pages/1.png", 1, false), page("pages/2.png", 2, true), page("pages/duplicate.png", 2, false)}}
	got := statusOf(k, m, EncodeStatus{})
	if got.Archives.Pages == nil || got.Archives.Pages.Completed != 2 || got.Archives.Pages.Total != 3 {
		t.Fatalf("excluded or duplicate pages miscounted: %+v", got.Archives)
	}
	m.Files[0].Meta["archive_completed_sha256"] = sum
	m.Files = m.Files[:1]
	got = statusOf(k, m, EncodeStatus{})
	if got.Archives.Pages.Completed != 3 || got.Archives.Completed != 1 {
		t.Fatalf("QA removals regressed completed extraction: %+v", got.Archives)
	}
	m.Files[0].Blob = statusBlob("replacement zip")
	got = statusOf(k, m, EncodeStatus{})
	if got.Archives.Pages.Completed != 0 || got.Archives.Completed != 0 {
		t.Fatalf("old completion applied to replacement ZIP: %+v", got.Archives)
	}
}

func TestStatusRejectsUnownedPublicGenerationAndPendingPrivateOutput(t *testing.T) {
	k := &Kind{Uploads: []Upload{{Path: "pages/{name}"}}, Private: []Private{{Name: "read", From: "pages/{name}"}}, Public: []Public{{Name: "thumb", From: "pages/{name}"}}}
	compileStatusKind(t, k)
	source := File{Path: "pages/1.png", Blob: statusBlob("current"), Pending: []string{"read"}, Public: []Publication{{Preset: "thumb", Source: "old-source", State: PublicationReady}}}
	m := &Manifest{Files: []File{source, {Path: "read/1.webp", From: source.Path, Preset: "read", Blob: statusBlob("old output")}}}
	got := statusOf(k, m, EncodeStatus{})
	if len(got.Presets) != 2 {
		t.Fatalf("expected both rendition presets: %+v", got.Presets)
	}
	for _, preset := range got.Presets {
		if preset.Completed != 0 {
			t.Fatalf("stale/pending output counted ready: %+v", preset)
		}
	}
}

func TestStatusTracksGenericUnattachedArchiveAndPrivateRenditions(t *testing.T) {
	k := &Kind{Uploads: []Upload{{Path: "zip/{name}", ArchiveTo: "body/{name}"}, {Path: "body/{name}"}}, Private: []Private{{Name: "read", From: "body/{name}"}}, Public: []Public{{Name: "thumb", From: "body/{name}"}}}
	compileStatusKind(t, k)
	archive := File{Path: "zip/book.zip", Type: "application/zip", Blob: statusBlob("generic ZIP"), Unattached: true, Meta: map[string]any{"archive_pages_total": 2}}
	raw, _ := layout.BlobDigest(archive.Blob)
	sum := hex.EncodeToString(raw)
	page := func(name string, index int) File {
		return File{Path: "body/" + name + ".png", Type: "image/png", Blob: statusBlob(name), Unattached: true, Pending: []string{"read", "thumb"}, Meta: map[string]any{"page": map[string]any{"archive_sha256": sum, "source_index": index}}}
	}
	m := &Manifest{Files: []File{archive, page("one", 1)}}
	got := statusOf(k, m, EncodeStatus{})
	if k.Readiness(m).State != StateReady || got.State != StateProcessing || got.Archives.Total != 1 || got.Archives.Completed != 0 || got.Archives.Pages == nil || got.Archives.Pages.Completed != 1 || got.Archives.Pages.Total != 2 {
		t.Fatalf("generic staged archive work lost: %+v", got)
	}
	m.Files = append(m.Files, page("two", 2))
	m.Files[0].Meta["archive_completed_sha256"] = sum
	got = statusOf(k, m, EncodeStatus{})
	if got.Archives.Completed != 1 || got.Archives.Pages.Completed != 2 || got.State != StateProcessing || len(got.Presets) != 1 || got.Presets[0].Name != "read" || got.Presets[0].Total != 2 || got.Presets[0].Completed != 0 {
		t.Fatalf("private work vs deferred public presets: %+v", got)
	}
	for i := 1; i <= 2; i++ {
		m.Files[i].Pending = []string{"thumb"}
		m.Files = append(m.Files, File{Path: "read/" + m.Files[i].Path, From: m.Files[i].Path, Preset: "read", Blob: statusBlob("output" + m.Files[i].Path)})
	}
	got = statusOf(k, m, EncodeStatus{})
	if k.Readiness(m).State != StateReady || got.State != StateReady || len(got.Presets) != 1 || got.Presets[0].Completed != 2 {
		t.Fatalf("unattached public presets incorrectly keep staging pending: %+v", got)
	}
	// Attachment makes public rendering relevant, independently of ZIP/source work.
	m.Files[1].Unattached = false
	got = statusOf(k, m, EncodeStatus{})
	if k.Readiness(m).State != StateProcessing || got.State != StateProcessing || len(got.Presets) != 2 || got.Presets[1].Name != "thumb" || got.Presets[1].Total != 1 || got.Presets[1].Completed != 0 {
		t.Fatalf("attached public work missing: %+v", got)
	}
}

func TestStatusCountsUnattachedPlacementAndFailureWithoutChangingReadiness(t *testing.T) {
	k := &Kind{Uploads: []Upload{{Path: "body/{name}"}}}
	compileStatusKind(t, k)
	m := &Manifest{Files: []File{{Path: "body/new.png", Type: "image/png", Unattached: true, Staged: "pending"}}}
	got := statusOf(k, m, EncodeStatus{})
	if k.Readiness(m).State != StateReady || got.State != StateProcessing || got.Uploads.Total != 1 || got.Uploads.Completed != 0 {
		t.Fatalf("staged placement lost: %+v", got)
	}
	m.Files[0].Failed = NewFailure(m.Files[0], &ImageError{Code: CodeImageUnreadable, Message: "worker internals"})
	got = statusOf(k, m, EncodeStatus{})
	if k.Readiness(m).State != StateReady || got.State != StateFailed || got.Uploads.Failed != 1 || got.FailureCount != 1 {
		t.Fatalf("staged failure lost: %+v", got)
	}
}

func TestStatusCountsArchivePagesOnlyInConfiguredDestination(t *testing.T) {
	k := &Kind{Uploads: []Upload{{Path: "zip/{name}", ArchiveTo: "body/{name}"}, {Path: "body/{name}"}, {Path: "other/{name}"}}}
	compileStatusKind(t, k)
	archive := File{Path: "zip/book.zip", Blob: statusBlob("shared ZIP"), Meta: map[string]any{"archive_pages_total": 2}}
	raw, _ := layout.BlobDigest(archive.Blob)
	sum := hex.EncodeToString(raw)
	page := func(path string, index int) File {
		return File{Path: path, Blob: statusBlob(path), Unattached: true, Meta: map[string]any{"page": map[string]any{"archive_sha256": sum, "source_index": index}}}
	}
	m := &Manifest{Files: []File{archive, page("body/one.png", 1), page("other/two.png", 2)}}
	got := statusOf(k, m, EncodeStatus{})
	if got.Archives.Pages == nil || got.Archives.Pages.Completed != 1 || got.Archives.Pages.Total != 2 {
		t.Fatalf("unrelated destination inflated archive progress: %+v", got.Archives)
	}
}

func TestStatusDefersUnattachedPublicEncoding(t *testing.T) {
	k := &Kind{Uploads: []Upload{{Path: "video"}}, Private: []Private{{Name: "stream", From: "video"}}, Public: []Public{{Name: "preview", From: "video"}}}
	compileStatusKind(t, k)
	video := File{Path: "video.mp4", Type: "video/mp4", Blob: statusBlob("staged video"), Unattached: true, Pending: []string{"stream", "preview"}}
	m := &Manifest{Files: []File{video}}
	checkpoint := EncodeProgress{Phase: PhaseEncoding, SegmentsDone: 7, SegmentsTotal: 30, Percent: 23}
	progress := EncodeStatus{Files: map[string]EncodeProgress{video.Path: checkpoint}}
	got := statusOf(k, m, progress)
	if k.Readiness(m).State != StateReady || got.State != StateProcessing || got.Encoding == nil || *got.Encoding != checkpoint {
		t.Fatalf("staged encoding checkpoint missing: %+v", got)
	}
	m.Files[0].Pending = []string{"preview"}
	m.Files = append(m.Files, File{Path: "stream/video.m3u8", From: video.Path, Preset: "stream", Blob: statusBlob("stream")})
	got = statusOf(k, m, progress)
	if got.State != StateReady || got.Encoding != nil || len(got.Presets) != 1 || got.Presets[0].Completed != 1 {
		t.Fatalf("deferred public work kept a stale encoding checkpoint: %+v", got)
	}
}
