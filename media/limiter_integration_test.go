package media_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/media"
)

func TestSettlementTransaction(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	pool := pgtest.Pool(t, func(cfg *pgxpool.Config) { cfg.MaxConns = 1 })
	limiter, err := media.NewPGLimiter(pool, pgtest.Schema(t, ctx, pool), media.PGLimits{
		Quota: func(ctx context.Context, _, _ string) (int64, error) {
			// The real host callback may query the same pool. Even with one
			// connection, resolving it before settlement must not block.
			var quota int64
			err := pool.QueryRow(ctx, "SELECT 100::bigint").Scan(&quota)
			return quota, err
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	reservation := media.Reservation{Tenant: "tenant", Owner: "owner", Key: "temp/upload", Size: 70}
	if err := limiter.Reserve(ctx, reservation); err != nil {
		t.Fatal(err)
	}
	other := reservation
	other.Tenant = "other"
	if err := limiter.Reserve(ctx, other); err != nil {
		t.Fatal(err)
	}
	quota, err := limiter.Quota(ctx, reservation.Tenant, reservation.Owner)
	if err != nil {
		t.Fatal(err)
	}
	settlement := media.Settlement{Tenant: reservation.Tenant, Owner: reservation.Owner,
		Keys: []string{reservation.Key}, Delta: reservation.Size, Enforce: true}
	rolledBack := errors.New("rollback after settlement")
	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if err := limiter.SettleTx(ctx, tx, settlement, quota); err != nil {
			return err
		}
		return rolledBack
	})
	if !errors.Is(err, rolledBack) {
		t.Fatal(err)
	}
	usage := func(tenant string, wantUsed, wantPending int64) {
		t.Helper()
		used, pending, err := limiter.Usage(ctx, tenant, reservation.Owner)
		if err != nil || used != wantUsed || pending != wantPending {
			t.Fatalf("%s: usage=%d pending=%d err=%v; want %d/%d", tenant, used, pending, err, wantUsed, wantPending)
		}
	}
	usage("tenant", 0, 70)
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		return limiter.SettleTx(ctx, tx, settlement, quota)
	}); err != nil {
		t.Fatal(err)
	}
	usage("tenant", 70, 0)
	usage("other", 0, 70)
	reservation.Key, reservation.Size = "temp/second", 30
	if err := limiter.Reserve(ctx, reservation); err != nil {
		t.Fatal(err)
	}
	settlement.Keys, settlement.Delta = []string{reservation.Key}, 40
	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		return limiter.SettleTx(ctx, tx, settlement, quota)
	})
	var refused *media.UploadError
	if !errors.As(err, &refused) || refused.Code != media.CodeQuota {
		t.Fatalf("over-quota settlement: %v", err)
	}
	usage("tenant", 70, 30)
	settlement.Delta = 30
	if err := limiter.Settle(ctx, settlement); err != nil {
		t.Fatal(err)
	}
	usage("tenant", 100, 0)
	settlement.Keys, settlement.Delta, settlement.Enforce = nil, -70, false
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		return limiter.SettleTx(ctx, tx, settlement, 0)
	}); err != nil {
		t.Fatal(err)
	}
	usage("tenant", 30, 0)
}

func TestQuotaReleaseAppliesOnce(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Pool(t, nil)
	limiter, err := media.NewPGLimiter(pool, pgtest.Schema(t, ctx, pool), media.PGLimits{})
	if err != nil {
		t.Fatal(err)
	}
	if err := limiter.Settle(ctx, media.Settlement{Tenant: "tenant", Owner: "owner", Delta: 100}); err != nil {
		t.Fatal(err)
	}
	release := media.QuotaRelease{Tenant: "tenant", Folder: "tenant/post/1/", Owner: "owner", Operation: "delete:1", Bytes: 70}
	if applied, err := limiter.PrepareRelease(ctx, release); err != nil || applied {
		t.Fatalf("prepare: applied=%t err=%v", applied, err)
	}
	used, _, err := limiter.Usage(ctx, "tenant", "owner")
	if err != nil || used != 100 {
		t.Fatalf("prepare changed usage: used=%d err=%v", used, err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- limiter.Release(ctx, "tenant", release.Operation)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	used, _, err = limiter.Usage(ctx, "tenant", "owner")
	if err != nil || used != 30 {
		t.Fatalf("replayed release: used=%d err=%v", used, err)
	}
	if applied, err := limiter.PrepareRelease(ctx, media.QuotaRelease{
		Tenant: "tenant", Folder: release.Folder, Owner: "owner", Operation: release.Operation,
	}); err != nil || !applied {
		t.Fatalf("prepare after an ambiguous success: applied=%t err=%v", applied, err)
	}
	if _, err := limiter.PrepareRelease(ctx, media.QuotaRelease{
		Tenant: "tenant", Folder: release.Folder, Owner: "other", Operation: release.Operation,
	}); err == nil {
		t.Fatal("operation accepted a different owner")
	}
}
