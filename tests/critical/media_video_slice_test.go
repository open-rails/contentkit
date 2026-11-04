package critical_test

import (
	"bytes"
	"fmt"
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

// Two videos of three chunk checkpoints each are queued together. With a
// video slice the job keeps encoding its own video, so one finishes before the
// other encodes more than one chunk; without one, each job yields after every
// chunk and the two interleave.
func TestMediaVideoSlice(t *testing.T) {
	source := filepath.Join(t.TempDir(), "source.mp4")
	command := exec.CommandContext(t.Context(), "ffmpeg", "-nostdin", "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=640x360:rate=12", "-t", "2", "-an", "-c:v", "libx264", "-threads", "1", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-movflags", "+faststart", source)
	if out, err := command.CombinedOutput(); err != nil {
		t.Fatalf("create source: %v %s", err, out)
	}
	video, err := os.ReadFile(source)
	must(t, err)
	for _, tc := range []struct {
		name        string
		slice       time.Duration
		interleaved bool
	}{
		{"with_a_slice_videos_finish_in_turn", 10 * time.Minute, false},
		{"without_a_slice_chunks_interleave", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newResilientMedia(t, func(c *ck.Config) {
				k := &c.Media.Config.Kinds[0]
				k.Uploads = append(k.Uploads, media.Upload{Path: "source", Types: []string{"video/mp4"}, MaxBytes: 10 << 20})
				k.Private = append(k.Private, media.Private{Name: "stream", From: "source", To: "streams/", HLS: &media.HLS{Ladder: []int{360, 240, 144}}})
			})
			// The derivative queue starts paused, so both videos are queued
			// before either is worked.
			_, err := m.pool.Exec(t.Context(), `INSERT INTO `+pgx.Identifier{m.config.Schema, "river_queue"}.Sanitize()+` (name, paused_at, updated_at) VALUES ($1, now(), now())`, workqueue.DerivativeQueue)
			must(t, err)
			client := m.app.engine.Client()
			refs := make([]media.RefBody, 2)
			for i := range refs {
				refs[i] = galleryRef(t, client, fmt.Sprintf("Slice %d", i))
				_, err := client.IngestMedia(t.Context(), nil, refs[i], uuid.NewString(), []ck.MediaFile{{Path: "source.mp4", Type: "video/mp4", Body: bytes.NewReader(video)}})
				must(t, err)
			}
			m.config.VideoSlice = tc.slice
			startVideoFailureWorker(t, m)
			queue := m.queue(t)
			waitForWithin(t, 30*time.Second, func() bool {
				jobs, err := queue.JobList(t.Context(), river.NewJobListParams().Kinds((workqueue.DerivativeArgs{}).Kind()).States(rivertype.JobStateAvailable))
				must(t, err)
				return len(jobs.Jobs) == len(refs)
			})
			must(t, queue.QueueResume(t.Context(), workqueue.DerivativeQueue, nil))
			for _, ref := range refs {
				awaitVideoRecovery(t, client, ref)
			}

			rows, err := m.pool.Query(t.Context(), `SELECT r.ref::text FROM `+pgx.Identifier{m.config.Schema, "encode_chunk"}.Sanitize()+` c
 JOIN `+pgx.Identifier{m.config.Schema, "encode_run"}.Sanitize()+` r ON r.id = c.run_id WHERE c.state = 'done' ORDER BY c.finished_at`)
			must(t, err)
			chunks, err := pgx.CollectRows(rows, pgx.RowTo[string])
			must(t, err)
			names, count, last := map[string]string{}, map[string]int{}, map[string]int{}
			var order strings.Builder
			for i, ref := range chunks {
				if names[ref] == "" {
					names[ref] = string(rune('A' + len(names)))
				}
				order.WriteString(names[ref])
				count[ref]++
				last[ref] = i
			}
			if len(count) != len(refs) {
				t.Fatalf("chunks of %d videos finished; want %d: %s", len(count), len(refs), order.String())
			}
			first := chunks[0]
			for ref, n := range count {
				if n < 3 {
					t.Fatalf("video %s finished in %d chunks; the fixture needs several: %s", names[ref], n, order.String())
				}
				if last[ref] < last[first] {
					first = ref
				}
			}
			others := 0
			for _, ref := range chunks[:last[first]] {
				if ref != first {
					others++
				}
			}
			t.Logf("chunk order %s", order.String())
			if tc.interleaved != (others > 1) {
				t.Fatalf("video %s finished after %d chunks of the other video; chunk order %s", names[first], others, order.String())
			}
		})
	}
}
