package workqueue

import (
	"context"
	"testing"

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
	if queue != VideoLightQueue {
		t.Fatalf("queued video job moved to %q, want %q", queue, VideoLightQueue)
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
