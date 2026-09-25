package video_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/workqueue"
)

// A host import (legacy migration) streams a source through Uploads.Ingest;
// its commit enqueues the encode on media_worker, and the encode plays.
func TestIngestEnqueuesEncode(t *testing.T) {
	requireFFmpeg(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	pool := pgtest.Pool(t, nil)
	if err := workqueue.Migrate(ctx, pool, workqueue.Schema); err != nil {
		t.Fatal(err)
	}
	var enq *workqueue.Queue
	e := newEnv(t, nil, queueFunc(func(ctx context.Context, j media.ProcessJob) error { return enq.Enqueue(ctx, j) }))
	enq, err := workqueue.New(pool, e.kinds, workqueue.Schema)
	if err != nil {
		t.Fatal(err)
	}

	src, err := os.Open(fixture{w: 640, h: 360, secs: 4, audio: 1, tone: 440}.make(t))
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	res, err := e.uploads.Ingest(ctx, admin, media.IngestRequest{Ref: e.ref, Name: "source", Type: "video/x-matroska", Body: src})
	if err != nil {
		t.Fatal(err)
	}

	var encodes int
	for encodes == 0 {
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+workqueue.Schema+`.river_job
			WHERE kind = $1 AND args->'ref'->>'tenant_id' = $2`, workqueue.VideoArgs{}.Kind(), e.Tenant).Scan(&encodes); err != nil {
			t.Fatal(err)
		}
		select {
		case <-ctx.Done():
			t.Fatal("the ingest's commit enqueued no encode on " + workqueue.Schema)
		case <-time.After(100 * time.Millisecond):
		}
	}
	if encodes != 1 {
		t.Fatalf("%d encode jobs for one ingest", encodes)
	}
	e.encode(t)
	m, _ := e.manifest(t)
	if h := m.Files[0].HLS; h == nil || h.Source != res.Original || len(h.Video) == 0 || len(m.Downloads) != 1 {
		t.Fatalf("ingested source did not encode: %+v", m)
	}
}
