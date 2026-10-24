package media

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"github.com/open-rails/contentkit/media/layout"
	"strings"
	"testing"
)

func statusBlob(value string) string {
	sum := sha256.Sum256([]byte(value))
	return layout.BlobName(sum[:], "00000000-0000-4000-8000-000000000001")
}

func TestStatusCountsWholeManifestAndArchiveEntries(t *testing.T) {
	k := &Kind{Publish: &Publish{Pages: "pages/{name}", Import: "import/{name}"}, Uploads: []Upload{{Path: "pages/{name}"}, {Path: "import/{name}"}}, Private: []Private{{Name: "read", From: "pages/{name}"}}, Public: []Public{{Name: "thumb", From: "pages/{name}"}}}
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
	got := statusOf(k, m, EncodeStatus{})
	if got.State != StateProcessing || got.Uploads != (ProcessingCount{Completed: 3, Total: 4, Failed: 1}) {
		t.Fatalf("uploads: %+v", got)
	}
	if got.Archives.Total != 1 || got.Archives.Completed != 0 || got.Archives.Pages == nil || *got.Archives.Pages != (ProcessingCount{Completed: 3, Total: 4}) {
		t.Fatalf("archive progress: %+v", got.Archives)
	}
	if len(got.Presets) != 2 || got.Presets[0].Name != "read" || got.Presets[1].Name != "thumb" {
		t.Fatalf("presets: %+v", got.Presets)
	}
	for _, preset := range got.Presets {
		if preset.ProcessingCount != (ProcessingCount{Completed: 1, Total: 3, Failed: 1}) {
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
