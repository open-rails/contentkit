package critical_test

import (
	"archive/zip"
	"bytes"
	"fmt"
	"image"
	"image/png"
	"io/fs"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/media"
)

// ZIP ingestion validates every entry in one streaming pass and extracts
// each page again as its batch is written, so the worker's scratch holds the
// ZIP and one page, never the expanded archive.
func TestArchiveIngestScratch(t *testing.T) {
	w := newWorld(t)
	a := w.apps[0]
	storage := attachMedia(t, w, a, func(c *ck.Config) {
		kind := &c.Media.Kinds[0]
		kind.Uploads[0].Max = 0
		kind.Uploads = append(kind.Uploads, media.Upload{Path: "import/{name}", Types: []string{"application/zip"}, MaxBytes: 1 << 20})
		kind.Publish = &media.Publish{Pages: "pages/{name}", Renditions: []string{"reader"}, Import: "import/{name}"}
	})
	// Uncompressed PNGs of one colour: large pages that deflate to almost nothing.
	const pages = 12
	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	var page int64
	for i := range pages {
		img := image.NewNRGBA(image.Rect(0, 0, 400, 380))
		for p := 0; p < len(img.Pix); p += 4 {
			img.Pix[p], img.Pix[p+1], img.Pix[p+2], img.Pix[p+3] = uint8(20*i), 90, 160, 255
		}
		var body bytes.Buffer
		must(t, (&png.Encoder{CompressionLevel: png.NoCompression}).Encode(&body, img))
		page = max(page, int64(body.Len()))
		file, err := writer.Create(fmt.Sprintf("%03d.png", i+1))
		must(t, err)
		_, err = file.Write(body.Bytes())
		must(t, err)
	}
	must(t, writer.Close())
	if expanded := page * pages; expanded < 20*int64(archive.Len()) {
		t.Fatalf("fixture ZIP of %d bytes expands to only %d", archive.Len(), expanded)
	}
	var peak atomic.Int64
	stop := make(chan struct{})
	var watching sync.WaitGroup
	watching.Go(func() {
		for {
			var used int64
			_ = filepath.WalkDir(storage.scratch, func(_ string, d fs.DirEntry, err error) error {
				if err == nil && !d.IsDir() {
					if info, err := d.Info(); err == nil {
						used += info.Size()
					}
				}
				return nil
			})
			if used > peak.Load() {
				peak.Store(used)
			}
			select {
			case <-stop:
				return
			case <-time.After(time.Millisecond):
			}
		}
	})
	admin := a.engine.Client()
	ref := galleryRef(t, admin, "Scratch-bounded archive")
	_, err := admin.IngestMedia(t.Context(), http.DefaultClient, ref, uuid.NewString(), []ck.MediaFile{
		{Path: "import/pages.zip", Type: "application/zip", Body: bytes.NewReader(archive.Bytes())}})
	must(t, err)
	ready := awaitMediaReady(t, admin, ref)
	close(stop)
	watching.Wait()
	imported := 0
	for _, file := range ready.Files {
		if file.Upload && strings.HasPrefix(file.Path, "pages/") {
			imported++
		}
	}
	if imported != pages {
		t.Fatalf("ingested %d of %d pages", imported, pages)
	}
	if limit := int64(archive.Len()) + 2*page; peak.Load() > limit {
		t.Fatalf("ingest scratch peaked at %d bytes: over the ZIP (%d) and two %d-byte pages", peak.Load(), archive.Len(), page)
	}
}
