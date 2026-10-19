package contentkit

import (
	"context"
	"errors"
	"testing"

	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/internal/testauth"
	"github.com/open-rails/contentkit/signal"
)

func TestReconcileFacetsRefusesErasureRecovery(t *testing.T) {
	e := &Engine{erasureRecovery: true}
	if n, err := e.ReconcileFacets(t.Context()); n != 0 || !errors.Is(err, ErrErasureReplayRequired) {
		t.Fatalf("recovery engine reconciled catalog facets: %d %v", n, err)
	}
}

func TestReconcileFacetsChecksCurrentErasureReadinessIntegration(t *testing.T) {
	ctx := t.Context()
	pool := pgtest.Pool(t, nil)
	cfg := scopeConfig(engineSchema(t, pool), Scope{TenantID: NewTenantID(), AppID: NewAppID()}, "catalog")
	e, err := New(ctx, cfg, Deps{Postgres: pool, Auth: testauth.Auth{Authz: testauth.All{}},
		Authorization: scopeTestAuthorization{}, ProviderDataEraser: &replayProvider{fail: true}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close(context.Background()) })
	// A new erasure obligation can appear after New's startup check. Explicit
	// facet repair must recheck it before touching the catalog.
	if report, err := e.EraseSubjects(ctx, []signal.Subject{{UserID: "pending-erasure"}}); err == nil || report.Complete() {
		t.Fatalf("fixture did not leave an erasure obligation: %+v %v", report, err)
	}
	if n, err := e.ReconcileFacets(ctx); n != 0 || !errors.Is(err, ErrErasureReplayRequired) {
		t.Fatalf("unready engine reconciled catalog facets: %d %v", n, err)
	}
}
