package publishing

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"image"
	"image/png"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/contentkit/internal/pgtest"
	riverhelpers "github.com/open-rails/helpers/river"
	"github.com/riverqueue/river"
)

func TestPublishingRecoveryRecreatesOnlyNeededMonitors(t *testing.T) {
	ctx := t.Context()
	pool := pgtest.Pool(t, nil)
	schema := pgtest.EmptySchema(t, ctx, pool)
	if err := riverhelpers.ApplyMigrations(ctx, pool, schema); err != nil {
		t.Fatal(err)
	}
	table := pgx.Identifier{schema, "operations"}.Sanitize()
	_, err := pool.Exec(ctx, `CREATE TABLE `+table+` (id text PRIMARY KEY,tenant_id text,state text,failure jsonb,lease_until timestamptz,publish_at timestamptz,reconcile_at timestamptz)`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO `+table+` (id,tenant_id,state,publish_at,reconcile_at) VALUES
 ('processing','tenant','processing',NULL,now()),
 ('review','tenant','review_required',NULL,now()),
 ('due','tenant','scheduled',now()-interval '1 minute',now()),
 ('future','tenant','scheduled',now()+interval '1 day',now()),
 ('done','tenant','published',NULL,now())`)
	if err != nil {
		t.Fatal(err)
	}
	s := &Store{o: Options{Pool: pool, Tenant: "tenant", Clock: time.Now}, table: table}
	_, err = riverhelpers.New(ctx, pool, &river.Config{Schema: schema}, s.RiverJobs())
	if err != nil {
		t.Fatal(err)
	}
	jobs := pgx.Identifier{schema, "river_job"}.Sanitize()
	count := func(want int) {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+jobs+` WHERE kind='contentkit_publishing_operation'`).Scan(&n); err != nil || n != want {
			t.Fatalf("monitors=%d, want %d: %v", n, want, err)
		}
	}
	for range 2 {
		if err := s.Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
		count(3) // No idle, far-future, completed or duplicate monitor.
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.enqueueOperation(ctx, tx, "rolled-back-upload"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	count(3)
	_, err = pool.Exec(ctx, `UPDATE `+jobs+` SET state='discarded',finalized_at=now() WHERE args->>'id'='processing'`)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	count(4) // Recovery replaces a missing/terminal monitor without duplicating live ones.
	_, err = pool.Exec(ctx, `INSERT INTO `+table+` (id,tenant_id,state,reconcile_at)
 SELECT 'pending-' || n, 'tenant', 'processing', now()-interval '1 hour' FROM generate_series(1,30) n`)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	count(29)
	if err := s.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	count(34) // Stopped/retrying first-batch workers cannot starve later monitors.
}

func TestCompletedUploadStartsMonitorWithoutPeriodicRecovery(t *testing.T) {
	f := newFixture(t, func(c *pgxpool.Config) { c.MaxConns = 8 })
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	schema := pgtest.EmptySchema(t, ctx, f.s.o.Pool)
	if err := riverhelpers.ApplyMigrations(ctx, f.s.o.Pool, schema); err != nil {
		t.Fatal(err)
	}
	client, err := riverhelpers.New(ctx, f.s.o.Pool, &river.Config{Schema: schema, FetchCooldown: 50 * time.Millisecond, FetchPollInterval: 100 * time.Millisecond},
		f.jobs.RiverJobs(), f.s.o.Catalog.RiverJobs(), f.s.o.Versions.RiverJobs(), f.s.RiverJobs())
	if err != nil {
		t.Fatal(err)
	}
	client.PeriodicJobs().Clear()
	if err := client.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.StopAndCancel(context.Background()) })
	var archive bytes.Buffer
	zipper := zip.NewWriter(&archive)
	page, err := zipper.Create("001.png")
	if err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(page, image.NewGray(image.Rect(0, 0, 8, 8))); err != nil {
		t.Fatal(err)
	}
	if err := zipper.Close(); err != nil {
		t.Fatal(err)
	}
	in := request()
	in.Archive.Size = int64(archive.Len())
	hash := sha256.Sum256(archive.Bytes())
	in.Archive.SHA256 = hex.EncodeToString(hash[:])
	op, err := f.s.Put(ctx, f.actor, uuid.NewString(), in)
	if err != nil {
		t.Fatal(err)
	}
	if op.Upload == nil || op.Upload.Put == nil {
		t.Fatal("expected a direct upload")
	}
	put, err := http.NewRequestWithContext(ctx, op.Upload.Put.Method, op.Upload.Put.URL, bytes.NewReader(archive.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	for key, value := range op.Upload.Put.Headers {
		put.Header.Set(key, value)
	}
	response, err := http.DefaultClient.Do(put)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("upload status %d", response.StatusCode)
	}
	if _, err := f.s.Complete(ctx, f.actor, op.ID); err != nil {
		t.Fatal(err)
	}
	// No media processor is running: rendering is real pending work. The host
	// monitor must nevertheless execute now, without the recovery timer, and
	// retain one scheduled job while it awaits the queued archive import.
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	jobs := pgx.Identifier{schema, "river_job"}.Sanitize()
	for {
		var ran bool
		if err := f.s.o.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM `+jobs+` j JOIN `+f.s.table+` o ON o.id::text=j.args->>'id'
 WHERE j.kind='contentkit_publishing_operation' AND j.args->>'id'=$1 AND coalesce((j.metadata->>'snoozes')::int,0)>0
 AND j.state IN ('available','scheduled') AND o.progress->>'stage'='rendering')`, op.ID).Scan(&ran); err != nil {
			t.Fatal(err)
		}
		if ran {
			break
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatal("completed upload did not execute its monitor:", ctx.Err())
		}
	}
	if _, err := f.s.Complete(ctx, f.actor, op.ID); err != nil {
		t.Fatal(err)
	}
	var monitors int
	if err := f.s.o.Pool.QueryRow(ctx, `SELECT count(*) FROM `+jobs+` WHERE kind='contentkit_publishing_operation' AND args->>'id'=$1`, op.ID).Scan(&monitors); err != nil || monitors != 1 {
		t.Fatalf("idempotent completion left %d monitors: %v", monitors, err)
	}
}
