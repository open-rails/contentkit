package video_test

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/video"
)

// A duplicate plan (an editor read or a second commit enqueues one) whose
// manifest snapshot predates the publish that settled the source must not
// mark it pending again: its outputs are current, so nothing would clear it.
func TestDuplicatePlanAcrossPublishLeavesNothingPending(t *testing.T) {
	e := newEnv(t, opts{ladder: []int{360}})
	e.wc.MaxWorkers = 2 // the duplicate plans while the first run's assemble waits for it
	var (
		once     sync.Once
		armed    atomic.Bool
		snapshot = make(chan struct{})
		settled  = make(chan struct{})
	)
	defer video.SetPlanSnapshot(func() {
		if !armed.CompareAndSwap(true, false) {
			return
		}
		close(snapshot)
		select { // plan from the stale snapshot only once the publish committed
		case <-settled:
		case <-time.After(2 * time.Minute):
		}
	})()
	// The plan's own publishes (its poster frame) run under the item's plan
	// lock; only the run's assemble publishes with it free.
	planLockFree := func() bool {
		c, err := e.pool.Acquire(e.ctx)
		if err != nil {
			return false
		}
		defer c.Release()
		key := "contentkit:media:video:" + e.ref.String()
		var ok bool
		if err := c.QueryRow(e.ctx, "SELECT pg_try_advisory_lock(hashtextextended($1, 0))", key).Scan(&ok); err != nil || !ok {
			return false
		}
		_, _ = c.Exec(e.ctx, "SELECT pg_advisory_unlock(hashtextextended($1, 0))", key)
		return true
	}
	defer video.SetBeforePublish(func() {
		if !planLockFree() {
			return
		}
		once.Do(func() {
			armed.Store(true)
			if err := e.queue.Enqueue(e.ctx, media.ProcessJob{Ref: e.ref}); err != nil {
				t.Error(err)
				return
			}
			select {
			case <-snapshot:
			case <-time.After(2 * time.Minute):
				t.Error("the duplicate plan never read the manifest")
				return
			}
			go func() { // this publish's edit follows the hook
				defer close(settled)
				for deadline := time.Now().Add(2 * time.Minute); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
					if m, _, err := e.ms.Get(e.ctx, e.ref); err == nil && len(m.Outputs("source.mkv", "hls")) > 0 {
						return
					}
				}
			}()
		})
	})()
	e.start()
	defer e.stopWorker()
	e.put("source", "video/x-matroska", fixture{w: 640, h: 360, secs: 3, audio: 1, tone: 440}.make(t), nil)
	e.wait()

	select {
	case <-snapshot:
	default:
		t.Fatal("no duplicate plan planned across the publish")
	}
	m := e.manifest()
	if f := e.file(m, "source.mkv"); len(f.Pending) != 0 {
		t.Fatalf("source pending %v after its outputs were published", f.Pending)
	}
	if r := e.readiness(m); !r.Ready() {
		t.Fatalf("readiness %+v", r)
	}
}
