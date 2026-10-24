package video_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	riverhelpers "github.com/open-rails/helpers/river"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/open-rails/contentkit/media/video"
	"github.com/open-rails/contentkit/media/workqueue"
)

type stopAtCheckpoint struct {
	river.HookDefaults
	stop context.CancelFunc
}

func (h *stopAtCheckpoint) WorkEnd(_ context.Context, job *rivertype.JobRow, err error) error {
	var snooze *river.JobSnoozeError
	if job.Kind == (workqueue.VideoArgs{}).Kind() && errors.As(err, &snooze) {
		h.stop()
	}
	return err
}

// A completed chunk survives a stopped worker. Its one owning job stays
// available, so KEDA can observe it without a running River scheduler.
func TestVideoCheckpointRemainsRunnableAfterWorkerStops(t *testing.T) {
	testVideoCheckpoint(t, false)
}

func TestVideoRestoresExpiredCheckpoint(t *testing.T) {
	testVideoCheckpoint(t, true)
}

func testVideoCheckpoint(t *testing.T, expire bool) {
	e := newEnv(t, opts{chunk: 2 * time.Second, ladder: []int{360}})
	e.put("source", "video/x-matroska", fixture{w: 640, h: 360, secs: 24, rate: 10}.make(t), nil)
	runCtx, stop := context.WithCancel(e.ctx)
	defer stop()
	contribution, err := video.Contribution(e.wc)
	if err != nil {
		t.Fatal(err)
	}
	cfg := video.ClientConfig(e.wc)
	cfg.Hooks = []rivertype.Hook{&stopAtCheckpoint{stop: stop}}
	client, err := riverhelpers.New(e.ctx, e.pool, cfg, contribution)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Start(runCtx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-client.Stopped():
	case <-time.After(2 * time.Minute):
		t.Fatal("worker did not stop at checkpoint")
	}
	var jobID int64
	var state string
	var attempt, chunks int
	if err := e.pool.QueryRow(e.ctx, `SELECT id,state,attempt FROM `+e.schema+`.river_job WHERE kind=$1`, (workqueue.VideoArgs{}).Kind()).Scan(&jobID, &state, &attempt); err != nil {
		t.Fatal(err)
	}
	if state != "available" || attempt != 0 {
		t.Fatalf("checkpoint job %d is %s attempt%d", jobID, state, attempt)
	}
	var output []byte
	if err := e.pool.QueryRow(e.ctx, `SELECT count(*),min(output::text)::jsonb FROM `+e.schema+`.encode_chunk WHERE state='done'`).Scan(&chunks, &output); err != nil || chunks != 1 {
		t.Fatalf("completed ranges %d: %v", chunks, err)
	}
	var objects map[string]string
	if err := json.Unmarshal(output, &objects); err != nil {
		t.Fatal(err)
	}
	if expire {
		for _, key := range objects {
			if err := e.store.Delete(e.ctx, key); err != nil {
				t.Fatal(err)
			}
		}
	}
	e.start()
	e.wait()
	for _, key := range objects {
		if _, err := e.store.Head(e.ctx, key); err != nil {
			t.Fatalf("checkpoint missing after resume: %v", err)
		}
	}
	var after []byte
	if err := e.pool.QueryRow(e.ctx, `SELECT output FROM `+e.schema+`.encode_chunk WHERE ordinal=0`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if string(after) != string(output) {
		t.Fatal("resuming replaced a completed checkpoint")
	}
	if r := e.readiness(e.manifest()); !r.Ready() {
		t.Fatalf("readiness %+v", r)
	}
	var ids []int64
	rows, err := e.pool.Query(e.ctx, `SELECT id FROM `+e.schema+`.river_job WHERE kind=$1`, (workqueue.VideoArgs{}).Kind())
	if err != nil {
		t.Fatal(err)
	}
	ids, err = pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil || len(ids) != 1 || ids[0] != jobID {
		t.Fatalf("conversion jobs %v: %v", ids, err)
	}
}
