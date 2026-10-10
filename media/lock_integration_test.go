package media_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	riverhelpers "github.com/open-rails/helpers/river"
	"github.com/riverqueue/river"

	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/internal/s3test"
)

// An upload commit settles quota inside the manifest edit. Under the PGLocker
// that must not deadlock a pool whose every connection the lock could take.
func TestPGLockerLeavesPoolToTheEdit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := pgtest.Pool(t, func(c *pgxpool.Config) { c.MaxConns = 1 })
	schema := pgtest.Schema(t, ctx, pool)
	limiter, err := media.NewPGLimiter(pool, schema, media.PGLimits{})
	if err != nil {
		t.Fatal(err)
	}
	locker := media.PGLocker(pool)
	edit, cancelEdit := context.WithTimeout(ctx, 5*time.Second)
	defer cancelEdit()
	unlock, err := locker.Lock(edit, "manifest")
	if err != nil {
		t.Fatal(err)
	}
	if err := limiter.Settle(edit, media.Settlement{Tenant: "t", Owner: "u", Delta: 10}); err != nil {
		t.Fatalf("settle under the lock: %v", err)
	}
	// A second editor waits for the lock, then proceeds.
	acquired := make(chan error, 1)
	go func() {
		unlock, err := locker.Lock(ctx, "manifest")
		if err == nil {
			unlock()
		}
		acquired <- err
	}()
	select {
	case err := <-acquired:
		t.Fatalf("lock not exclusive: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	unlock()
	if err := <-acquired; err != nil {
		t.Fatal(err)
	}
	used, _, err := limiter.Usage(ctx, "t", "u")
	if err != nil || used != 10 {
		t.Fatalf("used=%d err=%v", used, err)
	}
}

func TestDeleteItemsTxWithSingleConnectionPool(t *testing.T) {
	env := s3test.Open(t)
	pool := pgtest.Pool(t, func(c *pgxpool.Config) { c.MaxConns = 1 })
	schema := pgtest.Schema(t, t.Context(), pool)
	journal, err := media.NewPGJournal(pool, schema, nil)
	if err != nil {
		t.Fatal(err)
	}
	reg := miniRegistry(t, env.Tenant)
	jobs, err := media.NewJobs(media.JobsConfig{Store: env.Store, Registry: reg, Locker: media.PGLocker(pool), Journal: journal})
	if err != nil {
		t.Fatal(err)
	}
	riverSchema := pgtest.EmptySchema(t, t.Context(), pool)
	if err := riverhelpers.ApplyMigrations(t.Context(), pool, riverSchema); err != nil {
		t.Fatal(err)
	}
	if _, err := riverhelpers.New(t.Context(), pool, &river.Config{Schema: riverSchema}, jobs.RiverJobs()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	ref, _ := reg.Ref("post", cid(7))
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		return jobs.DeleteItemsTx(ctx, tx, media.Deletion{Ref: ref})
	}); err != nil {
		t.Fatalf("delete empty item while the host transaction holds the only pooled connection: %v", err)
	}
	cur, _, err := jobs.Manifests().Get(ctx, ref)
	if err != nil || cur.Incarnation == "" || !cur.Hidden || len(cur.Files) != 0 {
		t.Fatalf("empty deletion lifetime was not captured: %+v %v", cur, err)
	}
	var incarnation string
	if err := pool.QueryRow(ctx, "SELECT args->>'incarnation' FROM "+pgx.Identifier{riverSchema, "river_job"}.Sanitize()+" WHERE kind='contentkit_media_delete_folder'").Scan(&incarnation); err != nil || incarnation != cur.Incarnation {
		t.Fatalf("queued wrong incarnation: %q %v", incarnation, err)
	}
}
