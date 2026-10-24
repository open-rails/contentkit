package video_test

import (
	"sync"
	"testing"

	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/video"
	"github.com/open-rails/contentkit/media/workqueue"
)

// A second conversion queued during publication serializes with the first,
// finds its outputs current and does not leave the source pending again.
func TestDuplicateConversionLeavesNothingPending(t *testing.T) {
	e := newEnv(t, opts{ladder: []int{360}})
	e.wc.MaxWorkers = 2
	var once sync.Once
	defer video.SetBeforePublish(func() {
		once.Do(func() {
			if err := e.queue.Enqueue(e.ctx, media.ProcessJob{Ref: e.ref}); err != nil {
				t.Error(err)
			}
		})
	})()
	e.start()
	defer e.stopWorker()
	e.put("source", "video/x-matroska", fixture{w: 640, h: 360, secs: 3, audio: 1, tone: 440}.make(t), nil)
	e.wait()
	m := e.manifest()
	if f := e.file(m, "source.mkv"); len(f.Pending) != 0 {
		t.Fatalf("pending %v", f.Pending)
	}
	if r := e.readiness(m); !r.Ready() {
		t.Fatalf("readiness %+v", r)
	}
	var jobs int
	if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM `+e.schema+`.river_job WHERE kind=$1 AND state='completed'`, (workqueue.VideoArgs{}).Kind()).Scan(&jobs); err != nil || jobs < 2 {
		t.Fatalf("completed conversion jobs %d: %v", jobs, err)
	}
}
