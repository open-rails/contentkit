package video_test

import (
	"context"
	"os"
	"testing"
	"time"

	riverhelpers "github.com/open-rails/helpers/river"
	"github.com/riverqueue/river"

	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/video"
)

// A host import (legacy migration) streams a source through Uploads.Ingest;
// its commit enqueues the encode on media_worker, and the encode plays.
func TestIngestEnqueuesEncode(t *testing.T) {
	requireFFmpeg(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	pool := pgtest.Pool(t, nil)
	if err := video.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	var jobs *media.Jobs
	e := newEnv(t, nil, queueFunc(func(ctx context.Context, j media.ProcessJob) error { return jobs.Enqueue(ctx, j) }))
	enq, err := video.NewEnqueuer(pool, e.kinds)
	if err != nil {
		t.Fatal(err)
	}
	if jobs, err = media.NewJobs(media.JobsConfig{Store: e.store, Kinds: e.kinds}); err != nil {
		t.Fatal(err)
	}
	if err := jobs.AddProcessor(enq.Processor()); err != nil {
		t.Fatal(err)
	}
	hostSchema := pgtest.EmptySchema(t, ctx, pool)
	if err := riverhelpers.ApplyMigrations(ctx, pool, hostSchema); err != nil {
		t.Fatal(err)
	}
	host, err := riverhelpers.New(ctx, pool, &river.Config{Schema: hostSchema, FetchPollInterval: 100 * time.Millisecond}, jobs.RiverJobs())
	if err != nil {
		t.Fatal(err)
	}
	if err := host.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() {
		stopCtx, c := context.WithTimeout(context.Background(), 30*time.Second)
		defer c()
		_ = host.StopAndCancel(stopCtx)
	}()

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
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+video.Schema+`.river_job
			WHERE kind = $1 AND args->'ref'->>'tenant_id' = $2`, video.Args{}.Kind(), e.Tenant).Scan(&encodes); err != nil {
			t.Fatal(err)
		}
		select {
		case <-ctx.Done():
			t.Fatal("the ingest's commit enqueued no encode on " + video.Schema)
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
