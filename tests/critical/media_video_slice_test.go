package critical_test

import (
	"bytes"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/internal/media/workqueue"
	"github.com/open-rails/contentkit/media"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

// Two videos of three rungs, one chunk checkpoint each, are queued. Without a
// video slice each job yields after every chunk and the two interleave. With
// one, every video's first playable rung (144) still comes before any ladder
// rung, and then one video runs its whole ladder in a slice; a video queued
// during that slice plays first, and the slice then resumes.
func TestMediaVideoSlice(t *testing.T) {
	source := filepath.Join(t.TempDir(), "source.mp4")
	command := exec.CommandContext(t.Context(), "ffmpeg", "-nostdin", "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=640x360:rate=12", "-t", "2", "-an", "-c:v", "libx264", "-threads", "1", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-movflags", "+faststart", source)
	if out, err := command.CombinedOutput(); err != nil {
		t.Fatalf("create source: %v %s", err, out)
	}
	video, err := os.ReadFile(source)
	must(t, err)
	for _, tc := range []struct {
		name  string
		slice time.Duration
		late  bool // B is queued while A encodes its first ladder rung
		order string
	}{
		{"with_a_slice_first_playback_comes_before_each_whole_ladder", 10 * time.Minute, false, "A144 B144 A240 A360 B240 B360"},
		{"without_a_slice_chunks_interleave", 0, false, "A144 B144 A240 B240 A360 B360"},
		{"a_video_queued_during_a_ladder_slice_plays_first", 10 * time.Minute, true, "A144 A240 B144 A360 B240 B360"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newResilientMedia(t, func(c *ck.Config) {
				k := &c.Media.Config.Kinds[0]
				k.Uploads = append(k.Uploads, media.Upload{Path: "source", Types: []string{"video/mp4"}, MaxBytes: 10 << 20})
				k.Private = append(k.Private, media.Private{Name: "stream", From: "source", To: "streams/", HLS: &media.HLS{Ladder: []int{360, 240, 144}}})
			})
			// The derivative queue starts paused, so the videos are queued
			// before either is worked.
			_, err := m.pool.Exec(t.Context(), `INSERT INTO `+pgx.Identifier{m.config.Schema, "river_queue"}.Sanitize()+` (name, paused_at, updated_at) VALUES ($1, now(), now())`, workqueue.DerivativeQueue)
			must(t, err)
			client := m.app.engine.Client()
			refs := []media.RefBody{galleryRef(t, client, "Slice A"), galleryRef(t, client, "Slice B")}
			ingest := func(ref media.RefBody) {
				_, err := client.IngestMedia(t.Context(), nil, ref, uuid.NewString(), []ck.MediaFile{{Path: "source.mp4", Type: "video/mp4", Body: bytes.NewReader(video)}})
				must(t, err)
			}
			queue := m.queue(t)
			awaitAvailable := func(n int) {
				waitForWithin(t, 30*time.Second, func() bool {
					jobs, err := queue.JobList(t.Context(), river.NewJobListParams().Kinds((workqueue.DerivativeArgs{}).Kind()).States(rivertype.JobStateAvailable))
					must(t, err)
					return len(jobs.Jobs) == n
				})
			}
			ingest(refs[0])
			queued := 1
			if !tc.late {
				ingest(refs[1])
				queued = 2
			}
			m.config.VideoSlice = tc.slice
			startVideoFailureWorker(t, m)
			awaitAvailable(queued)
			var hold *mediaRequestHold
			if tc.late {
				chunks := 0
				hold = m.wire.pause(t, func(r *http.Request) bool {
					if r.Method != http.MethodPut || r.Header.Get("X-Amz-Copy-Source") != "" || !strings.Contains(r.URL.Path, "/tmp/") || !strings.HasSuffix(r.URL.Path, ".mp4") {
						return false
					}
					chunks++
					return chunks == 2
				})
			}
			must(t, queue.QueueResume(t.Context(), workqueue.DerivativeQueue, nil))
			if tc.late {
				select {
				case <-hold.arrived:
				case <-time.After(30 * time.Second):
					t.Fatal("the first video's ladder chunk was not reached")
				}
				ingest(refs[1])
				awaitAvailable(1)
				hold.unblock()
			}
			for _, ref := range refs {
				awaitVideoRecovery(t, client, ref)
			}

			rows, err := m.pool.Query(t.Context(), `SELECT r.ref::text, r.rung FROM `+pgx.Identifier{m.config.Schema, "encode_chunk"}.Sanitize()+` c
 JOIN `+pgx.Identifier{m.config.Schema, "encode_run"}.Sanitize()+` r ON r.id = c.run_id WHERE c.state = 'done' ORDER BY c.finished_at`)
			must(t, err)
			names := map[string]string{}
			var order []string
			for rows.Next() {
				var ref string
				var rung int
				must(t, rows.Scan(&ref, &rung))
				if names[ref] == "" {
					names[ref] = string(rune('A' + len(names)))
				}
				order = append(order, fmt.Sprintf("%s%d", names[ref], rung))
			}
			must(t, rows.Err())
			got := strings.Join(order, " ")
			t.Logf("chunk order %s", got)
			if got != tc.order {
				t.Fatalf("chunk order %s; want %s", got, tc.order)
			}
		})
	}
}
