package workqueue

import (
	"context"
	"github.com/open-rails/migratekit"
	"testing"
	"testing/fstest"

	riverhelpers "github.com/open-rails/helpers/river"

	"github.com/open-rails/contentkit/internal/pgtest"
)

func TestMigrateMovesQueuedVideoJobs(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Pool(t, nil)
	schema := pgtest.EmptySchema(t, ctx, pool)
	if err := riverhelpers.ApplyMigrations(ctx, pool, schema); err != nil {
		t.Fatal(err)
	}
	var id int64
	if err := pool.QueryRow(ctx, `INSERT INTO `+jobs(schema)+`
  (kind, args, max_attempts, queue, state)
VALUES ('contentkit_media_video', '{}', 5, 'media_video', 'available') RETURNING id`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := Migrate(ctx, pool, schema); err != nil {
			t.Fatal(err)
		}
	}
	var queue string
	var maxAttempts int
	if err := pool.QueryRow(ctx, `SELECT queue, max_attempts FROM `+jobs(schema)+` WHERE id = $1`, id).Scan(&queue, &maxAttempts); err != nil {
		t.Fatal(err)
	}
	if queue != VideoQueue {
		t.Fatalf("queued video job moved to %q, want %q", queue, VideoQueue)
	}
	if maxAttempts != VideoRiverMaxAttempts {
		t.Fatalf("queued video job max attempts = %d, want %d", maxAttempts, VideoRiverMaxAttempts)
	}
	for _, table := range []string{"encode_run", "encode_chunk"} {
		var exists bool
		if err := pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, schema+"."+table).Scan(&exists); err != nil || !exists {
			t.Fatalf("%s exists = %t: %v", table, exists, err)
		}
	}
}

// Upgrading a partially encoded video preserves completed ranges and replaces
// child tasks with one resumable job, without touching completed River history.
func TestMigrateResumesVideoCheckpoints(t *testing.T) {
	ctx := t.Context()
	pool := pgtest.Pool(t, nil)
	schema := pgtest.EmptySchema(t, ctx, pool)
	if err := riverhelpers.ApplyMigrations(ctx, pool, schema); err != nil {
		t.Fatal(err)
	}
	script, err := migrationFiles.ReadFile("migrations/0001_video_runs.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := migratekit.Load(fstest.MapFS{"0001_video_runs.up.sql": &fstest.MapFile{Data: script}}, ".")
	if err != nil {
		t.Fatal(err)
	}
	migrator, err := migratekit.NewPostgresFromPGXPool(pool, "contentkit-media-worker")
	if err != nil {
		t.Fatal(err)
	}
	defer migrator.Close()
	if err := migrator.WithSchema(schema).ApplyMigrations(ctx, baseline); err != nil {
		t.Fatal(err)
	}
	var run string
	ref := `{"tenant_id":"tenant","content_kind":"video","content_id":"01920000-0000-7000-8000-000000000001"}`
	if err := pool.QueryRow(ctx, `INSERT INTO `+schema+`.encode_run (tenant_id,ref,file_name,source_name,source_key,source_etag,spec,rung,class,probe,state,released,completed)
 VALUES ('tenant',$1,'source.mp4','blob','key','etag','hls@fp',240,'','{}','encoding',2,1) RETURNING id::text`, ref).Scan(&run); err != nil {
		t.Fatal(err)
	}
	var child int64
	if err := pool.QueryRow(ctx, `INSERT INTO `+jobs(schema)+` (kind,args,max_attempts,queue,state)
 VALUES ('contentkit_media_video_chunk',jsonb_build_object('ref',$1::jsonb,'run_id',$2::text,'index',1),32767,'media_video_encode','available') RETURNING id`, ref, run).Scan(&child); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO `+schema+`.encode_chunk (run_id,ordinal,start_ms,end_ms,state,job_id,output)
 VALUES ($1,0,0,8000,'done',NULL,'{"240-h264":"temp/checkpoint.mp4"}'),($1,1,8000,16000,'queued',$2,NULL)`, run, child); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO `+jobs(schema)+` (kind,args,max_attempts,queue,state,finalized_at)
 VALUES ('contentkit_media_video_assemble','{}',32767,'media_video_light','completed',now())`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := Migrate(ctx, pool, schema); err != nil {
			t.Fatal(err)
		}
	}
	var active, retired, history int
	if err := pool.QueryRow(ctx, `SELECT
 count(*) FILTER (WHERE kind='contentkit_media_video' AND queue='media_video' AND state='available' AND args->'ref'=$1::jsonb),
 count(*) FILTER (WHERE id=$2 AND state='cancelled'),
 count(*) FILTER (WHERE kind='contentkit_media_video_assemble' AND state='completed') FROM `+jobs(schema), ref, child).Scan(&active, &retired, &history); err != nil || active != 1 || retired != 1 || history != 1 {
		t.Fatalf("video=%d retired=%d history=%d: %v", active, retired, history, err)
	}
	var done, planned int
	if err := pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE state='done' AND output->>'240-h264'='temp/checkpoint.mp4'), count(*) FILTER (WHERE state='planned' AND output IS NULL) FROM `+schema+`.encode_chunk WHERE run_id=$1`, run).Scan(&done, &planned); err != nil || done != 1 || planned != 1 {
		t.Fatalf("done=%d planned=%d: %v", done, planned, err)
	}
}
