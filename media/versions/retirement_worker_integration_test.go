package versions_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/versions"
	"github.com/open-rails/contentkit/media/workqueue"
)

type publicationGuardStore struct {
	media.Store
	s       *site
	target  atomic.Value
	deleted atomic.Bool
	stale   atomic.Bool
}

func (s *publicationGuardStore) Delete(ctx context.Context, key string) error {
	target, _ := s.target.Load().(string)
	if target != "" && key == target {
		blob := key[strings.LastIndex(key, "/")+1:]
		var stale bool
		if err := s.s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM `+s.s.table("content_version_publications")+` WHERE tenant_id=$1 AND state='current' AND source::text LIKE '%'||$2||'%')`, s.s.tenant, blob).Scan(&stale); err != nil {
			return err
		}
		s.stale.Store(stale)
		s.deleted.Store(true)
	}
	return s.Store.Delete(ctx, key)
}

func TestStockWorkerWithdrawsSnapshotBeforeDeletingRendition(t *testing.T) {
	var guard *publicationGuardStore
	s := newSite(t, siteOptions{workerStore: func(s *site) media.Store { guard = &publicationGuardStore{Store: s.Store, s: s}; return guard }})
	v := s.version(s.content("gallery", `{"level":"public"}`))
	ref := s.ref("gallery", v)
	s.mustUpload(s.creator, ref, "originals/001", "image/png", pngOf(t, 30, 40, 1))
	// This test exercises the real image worker, not River's host publication
	// delivery. Publish explicitly so a delayed host queue cannot mask it.
	publish := func() {
		eventually(t, "ready media publication", 45*time.Second, func() bool {
			err := versions.Publish(s.svc, s.ctx, v)
			if errors.Is(err, versions.ErrNotReady) {
				return false
			}
			if err != nil {
				t.Fatal(err)
			}
			return true
		})
	}
	publish()
	s.plan(s.creator, v, func(p *versions.ReaderPlan) bool { return p.Pages != nil && *p.Pages == 1 })
	item, _ := s.reg.Item(ref)
	m := s.manifest(ref)
	files := m.Outputs("originals/001.png", "low")
	if len(files) != 1 {
		t.Fatalf("low rendition missing: %+v", files)
	}
	key, _ := item.Blob(files[0].Blob)
	guard.target.Store(key)
	queue, err := workqueue.New(s.pool, s.reg, s.workers)
	if err != nil {
		t.Fatal(err)
	}
	if err := queue.Enqueue(s.ctx, media.ProcessJob{Ref: ref, Force: true}); err != nil {
		t.Fatal(err)
	}
	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for !guard.deleted.Load() {
		select {
		case <-deadline.C:
			t.Fatal("worker did not retire old rendition")
		case <-tick.C:
		}
	}
	if guard.stale.Load() {
		t.Fatal("worker deleted a blob still offered by a current reader snapshot")
	}
	publish()
	s.plan(s.creator, v, func(p *versions.ReaderPlan) bool { return p.Pages != nil && *p.Pages == 1 })
}
