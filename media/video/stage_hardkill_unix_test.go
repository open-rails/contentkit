//go:build linux || darwin

package video_test

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	riverhelpers "github.com/open-rails/helpers/river"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/internal/s3test"
	mediaS3 "github.com/open-rails/contentkit/media/s3"
	"github.com/open-rails/contentkit/media/video"
	"github.com/open-rails/contentkit/media/workqueue"
)

func TestHardKilledChunkResumesWithIdenticalOutput(t *testing.T) {
	if os.Getenv("CONTENTKIT_HARDKILL_CHILD") == "1" {
		runHardKilledChunkWorkerChild(t)
		return
	}
	requireFFmpeg(t)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
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
	source := fixture{w: 1280, h: 720, secs: 120, rate: 10}.make(t)
	e.commit(t, source, media.OpInsert)
	enc, err := video.New(ctx, video.Config{Store: e.store, Locker: s3test.Locker(t, e.store), TempDir: t.TempDir(),
		Threads: 2, Encoder: video.EncoderCPU, Codecs: []media.Codec{media.CodecH264}, ProgressInterval: 100 * time.Millisecond})
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
	baselineWorker, err := riverhelpers.New(ctx, pool, cfg, contribution)
	if err != nil {
		t.Fatal(err)
	}
	if err := baselineWorker.Start(ctx); err != nil {
		t.Fatal(err)
	}
	baselineRunning := true
	defer func() {
		if !baselineRunning {
			return
		}
		stopCtx, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		_ = baselineWorker.StopAndCancel(stopCtx)
	}()
	for {
		m, _ := e.manifest(t)
		if h := m.Files[0].HLS; h != nil && len(h.Video) == 2 && h.Sprite != nil &&
			len(m.Downloads) > 0 && len(h.Pending) == 0 && m.Files[0].State() == media.StateReady {
			break
		}
		select {
		case <-time.After(100 * time.Millisecond):
		case <-ctx.Done():
			t.Fatal("uninterrupted encode did not finish")
		}
	}
	baseline, _ := e.manifest(t)
	stopCtx, stop := context.WithTimeout(context.Background(), 30*time.Second)
	if err := baselineWorker.StopAndCancel(stopCtx); err != nil {
		stop()
		t.Fatal(err)
	}
	stop()
	baselineRunning = false

	e.ref = contentref.NewVersion(e.Tenant, "video", cid(89), "v1")
	e.commit(t, source, media.OpInsert)
	configJSON, err := json.Marshal(e.Config)
	if err != nil {
		t.Fatal(err)
	}
	child := exec.Command(os.Args[0], "-test.run=^TestHardKilledChunkResumesWithIdenticalOutput$")
	child.Env = append(os.Environ(), "CONTENTKIT_HARDKILL_CHILD=1", "CONTENTKIT_HARDKILL_SCHEMA="+schema,
		"CONTENTKIT_HARDKILL_S3_CONFIG="+string(configJSON), "CONTENTKIT_HARDKILL_TEMP_DIR="+t.TempDir())
	child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var childOutput strings.Builder
	child.Stdout, child.Stderr = &childOutput, &childOutput
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	childExited := make(chan error, 1)
	go func() { childExited <- child.Wait() }()
	childReaped := false
	defer func() {
		if !childReaped {
			_ = syscall.Kill(-child.Process.Pid, syscall.SIGKILL)
			<-childExited
		}
	}()
	progress, err := workqueue.NewProgressSource(pool, schema)
	if err != nil {
		t.Fatal(err)
	}
	var chunkID int64
	for chunkID == 0 {
		select {
		case err := <-childExited:
			childReaped = true
			t.Fatalf("worker exited before a chunk was encoding: %v: %s", err, childOutput.String())
		default:
		}
		if err := pool.QueryRow(ctx, `SELECT COALESCE((SELECT id FROM `+schema+`.river_job
WHERE kind = $1 AND state = 'running' AND args->'ref'->>'content_id' = $2 LIMIT 1), 0)`,
			(workqueue.VideoChunkArgs{}).Kind(), e.ref.ContentID).Scan(&chunkID); err != nil {
			t.Fatal(err)
		}
		if chunkID != 0 {
			status, err := progress.EncodeProgress(ctx, e.ref)
			if err != nil {
				t.Fatal(err)
			}
			if status.Files["source"].Phase != media.PhaseEncoding || status.Files["source"].Percent == 0 {
				chunkID = 0
			}
		}
		if chunkID == 0 {
			select {
			case <-time.After(20 * time.Millisecond):
			case <-ctx.Done():
				t.Fatal("no chunk made encode progress before the deadline")
			}
		}
	}
	if err := syscall.Kill(-child.Process.Pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	waitErr := <-childExited
	childReaped = true
	if waitErr == nil {
		t.Fatal("hard-killed worker exited successfully")
	}
	var state string
	if err := pool.QueryRow(ctx, `SELECT state FROM `+schema+`.river_job WHERE id = $1`, chunkID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "running" {
		t.Fatalf("hard-killed chunk state %q, want running", state)
	}
	if _, err := pool.Exec(ctx, `UPDATE `+schema+`.river_job SET attempted_at = now() - interval '3 hours' WHERE id = $1`, chunkID); err != nil {
		t.Fatal(err)
	}
	rescuerContribution, err := video.Contribution(wc)
	if err != nil {
		t.Fatal(err)
	}
	rescuerConfig := video.ClientConfig(wc)
	rescuerConfig.FetchPollInterval = 100 * time.Millisecond
	rescuer, err := riverhelpers.New(ctx, pool, rescuerConfig, rescuerContribution)
	if err != nil {
		t.Fatal(err)
	}
	if err := rescuer.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() {
		stopCtx, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		_ = rescuer.StopAndCancel(stopCtx)
	}()
	for {
		m, _ := e.manifest(t)
		if h := m.Files[0].HLS; h != nil && len(h.Video) == 2 && h.Sprite != nil &&
			len(m.Downloads) > 0 && len(h.Pending) == 0 && m.Files[0].State() == media.StateReady {
			break
		}
		select {
		case <-time.After(100 * time.Millisecond):
		case <-ctx.Done():
			t.Fatal("River did not rescue and finish the hard-killed chunk")
		}
	}
	recovered, _ := e.manifest(t)
	var attempt int
	var rescued bool
	if err := pool.QueryRow(ctx, `SELECT attempt, EXISTS (
  SELECT 1 FROM unnest(errors) AS entry WHERE entry->>'error' = 'Stuck job rescued by JobRescuer'
) FROM `+schema+`.river_job WHERE id = $1`, chunkID).Scan(&attempt, &rescued); err != nil {
		t.Fatal(err)
	}
	if !rescued || attempt != 1 {
		t.Fatalf("hard-killed chunk rescued %v, attempt %d; want rescue with one working attempt", rescued, attempt)
	}
	outputHashes := func(m *media.Manifest) map[string]string {
		hashes := make(map[string]string)
		for _, rendition := range m.Files[0].HLS.Video {
			hashes[fmt.Sprintf("%d-%s", rendition.Rung, rendition.Codec)] = rendition.Blob
		}
		hashes["sprite"] = m.Files[0].HLS.Sprite.Blob
		for name, download := range m.Downloads {
			hashes[name] = download.Blob
		}
		return hashes
	}
	if want, got := outputHashes(baseline), outputHashes(recovered); !maps.Equal(got, want) {
		t.Fatalf("recovered output hashes %v, want %v", got, want)
	}
}

func runHardKilledChunkWorkerChild(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Pool(t, nil)
	var cfg mediaS3.Config
	if err := json.Unmarshal([]byte(os.Getenv("CONTENTKIT_HARDKILL_S3_CONFIG")), &cfg); err != nil {
		t.Fatal(err)
	}
	store, err := mediaS3.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	kinds, err := media.NewRegistry(media.Kind{Name: "video", Versioned: true,
		Video: &media.Video{PosterWidths: posterWidths}, Types: []string{"video/x-matroska", "video/mp4"}})
	if err != nil {
		t.Fatal(err)
	}
	enc, err := video.New(ctx, video.Config{Store: store, Locker: media.PGLocker(pool), TempDir: os.Getenv("CONTENTKIT_HARDKILL_TEMP_DIR"),
		Threads: 2, Encoder: video.EncoderCPU, Codecs: []media.Codec{media.CodecH264}, ProgressInterval: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	wc := video.WorkerConfig{Encoder: enc, Pool: pool, Schema: os.Getenv("CONTENTKIT_HARDKILL_SCHEMA"), Kinds: kinds,
		ChunkTarget: 8 * time.Second}
	contribution, err := video.Contribution(wc)
	if err != nil {
		t.Fatal(err)
	}
	clientConfig := video.ClientConfig(wc)
	clientConfig.FetchPollInterval = 100 * time.Millisecond
	client, err := riverhelpers.New(ctx, pool, clientConfig, contribution)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Start(ctx); err != nil {
		t.Fatal(err)
	}
	select {}
}
