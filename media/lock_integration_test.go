package media_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/media"
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
