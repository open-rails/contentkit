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

// A 1080p source publishes 480p in AV1 and H.264 first (with the tracks and
// the sprite, 1080 pending), then adds 1080p in both codecs. Every
// rendition has keyframes every 4 s and the same segments, so a player
// switches between the stages' rungs seamlessly.
func TestProgressivePublish(t *testing.T) {
	e := newEnv(t, nil, nil)
	source := e.commit(t, fixture{w: 1920, h: 1080, secs: 9, audio: 1, subs: true, tone: 440}.make(t), media.OpInsert)
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
	ladder := func(h *media.HLS) []string {
		var out []string
		for _, r := range h.Video {
			out = append(out, fmt.Sprintf("%d-%s", r.Rung, r.Codec))
		}
		return out
	}

	more, err := video.EncodeStage(context.Background(), e.encoder, job, report)
	if err != nil || !more {
		t.Fatalf("stage 1: more %v, %v", more, err)
	}
	m, _ := e.manifest(t)
	h := m.Files[0].HLS
	if h == nil || h.Source != source || !slices.Equal(ladder(h), []string{"480-av1", "480-h264"}) || !slices.Equal(h.Pending, []int{1080}) ||
		len(h.Audio) != 1 || len(h.Subs) != 1 || h.Sprite == nil {
		t.Fatalf("stage 1 hls %+v", h)
	}
	if _, ok := m.Downloads[video.DownloadKey("source", 480)]; !ok || len(m.Downloads) != 1 {
		t.Fatalf("stage 1 downloads %v", m.Downloads)
	}
	if r := m.Readiness(); r.State != media.StateProcessing {
		t.Fatalf("stage 1 readiness %+v", r)
	}
	audio, sprite := h.Audio[0].Blob, h.Sprite.Blob

	more, err = video.EncodeStage(context.Background(), e.encoder, job, report)
	if err != nil || more {
		t.Fatalf("stage 2: more %v, %v", more, err)
	}
	m, _ = e.manifest(t)
	h = m.Files[0].HLS
	if !slices.Equal(ladder(h), []string{"1080-av1", "480-av1", "1080-h264", "480-h264"}) || len(h.Pending) != 0 ||
		h.Audio[0].Blob != audio || h.Sprite.Blob != sprite {
		t.Fatalf("stage 2 hls %+v", h)
	}
	for _, n := range []int{1080, 480} {
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

	// Keyframes every 4 s (the first may carry the B-frame delay), the same
	// in each codec's rungs; segments the same in every rendition.
	paths := map[media.Codec][]string{}
	var byCodec = map[media.Codec][]media.Rendition{}
	for _, r := range h.Video {
		path := e.blob(t, r.Blob)
		checkByteRanges(t, path, r.Segments, "video", 9)
		paths[r.Codec] = append(paths[r.Codec], path)
		byCodec[r.Codec] = append(byCodec[r.Codec], r)
		for j, s := range r.Segments {
			if s.Seconds != h.Video[0].Segments[j].Seconds {
				t.Fatalf("%dp %s segment %d: %gs vs %gs", r.Rung, r.Codec, j, s.Seconds, h.Video[0].Segments[j].Seconds)
			}
		}
	}
	for c, ps := range paths {
		a, b := keyframes(t, ps[0]), keyframes(t, ps[1])
		if !slices.Equal(a, b) || len(a) != 3 || a[2]-a[1] != 4000 {
			t.Fatalf("%s keyframes %v and %v", c, a, b)
		}
		switchRungs(t, ps, byCodec[c])
	}
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
			for k := range switched {
				if switched[k] != alone[k] {
					t.Logf("first difference at frame %d: %v vs %v", k, switched[max(0, k-2):min(len(switched), k+3)], alone[max(0, k-2):min(len(alone), k+3)])
					break
				}
			}
			t.Fatalf("switching rungs plays %d frames %v…, rung %d alone %d %v…", len(switched), switched[:5], rs[i].Rung, len(alone), alone[:5])
		}
	}
}

// The worker plans, encodes bounded chunks, then assembles a playable tier.
func TestWorkerAssemblesPlayableChunks(t *testing.T) {
	requireFFmpeg(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	pool := pgtest.Pool(t, nil)
	schema := pgtest.EmptySchema(t, ctx, pool)
	if err := workqueue.Migrate(ctx, pool, schema); err != nil {
		t.Fatal(err)
	}
	var enq *workqueue.Queue
	e := newEnv(t, nil, queueFunc(func(ctx context.Context, j media.ProcessJob) error { return enq.Enqueue(ctx, j) }))
	var err error
	if enq, err = workqueue.New(pool, e.kinds, schema); err != nil {
		t.Fatal(err)
	}
	e.commit(t, fixture{w: 1280, h: 720, secs: 9, rate: 30, audio: 1, tone: 440}.make(t), media.OpInsert)
	enc, err := video.New(video.Config{Store: e.store, Locker: s3test.Locker(t, e.store), TempDir: t.TempDir(), Threads: 2, Encoder: video.EncoderCPU})
	if err != nil {
		t.Fatal(err)
	}
	wc := video.WorkerConfig{Encoder: enc, Pool: pool, Schema: schema, Kinds: e.kinds,
		Timeout: time.Hour, ChunkTarget: 8 * time.Second}
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
	var kinds []string
	for len(kinds) < 6 {
		select {
		case ev := <-done:
			if ev.Kind != river.EventKindJobCompleted {
				t.Fatalf("job %s: %+v", ev.Kind, ev.Job.Errors)
			}
			kinds = append(kinds, ev.Job.Kind)
		case <-ctx.Done():
			t.Fatal("jobs did not complete")
		}
	}
	if !slices.Equal(kinds, []string{(workqueue.VideoPlanArgs{}).Kind(),
		(workqueue.VideoChunkArgs{}).Kind(), (workqueue.VideoAssembleArgs{}).Kind(),
		(workqueue.VideoChunkArgs{}).Kind(), (workqueue.VideoChunkArgs{}).Kind(),
		(workqueue.VideoAssembleArgs{}).Kind()}) {
		t.Fatalf("job sequence %v", kinds)
	}
	m, _ := e.manifest(t)
	h := m.Files[0].HLS
	if h == nil || len(h.Video) != 4 || len(h.Pending) != 0 || !m.Files[0].Servable() {
		t.Fatalf("after assembly: %+v", h)
	}
	for _, rendition := range h.Video {
		checkByteRanges(t, e.blob(t, rendition.Blob), rendition.Segments, "video", 9)
	}
}

func TestWorkerPublishesEachRung(t *testing.T) {
	requireFFmpeg(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	pool := pgtest.Pool(t, nil)
	schema := pgtest.EmptySchema(t, ctx, pool)
	if err := workqueue.Migrate(ctx, pool, schema); err != nil {
		t.Fatal(err)
	}
	var enq *workqueue.Queue
	e := newEnv(t, nil, queueFunc(func(ctx context.Context, j media.ProcessJob) error { return enq.Enqueue(ctx, j) }))
	var err error
	if enq, err = workqueue.New(pool, e.kinds, schema); err != nil {
		t.Fatal(err)
	}
	e.commit(t, fixture{w: 3840, h: 2160, secs: 5, rate: 10, audio: 1, tone: 440}.make(t), media.OpInsert)
	enc, err := video.New(video.Config{Store: e.store, Locker: s3test.Locker(t, e.store), TempDir: t.TempDir(),
		Threads: 2, Encoder: video.EncoderCPU, Codecs: []media.Codec{media.CodecH264}})
	if err != nil {
		t.Fatal(err)
	}
	wc := video.WorkerConfig{Encoder: enc, Pool: pool, Schema: schema, Kinds: e.kinds, Timeout: time.Hour}
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
		stopCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = worker.StopAndCancel(stopCtx)
	}()
	assembled := 0
	for assembled < 3 {
		select {
		case ev := <-done:
			if ev.Kind != river.EventKindJobCompleted {
				t.Fatalf("job %s %s: %+v", ev.Kind, ev.Job.Kind, ev.Job.Errors)
			}
			if ev.Job.Kind != (workqueue.VideoAssembleArgs{}).Kind() {
				continue
			}
			assembled++
			m, _ := e.manifest(t)
			h := m.Files[0].HLS
			if h == nil {
				t.Fatal("assemble completed without HLS")
			}
			if assembled == 1 {
				if len(h.Video) != 1 || h.Video[0].Rung != 480 || !slices.Equal(h.Pending, []int{1080, 2160}) ||
					!m.Files[0].Servable() || m.Files[0].State() != media.StateProcessing || h.Sprite == nil {
					t.Fatalf("first rung: %+v", h)
				}
			} else if assembled == 2 {
				if len(h.Video) != 2 || !slices.Equal(h.Pending, []int{2160}) || m.Files[0].State() != media.StateProcessing {
					t.Fatalf("second rung: %+v", h)
				}
			} else if len(h.Video) != 3 || len(h.Pending) != 0 || m.Files[0].State() != media.StateReady {
				t.Fatalf("final rung: %+v", h)
			}
		case <-ctx.Done():
			t.Fatal("rungs did not complete")
		}
	}
	m, _ := e.manifest(t)
	for _, rendition := range m.Files[0].HLS.Video {
		checkByteRanges(t, e.blob(t, rendition.Blob), rendition.Segments, "video", 5)
	}
}

func TestWorkerResumesPublishedRung(t *testing.T) {
	requireFFmpeg(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	pool := pgtest.Pool(t, nil)
	schema := pgtest.EmptySchema(t, ctx, pool)
	if err := workqueue.Migrate(ctx, pool, schema); err != nil {
		t.Fatal(err)
	}
	var enq *workqueue.Queue
	e := newEnv(t, nil, queueFunc(func(ctx context.Context, j media.ProcessJob) error { return enq.Enqueue(ctx, j) }))
	var err error
	if enq, err = workqueue.New(pool, e.kinds, schema); err != nil {
		t.Fatal(err)
	}
	e.commit(t, fixture{w: 1280, h: 720, secs: 5, audio: 1, tone: 440}.make(t), media.OpInsert)
	if more, err := video.EncodeStage(ctx, e.encoder, video.Job{Ref: e.ref, Versioned: true}, nil); err != nil || !more {
		t.Fatalf("first rung: more %v, %v", more, err)
	}
	partial, _ := e.manifest(t)
	before := partial.Files[0].HLS
	if before == nil || !slices.Equal(before.Pending, []int{720}) {
		t.Fatalf("partial ladder: %+v", before)
	}
	wc := video.WorkerConfig{Encoder: e.encoder, Pool: pool, Schema: schema, Kinds: e.kinds}
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
		stopCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = worker.StopAndCancel(stopCtx)
	}()
	for {
		select {
		case event := <-done:
			if event.Kind != river.EventKindJobCompleted {
				t.Fatalf("job %s %s: %+v", event.Kind, event.Job.Kind, event.Job.Errors)
			}
			m, _ := e.manifest(t)
			h := m.Files[0].HLS
			if h == nil || len(h.Pending) != 0 {
				continue
			}
			low := slices.IndexFunc(h.Video, func(r media.Rendition) bool { return r.Rung == 480 && r.Codec == before.Video[0].Codec })
			if len(h.Video) != 4 || low < 0 || h.Video[low].Blob != before.Video[0].Blob ||
				h.Audio[0].Blob != before.Audio[0].Blob || h.Sprite.Blob != before.Sprite.Blob {
				t.Fatalf("published rung changed during resume: before %+v after %+v", before, h)
			}
			var released int
			if err := pool.QueryRow(ctx, `SELECT released FROM `+schema+`.encode_run WHERE rung = 480`).Scan(&released); err != nil || released != 0 {
				t.Fatalf("already-published rung released %d chunks: %v", released, err)
			}
			return
		case <-ctx.Done():
			t.Fatal("resumed rung did not complete")
		}
	}
}

func TestWorkerShortVideoOvertakesLongVideo(t *testing.T) {
	requireFFmpeg(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	pool := pgtest.Pool(t, nil)
	schema := pgtest.EmptySchema(t, ctx, pool)
	if err := workqueue.Migrate(ctx, pool, schema); err != nil {
		t.Fatal(err)
	}
	var enq *workqueue.Queue
	e := newEnv(t, nil, queueFunc(func(ctx context.Context, j media.ProcessJob) error { return enq.Enqueue(ctx, j) }))
	var err error
	if enq, err = workqueue.New(pool, e.kinds, schema); err != nil {
		t.Fatal(err)
	}
	longRef := e.ref
	e.commit(t, fixture{w: 1280, h: 720, secs: 40, rate: 10}.make(t), media.OpInsert)
	shortRef := contentref.NewVersion(e.Env.Tenant+"other", "video", cid(89), "v1")
	e.ref = shortRef
	e.commit(t, fixture{w: 640, h: 360, secs: 5, rate: 10}.make(t), media.OpInsert)
	enc, err := video.New(video.Config{Store: e.store, Locker: s3test.Locker(t, e.store), TempDir: t.TempDir(),
		Threads: 2, Encoder: video.EncoderCPU, Codecs: []media.Codec{media.CodecH264}})
	if err != nil {
		t.Fatal(err)
	}
	wc := video.WorkerConfig{Encoder: enc, Pool: pool, Schema: schema, Kinds: e.kinds, ChunkTarget: 8 * time.Second}
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
		stopCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = worker.StopAndCancel(stopCtx)
	}()
	for {
		select {
		case event := <-done:
			if event.Kind != river.EventKindJobCompleted {
				t.Fatalf("job %s %s: %+v", event.Kind, event.Job.Kind, event.Job.Errors)
			}
			short, _, err := e.manifests.Get(ctx, shortRef)
			if err != nil {
				t.Fatal(err)
			}
			if short.Files[0].HLS == nil || len(short.Files[0].HLS.Video) == 0 {
				continue
			}
			long, _, err := e.manifests.Get(ctx, longRef)
			if err != nil {
				t.Fatal(err)
			}
			if long.Files[0].State() == media.StateReady {
				t.Fatal("long video finished before the short tenant's video")
			}
			return
		case <-ctx.Done():
			t.Fatal("short video did not overtake long video")
		}
	}
}

func TestInterruptedChunkRetriesWithoutAttempt(t *testing.T) {
	requireFFmpeg(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	pool := pgtest.Pool(t, nil)
	schema := pgtest.EmptySchema(t, ctx, pool)
	if err := workqueue.Migrate(ctx, pool, schema); err != nil {
		t.Fatal(err)
	}
	var enq *workqueue.Queue
	e := newEnv(t, nil, queueFunc(func(ctx context.Context, j media.ProcessJob) error { return enq.Enqueue(ctx, j) }))
	var err error
	if enq, err = workqueue.New(pool, e.kinds, schema); err != nil {
		t.Fatal(err)
	}
	e.commit(t, fixture{w: 1280, h: 720, secs: 120, rate: 10}.make(t), media.OpInsert)
	enc, err := video.New(video.Config{Store: e.store, Locker: s3test.Locker(t, e.store), TempDir: t.TempDir(),
		Threads: 2, Encoder: video.EncoderCPU, Codecs: []media.Codec{media.CodecH264}})
	if err != nil {
		t.Fatal(err)
	}
	wc := video.WorkerConfig{Encoder: enc, Pool: pool, Schema: schema, Kinds: e.kinds}
	contribution, err := video.Contribution(wc)
	if err != nil {
		t.Fatal(err)
	}
	clientConfig := func() *river.Config {
		cfg := video.ClientConfig(wc)
		cfg.FetchPollInterval = 100 * time.Millisecond
		return cfg
	}
	first, err := riverhelpers.New(ctx, pool, clientConfig(), contribution)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Start(ctx); err != nil {
		t.Fatal(err)
	}
	chunkKind := (workqueue.VideoChunkArgs{}).Kind()
	var chunkID int64
	for chunkID == 0 {
		if err := pool.QueryRow(ctx, `SELECT COALESCE((SELECT id FROM `+schema+`.river_job
WHERE kind = $1 AND state = 'running' LIMIT 1), 0)`, chunkKind).Scan(&chunkID); err != nil {
			t.Fatal(err)
		}
		if chunkID == 0 {
			select {
			case <-time.After(20 * time.Millisecond):
			case <-ctx.Done():
				t.Fatal("chunk did not start")
			}
		}
	}
	progress, err := workqueue.NewProgressSource(pool, schema)
	if err != nil {
		t.Fatal(err)
	}
	status, err := progress.EncodeProgress(ctx, e.ref)
	if err != nil || status.Files["source"].Phase != media.PhaseEncoding {
		t.Fatalf("running chunk progress %+v: %v", status, err)
	}
	stopCtx, stop := context.WithTimeout(context.Background(), 30*time.Second)
	if err := first.StopAndCancel(stopCtx); err != nil {
		stop()
		t.Fatal(err)
	}
	stop()
	var attempt int
	var state string
	if err := pool.QueryRow(ctx, `SELECT attempt, state FROM `+schema+`.river_job WHERE id = $1`, chunkID).Scan(&attempt, &state); err != nil {
		t.Fatal(err)
	}
	if attempt != 0 || state != "available" {
		t.Fatalf("interrupted chunk attempt %d, state %q", attempt, state)
	}
	secondContribution, err := video.Contribution(wc)
	if err != nil {
		t.Fatal(err)
	}
	second, err := riverhelpers.New(ctx, pool, clientConfig(), secondContribution)
	if err != nil {
		t.Fatal(err)
	}
	done, unsubscribe := second.Subscribe(river.EventKindJobCompleted, river.EventKindJobFailed, river.EventKindJobCancelled)
	defer unsubscribe()
	if err := second.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() {
		stopCtx, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		_ = second.StopAndCancel(stopCtx)
	}()
	for {
		select {
		case event := <-done:
			if event.Kind != river.EventKindJobCompleted {
				t.Fatalf("retry job %s %s: %+v", event.Kind, event.Job.Kind, event.Job.Errors)
			}
			m, _ := e.manifest(t)
			if m.Files[0].State() == media.StateReady {
				return
			}
		case <-ctx.Done():
			t.Fatal("interrupted chunk did not resume")
		}
	}
}

// A compliant source's top rung in its own codec is its video stream,
// copied: the same packets, on the lower rung's segments, switchable with
// it. The other codec's top rung is encoded. HEVC is configured for the
// HEVC source (the default is AV1 + H.264).
func TestPassthroughTopRung(t *testing.T) {
	for _, c := range []struct {
		codec media.Codec
		args  []string
	}{
		{media.CodecH264, []string{"-c:v", "libx264", "-preset", "veryfast", "-crf", "26", "-sc_threshold", "0"}},
		{media.CodecHEVC, []string{"-c:v", "libx265", "-preset", "ultrafast", "-crf", "30", "-tag:v", "hvc1", "-forced-idr", "1",
			"-x265-params", "open-gop=0:scenecut=0:log-level=error"}},
	} {
		t.Run(string(c.codec), func(t *testing.T) {
			e := newEnv(t, nil, nil)
			codecs := []media.Codec{media.CodecAV1, media.CodecH264}
			if c.codec == media.CodecHEVC {
				codecs = []media.Codec{media.CodecHEVC, media.CodecH264}
				var err error
				if e.encoder, err = video.New(video.Config{Store: e.store, Locker: s3test.Locker(t, e.store), TempDir: t.TempDir(), Threads: 2,
					Encoder: video.EncoderCPU, Codecs: codecs}); err != nil {
					t.Fatal(err)
				}
			}
			src := filepath.Join(t.TempDir(), "source.mp4")
			args := append([]string{"-v", "error", "-f", "lavfi", "-i", "testsrc2=size=1280x720:rate=30:duration=9", "-f", "lavfi", "-i", "sine=duration=9",
				"-map", "0:v", "-map", "1:a", "-pix_fmt", "yuv420p", "-force_key_frames", "expr:gte(t,n_forced*4)"}, c.args...)
			if b, err := exec.Command("ffmpeg", append(args, "-c:a", "aac", "-y", src)...).CombinedOutput(); err != nil {
				t.Fatalf("fixture: %v: %s", err, b)
			}
			e.commit(t, src, media.OpInsert)
			e.encode(t)
			m, _ := e.manifest(t)
			h := m.Files[0].HLS
			find := func(rung int, codec media.Codec) media.Rendition {
				i := slices.IndexFunc(h.Video, func(r media.Rendition) bool { return r.Rung == rung && r.Codec == codec })
				if i < 0 {
					t.Fatalf("no %dp %s in %+v", rung, codec, h.Video)
				}
				return h.Video[i]
			}
			hash := func(path string) string {
				out, err := exec.Command("ffmpeg", "-v", "error", "-i", path, "-map", "0:v", "-c", "copy", "-f", "streamhash", "-").Output()
				if err != nil {
					t.Fatal(err)
				}
				return string(out)
			}
			if len(h.Video) != 4 {
				t.Fatalf("hls %+v", h)
			}
			for _, codec := range codecs {
				top, low := find(720, codec), find(480, codec)
				paths := []string{e.blob(t, top.Blob), e.blob(t, low.Blob)}
				if copied := hash(paths[0]) == hash(src); copied != (codec == c.codec) {
					t.Fatalf("%s 720 rung copied from the source: %v", codec, copied)
				}
				if codec == media.CodecHEVC && !strings.HasPrefix(top.Codecs, "hvc1.") {
					t.Fatalf("hevc codecs %q", top.Codecs)
				}
				checkByteRanges(t, paths[0], top.Segments, "video", 9)
				switchRungs(t, paths, []media.Rendition{top, low})
			}
		})
	}
}

// Cancel removes an item's queued image and video jobs (both stages share the job shape).
func TestCancelJobs(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Pool(t, nil)
	if err := workqueue.Migrate(ctx, pool, testSchema); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "DELETE FROM "+testSchema+".river_job"); err != nil {
		t.Fatal(err)
	}
	e := newEnv(t, nil, nil)
	enq, err := workqueue.New(pool, e.kinds, testSchema)
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
	rows, _ := pool.Query(ctx, "SELECT state FROM "+testSchema+".river_job ORDER BY id")
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		states = append(states, s)
	}
	if !slices.Equal(states, []string{"cancelled", "cancelled", "cancelled", "available", "available"}) {
		t.Fatalf("states %v", states)
	}
}
