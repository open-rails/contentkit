//go:build integration

package workqueue_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/workqueue"
)

func TestWorkerSchemasIsolateJobsAndProgress(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Pool(t, nil)
	first := pgtest.EmptySchema(t, ctx, pool)
	second := pgtest.EmptySchema(t, ctx, pool)
	for _, schema := range []string{first, second} {
		if err := workqueue.Migrate(ctx, pool, schema); err != nil {
			t.Fatal(err)
		}
	}
	kinds, err := media.NewRegistry(media.Kind{Name: "clip", Types: []string{"video/mp4"}, MaxBytes: 1 << 20, Video: &media.Video{}})
	if err != nil {
		t.Fatal(err)
	}
	firstQueue, err := workqueue.New(pool, kinds, first)
	if err != nil {
		t.Fatal(err)
	}
	secondQueue, err := workqueue.New(pool, kinds, second)
	if err != nil {
		t.Fatal(err)
	}
	ref := contentref.New("tenant", "clip", uuid.Must(uuid.NewV7()).String())
	for _, queue := range []*workqueue.Queue{firstQueue, secondQueue} {
		if err := queue.Enqueue(ctx, media.ProcessJob{Ref: ref}); err != nil {
			t.Fatal(err)
		}
	}

	if n, err := firstQueue.Cancel(ctx, ref); err != nil || n != 2 {
		t.Fatalf("cancel first schema: %d jobs, %v", n, err)
	}
	firstStatus, err := workqueue.NewProgressSource(pool, first).EncodeProgress(ctx, ref)
	if err != nil || firstStatus.Queued != nil {
		t.Fatalf("first schema still has queued work: %+v, %v", firstStatus, err)
	}
	secondStatus, err := workqueue.NewProgressSource(pool, second).EncodeProgress(ctx, ref)
	if err != nil || secondStatus.Queued == nil {
		t.Fatalf("second schema lost queued work: %+v, %v", secondStatus, err)
	}

	table := pgx.Identifier{second, "river_job"}.Sanitize()
	var id int64
	if err := pool.QueryRow(ctx, "UPDATE "+table+" SET state = 'running' WHERE kind = $1 RETURNING id", workqueue.VideoArgs{}.Kind()).Scan(&id); err != nil {
		t.Fatal(err)
	}
	progress := media.EncodeProgress{Phase: media.PhaseEncoding, At: time.Now().UnixMilli()}
	if err := workqueue.SetProgress(ctx, pool, second, id, map[string]media.EncodeProgress{"source.mp4": progress}); err != nil {
		t.Fatal(err)
	}
	secondStatus, err = workqueue.NewProgressSource(pool, second).EncodeProgress(ctx, ref)
	if err != nil || secondStatus.Files["source.mp4"].Phase != media.PhaseEncoding {
		t.Fatalf("second schema progress: %+v, %v", secondStatus, err)
	}
	if err := workqueue.ClearProgress(ctx, pool, second, id); err != nil {
		t.Fatal(err)
	}
	var hasProgress bool
	if err := pool.QueryRow(ctx, "SELECT metadata ? 'contentkit_progress' FROM "+table+" WHERE id = $1", id).Scan(&hasProgress); err != nil || hasProgress {
		t.Fatalf("clear second schema progress: %t, %v", hasProgress, err)
	}
}
