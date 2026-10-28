package critical_test

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	mediastore "github.com/open-rails/contentkit/internal/mediastore"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/media/worker"
	"github.com/open-rails/contentkit/internal/media/workqueue"
	"github.com/open-rails/contentkit/media"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

// Refuse actual object writes after successful work, then regenerate through
// the public API. Both queue exhaustion and persisted media state are observed.
func TestMediaVideoFailureRecovery(t *testing.T) {
	for _, mode := range []string{"video", "mp4", "subtitle", "poster", "image-before-video", "shutdown"} {
		t.Run(mode, func(t *testing.T) {
			m := newResilientMedia(t, func(c *ck.Config) {
				k := &c.Media.Config.Kinds[0]
				k.Uploads = append(k.Uploads, media.Upload{Path: "source", Types: []string{"video/mp4"}, MaxBytes: 10 << 20})
				switch mode {
				case "mp4":
					k.Private = append(k.Private, media.Private{Name: "download", From: "source", To: "download.mp4", MP4: media.Rung(144)})
				case "subtitle":
					k.Uploads = append(k.Uploads, media.Upload{Path: "subs/{name}", Types: []string{"application/x-subrip"}, MaxBytes: 1 << 20})
					k.Private = append(k.Private, media.Private{Name: "captions", From: "subs/{name}", To: "captions/{name}.vtt", Subtitles: &media.Subtitles{}})
				case "poster":
					k.Uploads = append(k.Uploads, media.Upload{Path: "poster", Types: []string{"image/png"}, MaxBytes: 1 << 20, Frames: "source"})
					k.Private = append(k.Private, media.Private{Name: "poster", From: "poster", To: "posters/preview.webp", Image: &media.Image{Width: 48, Height: 48}})
				default:
					k.Private = append(k.Private,
						media.Private{Name: "stream", From: "source", To: "streams/", HLS: &media.HLS{Ladder: []int{240, 144}}},
						media.Private{Name: "download", From: "source", To: "download.mp4", MP4: media.Rung(144)})
				}
			})
			client := m.app.engine.Client()
			ref := galleryRef(t, client, "Exhausted "+mode)
			source := filepath.Join(t.TempDir(), "source.mp4")
			command := exec.CommandContext(t.Context(), "ffmpeg", "-nostdin", "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=426x240:rate=12", "-t", "1", "-an", "-c:v", "libx264", "-threads", "1", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-movflags", "+faststart", source)
			if out, err := command.CombinedOutput(); err != nil {
				t.Fatalf("create source: %v %s", err, out)
			}
			video, err := os.ReadFile(source)
			must(t, err)
			files := []ck.MediaFile{{Path: "pages/one.png", Type: "image/png", Body: bytes.NewReader(imageBytes(t))}, {Path: "source.mp4", Type: "video/mp4", Body: bytes.NewReader(video)}}
			if mode == "subtitle" {
				files = append(files, ck.MediaFile{Path: "subs/en.srt", Type: "application/x-subrip", Body: strings.NewReader("1\n00:00:00,000 --> 00:00:00,900\nHello\n")})
			}
			_, err = client.IngestMedia(t.Context(), nil, ref, uuid.NewString(), files)
			must(t, err)
			if mode == "poster" {
				_, err = client.PostMediaUploadCommit(t.Context(), media.CommitBody{Ref: ref, OperationID: uuid.NewString(), Ops: []media.Op{{Op: media.OpFrame, Path: "poster", Auto: true}}})
				must(t, err)
			}
			if mode == "shutdown" {
				hold := m.wire.pause(t, func(r *http.Request) bool {
					return r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/internal/") && strings.HasPrefix(r.Header.Get("Content-Type"), "video/mp4")
				})
				stop := startVideoFailureWorker(t, m)
				select {
				case <-hold.arrived:
				case <-time.After(30 * time.Second):
					t.Fatal("video worker did not reach the held output")
				}
				stop()
				read, err := client.GetMediaVersion(t.Context(), ref.Kind, ref.ID, ref.Version, ck.GetMediaVersionQuery{Status: true})
				must(t, err)
				if read.Status == nil || read.Status.State != media.StateProcessing || read.Status.FailureCount != 0 {
					t.Fatalf("shutdown marked video failed: %+v", read.Status)
				}
				hold.unblock()
				startVideoFailureWorker(t, m)
				awaitVideoRecovery(t, client, ref)
				jobs, err := m.queue(t).JobList(t.Context(), river.NewJobListParams().Kinds((workqueue.DerivativeArgs{}).Kind()))
				must(t, err)
				for _, job := range jobs.Jobs {
					if len(job.Errors) != 0 {
						t.Fatalf("shutdown consumed a failure attempt: %+v", job.Errors)
					}
				}
				return
			}

			var videoPuts int
			m.wire.mu.Lock()
			m.wire.refuseOutput = func(r *http.Request) bool {
				if r.Method != http.MethodPut || !strings.Contains(r.URL.Path, "/internal/") {
					return false
				}
				kind := r.Header.Get("Content-Type")
				switch mode {
				case "video":
					if strings.HasPrefix(kind, "video/mp4") {
						videoPuts++
						return videoPuts > 2
					}
				case "mp4":
					return strings.HasPrefix(kind, "video/mp4")
				case "subtitle":
					return strings.HasPrefix(kind, "text/vtt")
				case "poster":
					return strings.HasPrefix(kind, "image/png")
				case "image-before-video":
					return strings.HasPrefix(kind, "image/webp")
				}
				return false
			}
			m.wire.mu.Unlock()
			startVideoFailureWorker(t, m)
			queue := m.queue(t)
			var finished *rivertype.JobRow
			waitForWithin(t, 60*time.Second, func() bool {
				jobs, err := queue.JobList(t.Context(), river.NewJobListParams().Kinds((workqueue.DerivativeArgs{}).Kind()))
				must(t, err)
				for _, job := range jobs.Jobs {
					if job.State == rivertype.JobStateRetryable {
						_, err := queue.JobRetry(t.Context(), job.ID)
						must(t, err)
					}
					if job.State == rivertype.JobStateCancelled || job.State == rivertype.JobStateDiscarded {
						finished = job
						return true
					}
				}
				return false
			})
			if finished.State != rivertype.JobStateCancelled || finished.Attempt != workqueue.MaxAttempts {
				t.Fatalf("exhausted job: state=%s attempt=%d errors=%+v", finished.State, finished.Attempt, finished.Errors)
			}
			read, err := client.GetMediaVersion(t.Context(), ref.Kind, ref.ID, ref.Version, ck.GetMediaVersionQuery{Status: true})
			must(t, err)
			if read.Status == nil || read.Status.State != media.StateFailed {
				t.Fatalf("exhausted %s remained pending: %+v", mode, read.Status)
			}
			manifests, logical := failureManifests(t, m, ref)
			before, _, err := manifests.Get(t.Context(), logical)
			must(t, err)
			want := []string{"source.mp4"}
			switch mode {
			case "subtitle":
				want = []string{"subs/en.srt"}
			case "poster":
				want = []string{"poster.png"}
			case "image-before-video":
				want = append(want, "pages/one.png")
			}
			if mode == "image-before-video" {
				imageFailures := 0
				for _, failure := range read.Status.Failures {
					if failure.Path == "pages/one.png" {
						imageFailures++
					}
				}
				if imageFailures != 1 || read.Status.FailureCount != len(read.Status.Failures) {
					t.Fatalf("exhausted image missing from failure status: %+v", read.Status)
				}
			}
			for _, name := range want {
				f, ok := before.Get(name)
				if !ok || f.Fail() == nil || len(f.Pending) != 0 {
					t.Fatalf("failure missing for %s: %+v", name, f)
				}
			}
			outputs := map[string]string{}
			for _, f := range before.Files {
				if !f.IsUpload() && f.Blob != "" {
					outputs[f.Path] = f.Blob
				}
			}
			if mode != "image-before-video" && outputs["reader/one.webp"] == "" {
				t.Fatal("completed image was lost")
			}
			var checkpoints string
			if mode == "video" {
				if outputs["streams/144-h264.mp4"] == "" || outputs["download.mp4"] == "" {
					t.Fatalf("partial video outputs were lost: %+v", outputs)
				}
				checkpoints = videoFailureCheckpoints(t, m)
				if checkpoints == "[]" {
					t.Fatal("no real completed encoding checkpoint")
				}
			}
			m.wire.mu.Lock()
			m.wire.refuseOutput = nil
			m.wire.mu.Unlock()
			_, err = client.PostMediaUploadCommit(t.Context(), media.CommitBody{Ref: ref, OperationID: uuid.NewString(), Ops: []media.Op{{Op: media.OpRegenerate}}})
			must(t, err)
			awaitVideoRecovery(t, client, ref)
			after, _, err := manifests.Get(t.Context(), logical)
			must(t, err)
			for path, blob := range outputs {
				f, ok := after.Get(path)
				if !ok || f.Blob != blob {
					t.Fatalf("regeneration replaced completed output %s", path)
				}
			}
			if mode == "video" {
				if after := videoFailureCheckpoints(t, m); after != checkpoints {
					t.Fatalf("regeneration changed encoding checkpoints: before=%s after=%s", checkpoints, after)
				}
			}
		})
	}
}

func startVideoFailureWorker(t *testing.T, m *resilientMedia) func() {
	t.Helper()
	config := m.config
	config.VideoCodecs = []media.Codec{media.CodecH264}
	config.VideoEncoder, config.Preset, config.TopPreset = "cpu", "ultrafast", "ultrafast"
	config.TempDir = t.TempDir()
	processor, err := worker.New(t.Context(), config)
	must(t, err)
	run, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- processor.Run(run) }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Error(err)
				}
			case <-time.After(20 * time.Second):
				t.Error("worker did not stop")
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

func failureManifests(t *testing.T, m *resilientMedia, ref media.RefBody) (*mediastore.Manifests, contentref.ContentRef) {
	t.Helper()
	logical, err := m.config.Kinds.VersionRef(ref.Kind, ref.ID, ref.Version)
	must(t, err)
	journal, err := mediastore.NewPGJournal(m.pool, m.schema, nil)
	must(t, err)
	manifests, err := mediastore.NewManifests(m.storage.store, m.config.Kinds, mediastore.ManifestOptions{Locker: mediastore.PGLocker(m.pool), Journal: journal})
	must(t, err)
	return manifests, logical
}

func awaitVideoRecovery(t *testing.T, client *ck.Client, ref media.RefBody) {
	t.Helper()
	waitForWithin(t, 60*time.Second, func() bool {
		read, err := client.GetMediaVersion(t.Context(), ref.Kind, ref.ID, ref.Version, ck.GetMediaVersionQuery{Status: true})
		must(t, err)
		return read.Status != nil && read.Status.State == media.StateReady && read.Status.FailureCount == 0
	})
}

func videoFailureCheckpoints(t *testing.T, m *resilientMedia) string {
	t.Helper()
	var checkpoints string
	must(t, m.pool.QueryRow(t.Context(), `SELECT coalesce(jsonb_agg(jsonb_build_object(
 'run',run_id,'ordinal',ordinal,'state',state,'output_hash',md5(output::text),'finished_at',finished_at)
 ORDER BY run_id,ordinal),'[]')::text FROM `+pgx.Identifier{m.config.Schema, "encode_chunk"}.Sanitize()).Scan(&checkpoints))
	return checkpoints
}
