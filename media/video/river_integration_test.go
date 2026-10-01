package video_test

import (
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/riverqueue/river/rivertype"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/video"
	"github.com/open-rails/contentkit/media/workqueue"
)

// runningChunk waits for a chunk of ref making encode progress and returns its job id.
func (e *env) runningChunk(ref contentref.ContentRef, exited <-chan error, out func() string) int64 {
	e.t.Helper()
	progress, err := workqueue.NewProgressSource(e.pool, e.schema)
	if err != nil {
		e.t.Fatal(err)
	}
	deadline := time.After(5 * time.Minute)
	for {
		select {
		case err := <-exited:
			e.t.Fatalf("worker exited before a chunk was encoding: %v: %s", err, out())
		case <-deadline:
			e.t.Fatal("no chunk made encode progress")
		case <-time.After(20 * time.Millisecond):
		}
		var id int64
		if err := e.pool.QueryRow(e.ctx, `SELECT COALESCE((SELECT id FROM `+e.schema+`.river_job
WHERE kind = $1 AND state = 'running' AND args->'ref'->>'content_id' = $2 LIMIT 1), 0)`,
			(workqueue.VideoChunkArgs{}).Kind(), ref.ContentID).Scan(&id); err != nil {
			e.t.Fatal(err)
		}
		if id == 0 {
			continue
		}
		st, err := progress.EncodeProgress(e.ctx, ref)
		if err != nil {
			e.t.Fatal(err)
		}
		if p := st.Files["source.mkv"]; p.Phase == media.PhaseEncoding && p.Percent > 0 {
			return id
		}
	}
}

func outputBlobs(m *media.Manifest) map[string]string {
	out := map[string]string{}
	for _, f := range m.Files {
		if !f.IsUpload() {
			out[f.Path] = f.Blob
		}
	}
	return out
}

// A chunk interrupted by a worker shutdown is snoozed without an attempt,
// and rescues recorded before Work runs do not spend the retry budget.
func TestInterruptedChunkRetriesWithoutAttempt(t *testing.T) {
	e := newEnv(t, opts{})
	e.put("source", "video/x-matroska", fixture{w: 1280, h: 720, secs: 120, rate: 10}.make(t), nil)
	e.start()
	chunk := e.runningChunk(e.ref, nil, nil)
	e.stopWorker()
	var attempt, maxAttempts int
	var state string
	if err := e.pool.QueryRow(e.ctx, `SELECT attempt, max_attempts, state FROM `+e.schema+`.river_job WHERE id = $1`, chunk).Scan(&attempt, &maxAttempts, &state); err != nil {
		t.Fatal(err)
	}
	if attempt != 0 || state != "available" || maxAttempts != workqueue.VideoRiverMaxAttempts {
		t.Fatalf("interrupted chunk attempt %d of %d, state %q", attempt, maxAttempts, state)
	}
	// Five hard kills before a worker reaches Work: River records each
	// rescue as an error and would discard the fifth with MaxAttempts = 5.
	rescue, err := json.Marshal(rivertype.AttemptError{At: time.Now(), Attempt: 1, Error: "Stuck job rescued by JobRescuer"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(e.ctx, `UPDATE `+e.schema+`.river_job SET attempt = 5, errors = array_fill($2::jsonb, ARRAY[5]) WHERE id = $1`, chunk, rescue); err != nil {
		t.Fatal(err)
	}
	e.start()
	e.wait()
	if r := e.readiness(e.manifest()); !r.Ready() {
		t.Fatalf("readiness %+v", r)
	}
	if err := e.pool.QueryRow(e.ctx, `SELECT attempt FROM `+e.schema+`.river_job WHERE id = $1`, chunk).Scan(&attempt); err != nil || attempt != 1 {
		t.Fatalf("rescued chunk took %d attempts (%v), want 1", attempt, err)
	}
}

// A short video of another namespace's item is published before a long one
// queued ahead of it finishes: chunks share the workers between tenants.
func TestShortVideoOvertakesLongVideo(t *testing.T) {
	e := newEnv(t, opts{chunk: 8 * time.Second, shared: true})
	long := e.ref
	e.put("source", "video/x-matroska", fixture{w: 1280, h: 720, secs: 40, rate: 10}.make(t), nil)
	short := e.refOf("clip", 2)
	e.ref = short
	e.put("source", "video/x-matroska", fixture{w: 640, h: 360, secs: 5, rate: 10}.make(t), nil)
	e.start()
	for {
		e.next((workqueue.VideoAssembleArgs{}).Kind())
		m, _, err := e.ms.Get(e.ctx, short)
		if err != nil {
			t.Fatal(err)
		}
		if len(m.Outputs("source.mkv", "hls")) == 0 {
			continue
		}
		l, _, err := e.ms.Get(e.ctx, long)
		if err != nil {
			t.Fatal(err)
		}
		if k, _ := e.reg.Kind("video"); k.Readiness(l).Ready() {
			t.Fatal("the long video finished before the short one")
		}
		return
	}
}

// Cancel cancels an item's queued image, video and audio jobs.
func TestCancelJobs(t *testing.T) {
	e := newEnv(t, opts{})
	other := e.refOf("video", 99)
	for _, ref := range []contentref.ContentRef{e.ref, e.ref, other} {
		if err := e.queue.Enqueue(e.ctx, media.ProcessJob{Ref: ref}); err != nil {
			t.Fatal(err)
		}
	}
	// Each enqueue queues a video and an audio job, and one pending image job.
	if n, err := e.queue.Cancel(e.ctx, e.ref); err != nil || n != 5 {
		t.Fatalf("cancelled %d: %v", n, err)
	}
	rows, err := e.pool.Query(e.ctx, "SELECT state FROM "+e.schema+".river_job ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	var states []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		states = append(states, s)
	}
	if want := []string{"cancelled", "cancelled", "cancelled", "cancelled", "cancelled", "available", "available", "available"}; !slices.Equal(states, want) {
		t.Fatalf("states %v", states)
	}
}

// An item deleted while its outputs are made stays deleted: the outputs
// written for it are dropped and its manifest is not recreated.
func TestDeletedFolderDropsInFlightOutputs(t *testing.T) {
	e := newEnv(t, opts{ladder: []int{360}})
	var once sync.Once
	defer video.SetBeforePublish(func() { once.Do(func() { e.drop(e.item().Prefix()) }) })()
	e.start()
	defer e.stopWorker()
	e.put("source", "video/x-matroska", fixture{w: 640, h: 360, secs: 3, audio: 1, tone: 440}.make(t), nil)
	e.wait()
	if _, _, err := e.ms.Get(e.ctx, e.ref); !errors.Is(err, media.ErrNotFound) {
		t.Fatalf("manifest after deletion: %v", err)
	}
	for o, err := range e.store.List(e.ctx, e.item().PrivatePrefix()) {
		if err != nil {
			t.Fatal(err)
		}
		t.Fatalf("%s kept after the folder was deleted", o.Key)
	}
}
