package video_test

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	riverhelpers "github.com/open-rails/helpers/river"
	"github.com/riverqueue/river"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/internal/s3test"
	"github.com/open-rails/contentkit/media/video"
	"github.com/open-rails/contentkit/media/workqueue"
)

type packet struct {
	PTS   string `json:"pts_time"`
	Flags string `json:"flags"`
}

// keyframes are a rendition's keyframe times, ms.
func keyframes(t *testing.T, path string) []int {
	t.Helper()
	out, err := exec.Command("ffprobe", "-v", "error", "-select_streams", "v", "-show_entries", "packet=pts_time,flags", "-of", "json", path).Output()
	if err != nil {
		t.Fatalf("ffprobe %s: %v", path, err)
	}
	var p struct{ Packets []packet }
	if err := json.Unmarshal(out, &p); err != nil {
		t.Fatal(err)
	}
	var keys []int
	for _, pk := range p.Packets {
		var v float64
		fmt.Sscan(pk.PTS, &v)
		if strings.HasPrefix(pk.Flags, "K") {
			keys = append(keys, int(math.Round(v*1000)))
		}
	}
	slices.Sort(keys)
	return keys
}

// A ladder with rungs above stageOneMax publishes the lower rungs first
// (playable, the rest pending), then adds the others in one edit. Every
// rung has keyframes at the same times (one per 4 s segment) and the same
// segments, so a player switches between the stages' rungs seamlessly.
func TestTwoStagePublish(t *testing.T) {
	defer video.SetStageOneMax(480)()
	e := newEnv(t, nil, nil)
	source := e.commit(t, fixture{w: 1280, h: 720, secs: 13, rate: 30, audio: 1, subs: true, tone: 440}.make(t), media.OpInsert)
	job := video.Job{Ref: e.ref, Versioned: true}
	var mu sync.Mutex
	var stages [][2]int
	report := func(_ context.Context, files map[string]media.EncodeProgress) {
		mu.Lock()
		defer mu.Unlock()
		if p, ok := files["source"]; ok && p.Stage > 0 && (len(stages) == 0 || stages[len(stages)-1] != [2]int{p.Stage, p.Stages}) {
			stages = append(stages, [2]int{p.Stage, p.Stages})
		}
	}

	more, err := video.EncodeStage(context.Background(), e.encoder, job, report)
	if err != nil || !more {
		t.Fatalf("stage 1: more %v, %v", more, err)
	}
	m, _ := e.manifest(t)
	h := m.Files[0].HLS
	if h == nil || h.Source != source || len(h.Video) != 1 || h.Video[0].Rung != 480 || !slices.Equal(h.Pending, []int{720}) ||
		len(h.Audio) != 1 || len(h.Subs) != 1 || h.Sprite == nil {
		t.Fatalf("stage 1 hls %+v", h)
	}
	if _, ok := m.Downloads[video.DownloadKey("source", 480)]; !ok || len(m.Downloads) != 1 {
		t.Fatalf("stage 1 downloads %v", m.Downloads)
	}
	audio, sprite := h.Audio[0].Blob, h.Sprite.Blob

	more, err = video.EncodeStage(context.Background(), e.encoder, job, report)
	if err != nil || more {
		t.Fatalf("stage 2: more %v, %v", more, err)
	}
	m, _ = e.manifest(t)
	h = m.Files[0].HLS
	if len(h.Video) != 2 || h.Video[0].Rung != 720 || h.Video[1].Rung != 480 || len(h.Pending) != 0 || h.Audio[0].Blob != audio || h.Sprite.Blob != sprite {
		t.Fatalf("stage 2 hls %+v", h)
	}
	for _, n := range []int{720, 480} {
		if d, ok := m.Downloads[video.DownloadKey("source", n)]; !ok || d.Spec != h.Spec {
			t.Fatalf("stage 2 downloads %v", m.Downloads)
		}
	}
	if more, err := video.EncodeStage(context.Background(), e.encoder, job, nil); err != nil || more {
		t.Fatalf("fresh: more %v, %v", more, err)
	}
	if !slices.Equal(stages, [][2]int{{1, 2}, {2, 2}}) {
		t.Fatalf("progress stages %v", stages)
	}

	var paths []string
	for _, r := range h.Video {
		paths = append(paths, e.blob(t, r.Blob))
		checkByteRanges(t, paths[len(paths)-1], r.Segments, "video", 13)
	}
	// Keyframes every 4 s (the first may carry the B-frame delay), the same in both rungs.
	a, b := keyframes(t, paths[0]), keyframes(t, paths[1])
	if !slices.Equal(a, b) || len(a) != 4 {
		t.Fatalf("keyframes %v and %v", a, b)
	}
	for j := 2; j < len(a); j++ {
		if a[j]-a[j-1] != 4000 {
			t.Fatalf("keyframes %v are not every 4 s", a)
		}
	}
	for j, s := range h.Video[0].Segments {
		if s.Seconds != h.Video[1].Segments[j].Seconds {
			t.Fatalf("segment %d: %gs vs %gs", j, s.Seconds, h.Video[1].Segments[j].Seconds)
		}
	}
	switchRungs(t, paths, h.Video)
}

// switchRungs decodes segments alternating between the stages' rungs, each
// as its rung's init plus the segment (what a player switching levels at
// every segment appends): the frames' times must match playing either rung
// alone.
func switchRungs(t *testing.T, paths []string, rs []media.Rendition) {
	t.Helper()
	dir := t.TempDir()
	play := func(pick func(j int) int) []string {
		var pts []string
		for j := range rs[0].Segments {
			i := pick(j)
			data, err := os.ReadFile(paths[i])
			if err != nil {
				t.Fatal(err)
			}
			s := rs[i].Segments[j]
			seg := filepath.Join(dir, fmt.Sprintf("s%d-%d.mp4", j, i))
			if err := os.WriteFile(seg, append(slices.Clone(data[:rs[i].Segments[0].Offset]), data[s.Offset:s.Offset+s.Length]...), 0o600); err != nil {
				t.Fatal(err)
			}
			out, err := exec.Command("ffprobe", "-v", "error", "-show_entries", "frame=pts_time", "-of", "csv=p=0", seg).CombinedOutput()
			if err != nil || strings.Contains(string(out), "error") {
				t.Fatalf("segment %d of rung %d: %v: %s", j, rs[i].Rung, err, out)
			}
			pts = append(pts, strings.Fields(string(out))...)
		}
		return pts
	}
	switched := play(func(j int) int { return j % len(rs) })
	for i := range rs {
		if alone := play(func(int) int { return i }); !slices.Equal(switched, alone) {
			t.Fatalf("switching rungs plays %d frames %v…, rung %d alone %d %v…", len(switched), switched[:5], rs[i].Rung, len(alone), alone[:5])
		}
	}
}

// The worker runs one stage per job: the second stage is a follow-up job at
// a lower priority, and the read API reports it as stage 2 of 2.
func TestWorkerQueuesSecondStage(t *testing.T) {
	defer video.SetStageOneMax(480)()
	requireFFmpeg(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	pool := pgtest.Pool(t, nil)
	if err := workqueue.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "DELETE FROM "+workqueue.Schema+".river_job"); err != nil {
		t.Fatal(err)
	}
	var enq *workqueue.Queue
	e := newEnv(t, nil, queueFunc(func(ctx context.Context, j media.ProcessJob) error { return enq.Enqueue(ctx, j) }))
	var err error
	if enq, err = workqueue.New(pool, e.kinds); err != nil {
		t.Fatal(err)
	}
	e.commit(t, fixture{w: 1280, h: 720, secs: 5, rate: 30, audio: 1, tone: 440}.make(t), media.OpInsert)
	enc, err := video.New(video.Config{Store: e.store, Locker: s3test.Locker(t, e.store), TempDir: t.TempDir(), Threads: 2, Encoder: video.EncoderX264})
	if err != nil {
		t.Fatal(err)
	}
	wc := video.WorkerConfig{Encoder: enc, Pool: pool, Kinds: e.kinds, Timeout: time.Hour}
	contribution, err := video.Contribution(wc)
	if err != nil {
		t.Fatal(err)
	}
	cfg := video.ClientConfig(wc)
	cfg.FetchPollInterval = 100 * time.Millisecond
	worker, err := riverhelpers.New(ctx, pool, cfg, contribution)
	if err != nil {
		t.Fatal(err)
	}
	done, stop := worker.Subscribe(river.EventKindJobCompleted, river.EventKindJobFailed, river.EventKindJobCancelled)
	defer stop()
	if err := worker.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() {
		stopCtx, c := context.WithTimeout(context.Background(), 30*time.Second)
		defer c()
		_ = worker.StopAndCancel(stopCtx)
	}()
	var priorities []int
	for len(priorities) < 2 {
		select {
		case ev := <-done:
			if ev.Kind != river.EventKindJobCompleted {
				t.Fatalf("job %s: %+v", ev.Kind, ev.Job.Errors)
			}
			priorities = append(priorities, ev.Job.Priority)
			if len(priorities) == 1 {
				m, _ := e.manifest(t)
				if h := m.Files[0].HLS; h == nil || len(h.Video) != 1 || !slices.Equal(h.Pending, []int{720}) {
					t.Fatalf("after the first job: %+v", h)
				}
			}
		case <-ctx.Done():
			t.Fatal("jobs did not complete")
		}
	}
	if !slices.Equal(priorities, []int{1, 2}) {
		t.Fatalf("job priorities %v", priorities)
	}
	m, _ := e.manifest(t)
	if h := m.Files[0].HLS; len(h.Video) != 2 || len(h.Pending) != 0 {
		t.Fatalf("after the follow-up: %+v", h)
	}
}

// A compliant source's top rung is its own video stream, copied: the same
// packets, on the lower rungs' segments, switchable with them.
func TestPassthroughTopRung(t *testing.T) {
	e := newEnv(t, nil, nil)
	src := filepath.Join(t.TempDir(), "source.mp4")
	if b, err := exec.Command("ffmpeg", "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=1280x720:rate=30:duration=9", "-f", "lavfi", "-i", "sine=duration=9",
		"-map", "0:v", "-map", "1:a", "-c:v", "libx264", "-preset", "veryfast", "-crf", "26", "-pix_fmt", "yuv420p",
		"-force_key_frames", "expr:gte(t,n_forced*4)", "-sc_threshold", "0", "-c:a", "aac", "-y", src).CombinedOutput(); err != nil {
		t.Fatalf("fixture: %v: %s", err, b)
	}
	e.commit(t, src, media.OpInsert)
	e.encode(t)
	m, _ := e.manifest(t)
	h := m.Files[0].HLS
	if len(h.Video) != 2 || h.Video[0].Rung != 720 {
		t.Fatalf("hls %+v", h)
	}
	hash := func(path string) string {
		out, err := exec.Command("ffmpeg", "-v", "error", "-i", path, "-map", "0:v", "-c", "copy", "-f", "streamhash", "-").Output()
		if err != nil {
			t.Fatal(err)
		}
		return string(out)
	}
	top := e.blob(t, h.Video[0].Blob)
	if hash(top) != hash(src) {
		t.Fatal("the 720 rung is not the source's video stream")
	}
	paths := []string{top, e.blob(t, h.Video[1].Blob)}
	for i, r := range h.Video {
		checkByteRanges(t, paths[i], r.Segments, "video", 9)
	}
	switchRungs(t, paths, h.Video)
}

// Cancel removes an item's queued image and video jobs (both stages share the job shape).
func TestCancelJobs(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Pool(t, nil)
	if err := workqueue.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "DELETE FROM "+workqueue.Schema+".river_job"); err != nil {
		t.Fatal(err)
	}
	e := newEnv(t, nil, nil)
	enq, err := workqueue.New(pool, e.kinds)
	if err != nil {
		t.Fatal(err)
	}
	other := e.ref
	other.ContentID = cid(99)
	for _, ref := range []contentref.ContentRef{e.ref, e.ref, other} {
		if err := enq.Enqueue(ctx, media.ProcessJob{Ref: ref}); err != nil {
			t.Fatal(err)
		}
	}
	// Each commit of a video kind queues a video job and (one pending) image job.
	if n, err := enq.Cancel(ctx, e.ref); err != nil || n != 3 {
		t.Fatalf("cancelled %d: %v", n, err)
	}
	var states []string
	rows, _ := pool.Query(ctx, "SELECT state FROM "+workqueue.Schema+".river_job ORDER BY id")
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		states = append(states, s)
	}
	if !slices.Equal(states, []string{"cancelled", "cancelled", "cancelled", "available", "available"}) {
		t.Fatalf("states %v", states)
	}
}
