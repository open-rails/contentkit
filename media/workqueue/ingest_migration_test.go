package workqueue

import (
	"testing"

	"github.com/open-rails/contentkit/internal/pgtest"
	riverhelpers "github.com/open-rails/helpers/river"
)

func TestIngestMigrationPreservesRequestAndTerminalHistory(t *testing.T) {
	ctx := t.Context()
	pool := pgtest.Pool(t, nil)
	schema := pgtest.EmptySchema(t, ctx, pool)
	if err := riverhelpers.ApplyMigrations(ctx, pool, schema); err != nil {
		t.Fatal(err)
	}
	var active, completed int64
	insert := func() int64 {
		t.Helper()
		var id int64
		if err := pool.QueryRow(ctx, `INSERT INTO `+jobs(schema)+`(kind,args,queue,max_attempts,metadata) VALUES('contentkit_media_place','{"ref":{"tenant_id":"site","content_kind":"post","content_id":"item"}}','media_place',5,'{"checkpoint":"retained"}') RETURNING id`).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	active = insert()
	completed = insert()
	if _, err := pool.Exec(ctx, `UPDATE `+jobs(schema)+` SET state='completed',finalized_at=now() WHERE id=$1`, completed); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := Migrate(ctx, pool, schema); err != nil {
			t.Fatal(err)
		}
	}
	var kind, queue, checkpoint, history string
	if err := pool.QueryRow(ctx, `SELECT kind,queue,metadata->>'checkpoint' FROM `+jobs(schema)+` WHERE id=$1`, active).Scan(&kind, &queue, &checkpoint); err != nil {
		t.Fatal(err)
	}
	if kind != "contentkit_media_ingest" || queue != IngestQueue || checkpoint != "retained" {
		t.Fatalf("request changed=%q/%q/%q", kind, queue, checkpoint)
	}
	if err := pool.QueryRow(ctx, `SELECT kind FROM `+jobs(schema)+` WHERE id=$1 AND state='completed'`, completed).Scan(&history); err != nil || history != "contentkit_media_place" {
		t.Fatalf("terminal history=%q err=%v", history, err)
	}
}
