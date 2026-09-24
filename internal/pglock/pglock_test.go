package pglock

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/contentkit/internal/pgtest"
)

func TestHeldLockLeavesThePoolFree(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool := pgtest.Pool(t, func(c *pgxpool.Config) { c.MaxConns = 1 })
	release, ok, err := Acquire(ctx, pool, "pglock-test", false)
	if err != nil || !ok {
		t.Fatalf("acquire: %v %v", ok, err)
	}
	if _, err := pool.Exec(ctx, "SELECT 1"); err != nil {
		t.Fatalf("pool starved by the lock: %v", err)
	}
	if _, ok, err := Acquire(ctx, pool, "pglock-test", false); err != nil || ok {
		t.Fatalf("second holder: %v %v", ok, err)
	}
	release()
	again, ok, err := Acquire(ctx, pool, "pglock-test", false)
	if err != nil || !ok {
		t.Fatalf("released lock not free: %v %v", ok, err)
	}
	again()
}
