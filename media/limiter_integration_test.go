package media_test

import (
	"context"
	"sync"
	"testing"

	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/media"
)

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
