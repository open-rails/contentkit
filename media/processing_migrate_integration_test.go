package media_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	riverhelpers "github.com/open-rails/helpers/river"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/media"
)

func TestProcessingMigrationRetiresOwnedJobsAndBackfillsOnce(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	pool := f.env.Pool()
	f.visible(1)
	ref := f.gallery(1, 1)
	f.q.take()
	host := pgtest.EmptySchema(t, ctx, pool)
	if err := riverhelpers.ApplyMigrations(ctx, pool, host); err != nil {
		t.Fatal(err)
	}
	jobs := pgx.Identifier{host, "river_job"}.Sanitize()
	insert := func(kind, state string, ref contentref.ContentRef) int64 {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"ref": ref})
		var id int64
		var finalized *time.Time
		if state == "completed" {
			at := time.Now()
			finalized = &at
		}
		err := pool.QueryRow(ctx, `INSERT INTO `+jobs+` (kind,args,max_attempts,queue,state,finalized_at) VALUES($1,$2,5,'contentkit_media',$3,$4) RETURNING id`, kind, body, state, finalized).Scan(&id)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	waiting := insert("contentkit_media_expose", "available", ref)
	running := insert("contentkit_media_ready", "running", ref)
	foreign := insert("contentkit_media_ready", "available", contentref.New("foreign", "gallery", ref.ContentID))
	history := insert("contentkit_media_ready", "completed", ref)
	publications := 0
	publish := func(context.Context, pgx.Tx, contentref.ContentRef) error { publications++; return nil }
	if err := media.MigrateProcessing(ctx, pool, f.reg, f.journal, host, f.q, publish); err == nil || !strings.Contains(err.Error(), "stop old workers") {
		t.Fatalf("running old job accepted: %v", err)
	}
	state := func(id int64) string {
		t.Helper()
		var state string
		if err := pool.QueryRow(ctx, `SELECT state FROM `+jobs+` WHERE id=$1`, id).Scan(&state); err != nil {
			t.Fatal(err)
		}
		return state
	}
	if state(waiting) != "available" {
		t.Fatal("failed migration retired earlier waiting work")
	}
	if err := media.ValidateProcessingMigration(ctx, pool, f.reg, host); err == nil {
		t.Fatal("old queue passed startup validation")
	}
	if _, err := pool.Exec(ctx, `UPDATE `+jobs+` SET state='available' WHERE id=$1`, running); err != nil {
		t.Fatal(err)
	}
	publications = 0
	for range 2 {
		if err := media.MigrateProcessing(ctx, pool, f.reg, f.journal, host, f.q, publish); err != nil {
			t.Fatal(err)
		}
	}
	if state(waiting) != "cancelled" || state(running) != "cancelled" || state(foreign) != "available" || state(history) != "completed" {
		t.Fatal("retirement changed the wrong lifecycle jobs")
	}
	if publications != 2 {
		t.Fatalf("real metadata wakeups=%d", publications)
	}
	if err := media.ValidateProcessingMigration(ctx, pool, f.reg, host); err != nil {
		t.Fatal(err)
	}
	if work := f.q.take(); len(work) != 1 || work[0].Ref != ref {
		t.Fatalf("backfill processing=%+v", work)
	}
	// The one-time receipt survives after the queued job is no longer pending.
	if err := media.MigrateProcessing(ctx, pool, f.reg, f.journal, host, f.q, publish); err != nil {
		t.Fatal(err)
	}
	if work := f.q.take(); len(work) != 0 {
		t.Fatalf("migration requeued completed backfill=%+v", work)
	}
}
