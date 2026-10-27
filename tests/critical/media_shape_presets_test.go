package critical_test

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/worker"
	"github.com/open-rails/contentkit/media/workqueue"
)

var (
	shapeBase = media.Image{Width: 48, Height: 48}
	shapeTall = media.Image{Width: 48, Height: 96}
	shapeWide = media.Image{Width: 96, Height: 40}
	// Tall pages keep more height and wide pages more width, as a reader's
	// low-resolution preset does.
	readerShapes = []media.Shape{{NoWiderThan: media.Ratio("1:2"), Image: shapeTall}, {WiderThan: media.Square, Image: shapeWide}}
)

func TestImageShapePresets(t *testing.T) {
	t.Run("CK-J09-S31_registry_validates_shapes_and_unmeasured_uploads_use_the_preset_image", func(t *testing.T) {
		kind := func(p media.Private) media.Config {
			return media.Config{Namespace: "shapes", Kinds: []media.Kind{{Name: "gallery",
				Uploads: []media.Upload{{Path: "pages/{name}", Types: []string{"image/png"}, MaxBytes: 1 << 20}},
				Private: []media.Private{p}}}}
		}
		reader := func(shapes ...media.Shape) media.Private {
			return media.Private{Name: "reader", From: "pages/{name}", To: "reader/{name}.webp", Image: &shapeBase, Shapes: shapes}
		}
		registry, err := media.NewRegistry(kind(reader(readerShapes...)))
		must(t, err)
		gallery, err := registry.Kind("gallery")
		must(t, err)
		if got := gallery.Private[0].ImageFor(media.Dims{}); got != shapeBase {
			t.Fatalf("an unmeasured upload rendered %+v, not the preset image", got)
		}
		_, err = media.NewRegistry(kind(reader(media.Shape{NoWiderThan: media.Square, Image: shapeTall}, media.Shape{WiderThan: media.Square, Image: shapeWide})))
		must(t, err) // touching ranges: square belongs to the first
		source := shapeTall
		source.Format = media.ImageFormatSource
		still := shapeTall
		still.Animation = media.AnimationReject
		invalid := map[string]media.Config{
			"overlap":           kind(reader(media.Shape{NoWiderThan: media.Square, Image: shapeTall}, media.Shape{WiderThan: media.Ratio("1:2"), Image: shapeWide})),
			"unbounded":         kind(reader(media.Shape{Image: shapeTall})),
			"empty range":       kind(reader(media.Shape{WiderThan: media.Ratio("2:1"), NoWiderThan: media.Square, Image: shapeTall})),
			"unreduced bound":   kind(reader(media.Shape{NoWiderThan: media.Aspect{W: 2, H: 4}, Image: shapeTall})),
			"other format":      kind(reader(media.Shape{NoWiderThan: media.Ratio("1:2"), Image: source})),
			"other animation":   kind(reader(media.Shape{NoWiderThan: media.Ratio("1:2"), Image: still})),
			"invalid image":     kind(reader(media.Shape{NoWiderThan: media.Ratio("1:2"), Image: media.Image{Width: -1}})),
			"no image producer": kind(media.Private{Name: "pages", Zip: "reader/", To: "pages.zip", Shapes: readerShapes}),
		}
		for name, config := range invalid {
			if _, err := media.NewRegistry(config); err == nil {
				t.Errorf("%s: registry accepted invalid shapes", name)
			}
		}
	})
	t.Run("CK-J09-S31_stock_worker_renders_tall_wide_and_square_pages_by_shape_from_registry_json", func(t *testing.T) {
		m := newResilientMedia(t, func(c *ck.Config) {
			c.Media.Kinds[0].Private[0].Image = &shapeBase
			c.Media.Kinds[0].Private[0].Shapes = readerShapes
		})
		// The registry as `contentkit media-config` prints it and
		// cmd/media-worker loads it from MEDIA_KINDS_FILE.
		printed, err := json.MarshalIndent(m.config.Kinds, "", "  ")
		must(t, err)
		var compact bytes.Buffer
		must(t, json.Compact(&compact, printed))
		if !strings.Contains(compact.String(), `"image":{"width":48,"height":48},"shapes":[{"no_wider_than":"1:2","image":{"width":48,"height":96}},{"wider_than":"1:1","image":{"width":96,"height":40}}]`) {
			t.Fatalf("registry JSON lacks the reader shapes: %s", compact.String())
		}
		parsed, err := media.ParseConfig(printed)
		must(t, err)
		registry, err := media.NewRegistry(parsed)
		must(t, err)
		gallery, err := registry.Kind("gallery")
		must(t, err)
		if !slices.Equal(gallery.Private[0].Shapes, readerShapes) {
			t.Fatalf("registry JSON changed the shapes: %+v", gallery.Private[0].Shapes)
		}
		config := m.config
		config.Kinds = registry
		w, err := worker.New(t.Context(), config)
		must(t, err)
		run, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- w.Run(run) }()
		t.Cleanup(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("stop JSON-registry worker: %v", err)
				}
			case <-time.After(20 * time.Second):
				t.Error("JSON-registry worker did not stop")
			}
		})

		client := m.app.engine.Client()
		ref := galleryRef(t, client, "Shaped pages")
		pages := []struct {
			name       string
			w, h       int
			outW, outH int
		}{
			{"tall", 50, 100, 48, 96},     // exactly 1:2 is tall
			{"portrait", 75, 100, 36, 48}, // between: the preset image
			{"square", 100, 100, 48, 48},  // square is not wide
			{"wide", 120, 60, 80, 40},
		}
		var files []ck.MediaFile
		for _, p := range pages {
			files = append(files, ck.MediaFile{Path: "pages/" + p.name + ".png", Type: "image/png", Body: bytes.NewReader(shapePage(t, p.w, p.h))})
		}
		_, err = client.IngestMedia(t.Context(), nil, ref, uuid.NewString(), files)
		must(t, err)
		queue := m.queue(t)
		waitForWithin(t, 45*time.Second, func() bool {
			read, err := client.GetMediaVersion(t.Context(), ref.Kind, ref.ID, ref.Version, ck.GetMediaVersionQuery{Status: true})
			must(t, err)
			if read.Status != nil && read.Status.FailureCount > 0 {
				t.Fatalf("shaped pages failed: %+v", read.Status.Failures)
			}
			if read.Status == nil || read.Status.State != media.StateReady {
				return false
			}
			jobs, err := queue.JobList(t.Context(), river.NewJobListParams().Kinds((workqueue.DerivativeArgs{}).Kind()))
			must(t, err)
			for _, job := range jobs.Jobs {
				if job.State != rivertype.JobStateCompleted {
					if job.State == rivertype.JobStateCancelled || job.State == rivertype.JobStateDiscarded || len(job.Errors) > 0 {
						t.Fatalf("image job failed: %s %+v", job.State, job.Errors)
					}
					return false
				}
			}
			return len(jobs.Jobs) > 0
		})
		// Each page renders once, at its shape: an unmeasured upload is not
		// first rendered with the preset image and then again.
		m.wire.mu.Lock()
		writes := m.wire.webpPuts
		m.wire.mu.Unlock()
		if writes != len(pages) {
			t.Fatalf("worker wrote %d WebP renditions for %d pages", writes, len(pages))
		}

		logical, err := registry.VersionRef(ref.Kind, ref.ID, ref.Version)
		must(t, err)
		item, err := registry.Item(logical)
		must(t, err)
		journal, err := media.NewPGJournal(m.pool, m.schema, nil)
		must(t, err)
		manifests, err := media.NewManifests(m.storage.store, registry, media.ManifestOptions{Locker: media.PGLocker(m.pool), Journal: journal})
		must(t, err)
		manifest, _, err := manifests.Get(t.Context(), logical)
		must(t, err)
		for _, p := range pages {
			upload, ok := manifest.Get("pages/" + p.name + ".png")
			if !ok || upload.W != p.w || upload.H != p.h || upload.Fail() != nil {
				t.Fatalf("%s upload measured %+v", p.name, upload)
			}
			out, ok := manifest.Get("reader/" + p.name + ".webp")
			if !ok || out.W != p.outW || out.H != p.outH {
				t.Fatalf("%s rendition = %+v, want %dx%d", p.name, out, p.outW, p.outH)
			}
			key, err := item.Blob(out.Blob)
			must(t, err)
			body, _, err := m.storage.store.Get(t.Context(), key, media.GetOptions{})
			must(t, err)
			data, err := io.ReadAll(body)
			body.Close()
			must(t, err)
			decoded, format, err := image.DecodeConfig(bytes.NewReader(data))
			must(t, err)
			if format != "webp" || decoded.Width != p.outW || decoded.Height != p.outH {
				t.Fatalf("%s stored rendition = %s %dx%d, want webp %dx%d", p.name, format, decoded.Width, decoded.Height, p.outW, p.outH)
			}
		}
	})
}

func shapePage(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.SetNRGBA(x, y, color.NRGBA{R: uint8(x * 2), G: uint8(y * 2), B: 90, A: 255})
		}
	}
	var out bytes.Buffer
	must(t, png.Encode(&out, img))
	return out.Bytes()
}
