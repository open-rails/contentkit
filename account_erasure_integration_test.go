package contentkit

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/internal/testauth"
	"github.com/open-rails/contentkit/signal"
)

func TestAccountErasureCompletionSurvivesCatalogReplay(t *testing.T) {
	ctx := t.Context()
	pool := pgtest.Pool(t, nil)
	accounts := engineSchema(t, pool)
	tenant := NewTenantID()
	provider := &replayProvider{fail: true}
	var engines []*Engine
	var configs []Config
	var dependencies []Deps
	for i, owner := range []TenantID{tenant, tenant, NewTenantID()} {
		scope := Scope{TenantID: owner, AppID: NewAppID()}
		cfg := scopeConfig(engineSchema(t, pool), scope, "catalog")
		cfg.Accounts = &AccountsConfig{Schema: accounts}
		deps := Deps{Postgres: pool, Auth: testauth.Auth{Authz: testauth.All{}}, Authorization: scopeTestAuthorization{}}
		if i == 0 {
			deps.ProviderDataEraser = provider
		}
		e, err := New(ctx, cfg, deps)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = e.Close(context.Background()) })
		engines, configs, dependencies = append(engines, e), append(configs, cfg), append(dependencies, deps)
	}
	subjects := []signal.Subject{{UserID: "erased-account"}}
	if report, err := engines[0].EraseSubjects(ctx, subjects); err == nil || report.Complete() {
		t.Fatalf("failed full cleanup was acknowledged: %+v %v", report, err)
	}
	provider.fail = false
	for _, e := range engines[:2] {
		if report, err := e.ReplayErasureFences(ctx); !errors.Is(err, ErrErasureReplayRequired) || report.Complete() || report.Remaining["account_plane"] == 0 {
			t.Fatalf("app-only replay discharged global cleanup: %+v %v", report, err)
		}
	}
	if err := engines[2].checkErasureReplay(ctx); err != nil {
		t.Fatalf("another tenant inherited erasure: %v", err)
	}
	if report, err := engines[1].ReplayAccountErasures(ctx); err != nil || !report.Complete() {
		t.Fatalf("second app full completion: %+v %v", report, err)
	}
	if e, err := New(ctx, configs[0], dependencies[0]); !errors.Is(err, ErrErasureReplayRequired) {
		if e != nil {
			_ = e.Close(ctx)
		}
		t.Fatalf("sibling receipt discharged first app obligation: %v", err)
	}
	if report, err := engines[0].ReplayAccountErasures(ctx); err != nil || !report.Complete() {
		t.Fatalf("explicit full recovery: %+v %v", report, err)
	}
	receipts := pgx.Identifier{accounts, "content_account_erasure_receipts"}.Sanitize()
	var completed time.Time
	if err := pool.QueryRow(ctx, `SELECT completed_at FROM `+receipts+` WHERE tenant_id=$1 AND app_id=$2 AND actor_id=$3`, tenant.String(), configs[0].Scope.AppID.String(), subjects[0].UserID).Scan(&completed); err != nil {
		t.Fatal(err)
	}
	// A catalog-only restore loses local fences, not the independent proof
	// that full account cleanup already finished.
	if _, err := pool.Exec(ctx, `DELETE FROM `+pgx.Identifier{engines[0].schema, "content_erased_subjects"}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	if err := engines[0].checkErasureReplay(ctx); !errors.Is(err, ErrErasureReplayRequired) {
		t.Fatalf("missing restored local fence was accepted: %v", err)
	}
	if report, err := engines[0].ReplayErasureFences(ctx); err != nil || !report.Complete() {
		t.Fatalf("completed account could not replay local catalog: %+v %v", report, err)
	}
	var after time.Time
	if err := pool.QueryRow(ctx, `SELECT completed_at FROM `+receipts+` WHERE tenant_id=$1 AND app_id=$2 AND actor_id=$3`, tenant.String(), configs[0].Scope.AppID.String(), subjects[0].UserID).Scan(&after); err != nil || !after.Equal(completed) {
		t.Fatalf("local replay rewrote global completion: %v %v", after, err)
	}
	if err := engines[0].checkErasureReplay(ctx); err != nil {
		t.Fatalf("completed replay still blocked: %v", err)
	}
}

func TestAccountErasureRequiresAvatarPrimaryCompletion(t *testing.T) {
	ctx := t.Context()
	pool := pgtest.Pool(t, nil)
	accounts := engineSchema(t, pool)
	tenant := NewTenantID()
	var engines []*Engine
	for range 2 {
		cfg := scopeConfig(engineSchema(t, pool), Scope{TenantID: tenant, AppID: NewAppID()}, "catalog")
		cfg.Accounts = &AccountsConfig{Schema: accounts}
		e, err := New(ctx, cfg, Deps{Postgres: pool, Auth: testauth.Auth{Authz: testauth.All{}}, Authorization: scopeTestAuthorization{}})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = e.Close(context.Background()) })
		engines = append(engines, e)
	}
	// The primary runtime has lost its avatar configuration. It cannot claim
	// completion merely because local catalog cleanup still works.
	if _, err := pool.Exec(ctx, `INSERT INTO `+pgx.Identifier{accounts, "content_account_avatar_placement"}.Sanitize()+` (tenant_id,primary_app_id) VALUES ($1,$2)`, tenant.String(), engines[0].scope.AppID.String()); err != nil {
		t.Fatal(err)
	}
	subjects := []signal.Subject{{UserID: "erased-account"}}
	if report, err := engines[0].EraseSubjects(ctx, subjects); err == nil || report.Remaining["media_plane"] == 0 {
		t.Fatalf("unconfigured avatar primary acknowledged cleanup: %+v %v", report, err)
	}
	if report, err := engines[1].EraseSubjects(ctx, subjects); err != nil || !report.Complete() {
		t.Fatalf("secondary local obligation failed: %+v %v", report, err)
	}
	for _, e := range engines {
		if report, err := e.ReplayErasureFences(ctx); !errors.Is(err, ErrErasureReplayRequired) || report.Complete() {
			t.Fatalf("app replay bypassed missing primary receipt: %+v %v", report, err)
		}
		if err := e.checkErasureReplay(ctx); !errors.Is(err, ErrErasureReplayRequired) {
			t.Fatalf("unfinished primary did not gate app %s: %v", e.scope.AppID, err)
		}
	}
}
