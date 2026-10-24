package versions_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/versions"
)

type holdReaderProducers struct {
	river.HookDefaults
	images, video, releaseImages chan struct{}
	imageStarted, videoStarted   sync.Once
	imageReleased                sync.Once
}

func (h *holdReaderProducers) WorkBegin(ctx context.Context, job *rivertype.JobRow) error {
	switch job.Kind {
	case "contentkit_media_image":
		h.imageStarted.Do(func() { close(h.images) })
		select {
		case <-h.releaseImages:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	case "contentkit_media_video":
		h.videoStarted.Do(func() { close(h.video) })
		<-ctx.Done() // The test keeps video pending until worker shutdown.
		return ctx.Err()
	}
	return nil
}

func (h *holdReaderProducers) allowImages() {
	h.imageReleased.Do(func() { close(h.releaseImages) })
}

type holdPublicationCompletion struct {
	river.HookDefaults
	armed   atomic.Bool
	entered chan int64
	release chan struct{}
	once    sync.Once
}

func (h *holdPublicationCompletion) WorkEnd(ctx context.Context, job *rivertype.JobRow, err error) error {
	if job.Kind != "contentkit_version_publish" || !h.armed.CompareAndSwap(true, false) {
		return err
	}
	h.entered <- job.ID
	select {
	case <-h.release:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (h *holdPublicationCompletion) allowCompletion() {
	h.once.Do(func() { close(h.release) })
}

// Real workers place both sources, but image/video processing is held. An early
// snapshot attempt must finish instead of polling. Image completion then wakes
// the reader even while video is pending, including when a publisher is running.
func TestReaderSnapshotWakesWhileVideoAndPublicationAreHeld(t *testing.T) {
	producers := &holdReaderProducers{images: make(chan struct{}), video: make(chan struct{}), releaseImages: make(chan struct{})}
	publisher := &holdPublicationCompletion{entered: make(chan int64, 1), release: make(chan struct{})}
	s := newSite(t, siteOptions{
		mediaGallery: func(k *media.Kind) {
			k.Uploads = append(k.Uploads, media.Upload{Path: "clip", Types: []string{"video/mp4"}, MaxBytes: 1 << 20})
			k.Private = append(k.Private, media.Private{Name: "clip-hls", From: "clip", To: "clip-hls/", HLS: &media.HLS{Ladder: []int{240}}})
		},
		hostHooks: []rivertype.Hook{publisher}, workerHooks: []rivertype.Hook{producers}, shutdownGrace: 100 * time.Millisecond,
	})
	t.Cleanup(func() {
		producers.allowImages()
		publisher.allowCompletion()
		if t.Failed() {
			s.dumpJobs()
		}
	})
	v := s.version(s.content("gallery", `{"level":"public"}`))
	ref := s.ref("gallery", v)
	stage := func(path, contentType string, body []byte) media.Op {
		t.Helper()
		sum := sha256.Sum256(body)
		p, err := s.uploads.Presign(s.ctx, s.creator, media.PresignRequest{Ref: ref, Path: path, Type: contentType, Size: int64(len(body)), SHA256: sum[:]})
		if err != nil {
			t.Fatal(err)
		}
		req, err := http.NewRequest(p.Put.Method, p.Put.URL, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header = p.Put.Header.Clone()
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("PUT %s: %d", path, response.StatusCode)
		}
		return media.Op{Op: media.OpPut, Path: p.Path, Blob: p.Blob}
	}
	videoPath := filepath.Join(t.TempDir(), "clip.mp4")
	if output, err := exec.Command("ffmpeg", "-v", "error", "-nostdin", "-f", "lavfi", "-i", "color=c=blue:s=32x32:d=1", "-f", "lavfi", "-i", "sine=frequency=440:duration=1", "-c:v", "libx264", "-preset", "ultrafast", "-threads", "1", "-pix_fmt", "yuv420p", "-c:a", "aac", "-shortest", videoPath).CombinedOutput(); err != nil {
		t.Fatalf("video fixture: %v: %s", err, output)
	}
	video, err := os.ReadFile(videoPath)
	if err != nil {
		t.Fatal(err)
	}
	s.commit(s.creator, ref, stage("originals/001", "image/png", pngOf(t, 30, 40, 23)), stage("clip", "video/mp4", video))
	for name, entered := range map[string]<-chan struct{}{"image": producers.images, "video": producers.video} {
		select {
		case <-entered:
		case <-time.After(time.Minute):
			t.Fatalf("%s producer did not start", name)
		}
	}
	jobs := pgx.Identifier{s.host, "river_job"}.Sanitize()
	eventually(t, "not-ready snapshot attempts finish without polling", time.Minute, func() bool {
		var completed, active int
		if err := s.pool.QueryRow(s.ctx, `SELECT count(*) FILTER (WHERE kind='contentkit_version_publish' AND state='completed'),count(*) FILTER (WHERE finalized_at IS NULL)
FROM `+jobs+` WHERE kind IN ('contentkit_version_publish','contentkit_media_ready')`).Scan(&completed, &active); err != nil {
			t.Fatal(err)
		}
		return completed > 0 && active == 0
	})
	// Hold a new early publisher after it read the pending pages, while its
	// River row is still running. This is the InsertOnce follow-up boundary.
	publisher.armed.Store(true)
	s.commit(s.creator, ref, media.Op{Op: media.OpMeta, Meta: map[string]any{"note": "publication wakeup"}})
	var publishing int64
	select {
	case publishing = <-publisher.entered:
	case <-time.After(time.Minute):
		t.Fatal("publisher did not reach the completion boundary")
	}
	producers.allowImages()
	eventually(t, "image progress queues a publication follow-up", time.Minute, func() bool {
		var count int
		if err := s.pool.QueryRow(s.ctx, `SELECT count(*) FROM `+jobs+` WHERE kind='contentkit_version_publish' AND args->>'after'=$1 AND finalized_at IS NULL`, fmt.Sprint(publishing)).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count > 0
	})
	publisher.allowCompletion()
	s.plan(s.fan, v, func(plan *versions.ReaderPlan) bool { return plan.Pages != nil && *plan.Pages == 1 })
	clip, ok := s.manifest(ref).Get("clip.mp4")
	if !ok || len(clip.Pending) == 0 {
		t.Fatal("reader waited for the unrelated video to finish")
	}
}
