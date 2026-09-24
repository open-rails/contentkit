package video_test

import (
	"context"
	"math"
	"testing"
	"time"

	riverhelpers "github.com/open-rails/helpers/river"
	"github.com/riverqueue/river"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/token"
	"github.com/open-rails/contentkit/media/video"
)

type visible struct{}

func (visible) Resolve(_ context.Context, refs []contentref.ContentRef, _ access.Actor) (map[contentref.ContentKey]access.Resolution, error) {
	out := map[contentref.ContentKey]access.Resolution{}
	for _, ref := range refs {
		out[ref.Key()] = access.Resolution{Visible: true, Accessible: true}
	}
	return out, nil
}

// The production path: a committed upload's job waits in media_worker (queue
// position), the worker reports progress on its River row, and the read API
// serves it on the pending file until the ladder is published.
func TestEncodeProgressThroughReadAPI(t *testing.T) {
	requireFFmpeg(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	pool := pgtest.Pool(t, nil)
	if err := video.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "DELETE FROM "+video.Schema+".river_job"); err != nil {
		t.Fatal(err)
	}
	var enq *video.Enqueuer
	e := newEnv(t, nil, queueFunc(func(ctx context.Context, j media.ProcessJob) error { return enq.Enqueue(ctx, j) }))
	var err error
	if enq, err = video.NewEnqueuer(pool, e.kinds); err != nil {
		t.Fatal(err)
	}
	var locker media.Locker
	if !e.store.Capabilities().ConditionalPut {
		locker = media.PGLocker(pool)
	}
	enc, err := video.New(video.Config{Store: e.store, Locker: locker, TempDir: t.TempDir(), Threads: 2, ProgressInterval: 300 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	reader, err := media.NewReader(media.ReaderOptions{Manifests: e.manifests, Kinds: e.kinds, Resolver: visible{},
		Progress: video.NewProgressSource(pool),
		Delivery: media.Delivery{Mode: media.DeliverURL, BaseURL: "https://media.test",
			SigningKey: token.Key{ID: "k1", Secret: []byte("0123456789abcdef0123456789abcdef")}}})
	if err != nil {
		t.Fatal(err)
	}
	read := func() media.FileInfo {
		t.Helper()
		res, err := reader.Read(ctx, e.ref, access.Actor{Anonymous: true}, media.ReadOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return res.Files[0]
	}

	const secs = 42 // 11 segments, the last short
	e.commit(t, fixture{w: 1280, h: 720, secs: secs, rate: 30, audio: 1, tone: 440}.make(t), media.OpInsert)
	if f := read(); f.HLS || f.Progress == nil || f.Progress.Phase != media.PhaseQueued || f.Progress.QueuePosition != 1 {
		t.Fatalf("before the worker: %+v %+v", f, f.Progress)
	}

	wc := video.WorkerConfig{Encoder: enc, Pool: pool, Timeout: time.Hour}
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

	var seen []media.EncodeProgress
	imagePhases := map[string]bool{}
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
poll:
	for {
		select {
		case ev := <-done:
			if ev.Kind != river.EventKindJobCompleted {
				t.Fatalf("job %s: %+v", ev.Kind, ev.Job.Errors)
			}
			break poll
		case <-tick.C:
			if p := read().Progress; p != nil && (len(seen) == 0 || p.At != seen[len(seen)-1].At) {
				seen = append(seen, *p)
			}
			vi, err := reader.VideoImages(ctx, e.ref, access.Actor{Anonymous: true})
			if err != nil {
				t.Fatal(err)
			}
			if vi.Progress != nil {
				imagePhases[vi.Progress.Phase] = true
			}
		case <-ctx.Done():
			t.Fatal("job did not complete")
		}
	}

	m, _ := e.manifest(t)
	h := m.Files[0].HLS
	if h == nil || len(h.Video) == 0 {
		t.Fatalf("not encoded: %+v", m.Files[0])
	}
	want := int(math.Ceil(secs / 4.0))
	if len(h.Video[0].Segments) != want {
		t.Fatalf("ladder has %d segments, want %d", len(h.Video[0].Segments), want)
	}
	var encoding []media.EncodeProgress
	phases := map[string]bool{}
	for i, p := range seen {
		phases[p.Phase] = true
		if i > 0 && (p.Percent < seen[i-1].Percent || p.SegmentsDone < seen[i-1].SegmentsDone) {
			t.Fatalf("progress went backwards: %+v then %+v", seen[i-1], p)
		}
		if p.Phase == media.PhaseEncoding && p.ETA > 0 {
			encoding = append(encoding, p)
		}
		if p.SegmentsTotal != 0 && p.SegmentsTotal != want {
			t.Fatalf("segments_total %d, want %d", p.SegmentsTotal, want)
		}
	}
	t.Logf("%d samples; encoding: %+v", len(seen), encoding)
	if !phases[media.PhaseEncoding] || !phases[media.PhaseUploading] {
		t.Fatalf("phases seen %v", phases)
	}
	if !imagePhases[media.PhaseEncoding] || !imagePhases[media.PhaseImages] {
		t.Fatalf("video-images progress phases %v", imagePhases)
	}
	if len(encoding) < 3 {
		t.Fatalf("%d encoding samples with an ETA: %+v", len(encoding), seen)
	}
	first, last := encoding[0], encoding[len(encoding)-1]
	if last.ETA >= first.ETA || last.SegmentsDone <= first.SegmentsDone || last.Speed <= 0 {
		t.Fatalf("encoding did not advance: first %+v last %+v", first, last)
	}

	if f := read(); !f.HLS || f.Progress != nil {
		t.Fatalf("after publish: %+v %+v", f, f.Progress)
	}
	if vi, err := reader.VideoImages(ctx, e.ref, access.Actor{Anonymous: true}); err != nil || vi.Progress != nil {
		t.Fatalf("video-images after the job: %+v %v", vi.Progress, err)
	}
	var left int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM "+video.Schema+".river_job WHERE metadata ? 'contentkit_progress'").Scan(&left); err != nil || left != 0 {
		t.Fatalf("progress left on %d jobs (%v)", left, err)
	}
}
