package critical_test

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/internal/media/workqueue"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/worker"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

// A worker killed mid-job leaves its scratch on disk; the next worker to
// start under the same TempDir removes it, and a worker that stops removes
// its own.
func TestMediaWorkerScratch(t *testing.T) {
	t.Run("CK-J11-S13_a_killed_job's_scratch_is_gone_after_a_restart", func(t *testing.T) {
		m := newResilientMedia(t, func(c *ck.Config) {
			gallery := &c.Media.Config.Kinds[0]
			gallery.Uploads = append(gallery.Uploads, media.Upload{Path: "import/{name}", Types: []string{"application/zip"}, MaxBytes: 8 << 20, Max: 1})
			gallery.Publish = &media.Publish{Pages: "pages/{name}", Renditions: []string{"reader"}, Import: "import/{name}"}
		})
		root := t.TempDir()
		client := m.app.engine.Client()
		ref := galleryRef(t, client, "Worker killed mid-archive")
		// The second extracted page's PUT waits: the ZIP and a page are on the
		// worker's disk.
		pages := 0
		hold := m.wire.pause(t, func(r *http.Request) bool {
			if r.Method != http.MethodPut || !strings.HasPrefix(r.Header.Get("Content-Type"), "image/png") {
				return false
			}
			pages++
			return pages == 2
		})
		child := m.start(t, func(c *mediaChildConfig) { c.TempDir = root })
		_, err := client.IngestMedia(t.Context(), nil, ref, uuid.NewString(), []ck.MediaFile{{Path: "import/pages.zip", Type: "application/zip", Body: bytes.NewReader(scratchArchive(t, 3))}})
		must(t, err)
		select {
		case <-hold.arrived:
		case <-time.After(30 * time.Second):
			t.Fatal("worker never reached the second page")
		}
		left := jobScratch(t, root)
		if len(left) == 0 {
			t.Fatal("the worker held no scratch mid-job")
		}
		queue := m.queue(t)
		running, err := queue.JobList(t.Context(), river.NewJobListParams().Kinds((workqueue.IngestArgs{}).Kind()).States(rivertype.JobStateRunning))
		must(t, err)
		if len(running.Jobs) != 1 {
			t.Fatalf("expected the archive's running ingest job, got %d", len(running.Jobs))
		}
		child.kill(t)
		if got := jobScratch(t, root); len(got) != len(left) {
			t.Fatalf("a killed process removed its own scratch: %v -> %v", left, got)
		}

		config := m.config
		config.TempDir = root
		restarted, err := worker.New(t.Context(), config)
		must(t, err)
		for _, path := range left {
			if _, err := os.Lstat(path); err == nil {
				t.Fatalf("the killed job's scratch survived a restart: %s", path)
			}
		}
		run, stop := context.WithCancel(context.Background())
		stopped := make(chan error, 1)
		go func() { stopped <- restarted.Run(run) }()
		t.Cleanup(stop)
		// The restarted worker finishes the killed job's archive.
		replayMediaJob(t, queue, running.Jobs[0])
		ready := awaitMediaReady(t, client, ref)
		if ready.Status == nil || ready.Status.Archives.Pages == nil || ready.Status.Archives.Pages.Completed != 3 {
			t.Fatalf("restarted worker did not finish the archive: %+v", ready.Status)
		}
		stop()
		select {
		case err := <-stopped:
			must(t, err)
		case <-time.After(20 * time.Second):
			t.Fatal("restarted worker did not stop")
		}
		if got := jobScratch(t, root); len(got) != 0 {
			t.Fatalf("a stopped worker left scratch: %v", got)
		}
	})
}

// jobScratch is every file and directory the worker's jobs left under root.
func jobScratch(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	must(t, filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		for _, prefix := range []string{"contentkit-archive-", "contentkit-master-", "ck-video-"} {
			if strings.HasPrefix(d.Name(), prefix) {
				out = append(out, path)
			}
		}
		return nil
	}))
	return out
}

func scratchArchive(t *testing.T, pages int) []byte {
	t.Helper()
	var body bytes.Buffer
	archive := zip.NewWriter(&body)
	for page := 1; page <= pages; page++ {
		img := image.NewNRGBA(image.Rect(0, 0, 64, 96))
		for i := range img.Pix {
			img.Pix[i] = uint8(i * page)
		}
		img.SetNRGBA(0, 0, color.NRGBA{A: 255})
		file, err := archive.Create(fmt.Sprintf("%03d.png", page))
		must(t, err)
		must(t, png.Encode(file, img))
	}
	must(t, archive.Close())
	return body.Bytes()
}
