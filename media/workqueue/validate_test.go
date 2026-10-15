package workqueue

import (
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/contentkit/internal/pgtest"
)

func TestValidateWorkerReadOnlyAndLeavesQueuedJobs(t *testing.T) {
	ctx := t.Context()
	pool := pgtest.Pool(t, nil)
	schema := pgtest.EmptySchema(t, ctx, pool)
	if err := Validate(ctx, pool, schema); err == nil {
		t.Fatal("empty schema accepted")
	}
	if err := Migrate(ctx, pool, schema); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO `+jobs(schema)+` (kind,args,max_attempts,queue,state) VALUES ('contentkit_media_video','{}',5,'legacy_queue','available')`); err != nil {
		t.Fatal(err)
	}
	cfg := pool.Config()
	cfg.ConnConfig.RuntimeParams["default_transaction_read_only"] = "on"
	read, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer read.Close()
	if err := Validate(ctx, read, schema); err != nil {
		t.Fatalf("read-only validate: %v", err)
	}
	var queue string
	var attempts int
	if err := pool.QueryRow(ctx, `SELECT queue,max_attempts FROM `+jobs(schema)).Scan(&queue, &attempts); err != nil || queue != "legacy_queue" || attempts != 5 {
		t.Fatalf("validation rewrote job %s/%d: %v", queue, attempts, err)
	}
}
