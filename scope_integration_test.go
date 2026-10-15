package contentkit

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/authkit/auth"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/internal/testauth"
)

// scopeTestAuthorization is explicit fixture authority; production hosts must
// supply their own verified tenant-account association and app grants.
type scopeTestAuthorization struct{}

func (scopeTestAuthorization) ResolveActor(_ context.Context, _ Scope, verified auth.Verified) (string, error) {
	return verified.Identity().Subject, nil
}

func (scopeTestAuthorization) Authorize(context.Context, Scope, auth.Verified, PermissionTarget) (bool, error) {
	return true, nil
}

func TestScopeAdoptsLegacyCatalogWithoutRekeying(t *testing.T) {
	ctx := t.Context()
	pool := pgtest.Pool(t, nil)
	schema := engineSchema(t, pool)
	deps := Deps{Postgres: pool, Auth: testauth.Auth{Authz: testauth.All{}}}
	cfg := siteConfig(schema)
	legacy, err := New(ctx, cfg, deps)
	if err != nil {
		t.Fatal(err)
	}
	created, err := legacy.Client().PostContents(ctx, catalog.ContentInput{Kind: "blog", Text: map[string]catalog.Text{"en": {Title: "Existing catalog", Body: "retained"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(ctx); err != nil {
		t.Fatal(err)
	}
	scope := Scope{TenantID: NewTenantID(), AppID: NewAppID()}
	cfg.Tenant, cfg.Scope, cfg.LegacyCatalogKey = "", &scope, tenant
	if e, err := New(ctx, cfg, deps); err == nil {
		_ = e.Close(ctx)
		t.Fatal("scoped engine accepted missing authorization binding")
	}
	deps.Authorization = scopeTestAuthorization{}
	adopted, err := New(ctx, cfg, deps)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = adopted.Close(context.Background()) })
	if got, ok := adopted.Scope(); !ok || got != scope {
		t.Fatalf("scope = %+v, %t", got, ok)
	}
	read, err := adopted.Client().GetContent(ctx, created.ID, GetContentQuery{})
	if err != nil || read.ID != created.ID || read.Title != "Existing catalog" {
		t.Fatalf("adopted catalog read: %+v, %v", read, err)
	}
	var storedKey string
	if err := pool.QueryRow(ctx, `SELECT tenant_id FROM `+pgx.Identifier{schema, "content_items"}.Sanitize()+` WHERE id = $1`, created.ID).Scan(&storedKey); err != nil || storedKey != tenant {
		t.Fatalf("catalog was rekeyed: %q, %v", storedKey, err)
	}
	if adopted.gate.Tenant() != tenant {
		t.Fatalf("access key prefix changed: %s", adopted.gate.Tenant())
	}
	if cfg.Tenant != "" || cfg.Scope != &scope || cfg.LegacyCatalogKey != tenant {
		t.Fatal("New mutated caller configuration")
	}
	replica, err := New(ctx, cfg, deps)
	if err != nil {
		t.Fatalf("same app replica refused: %v", err)
	}
	_ = replica.Close(ctx)
	for name, changed := range map[string]Config{
		"legacy downgrade":       siteConfig(schema),
		"new business":           scopeConfig(schema, Scope{TenantID: NewTenantID(), AppID: scope.AppID}, tenant),
		"other app same catalog": scopeConfig(schema, Scope{TenantID: scope.TenantID, AppID: NewAppID()}, tenant),
		"same app changed key":   scopeConfig(schema, scope, "renamed"),
	} {
		changedDeps := deps
		if changed.Scope == nil {
			changedDeps.Authorization = nil
		}
		if got, err := New(ctx, changed, changedDeps); !errors.Is(err, ErrScopeConflict) {
			if got != nil {
				_ = got.Close(ctx)
			}
			t.Errorf("%s: want ErrScopeConflict, got %v", name, err)
		}
	}
}

func scopeConfig(schema string, scope Scope, key string) Config {
	cfg := siteConfig(schema)
	cfg.Tenant, cfg.Scope, cfg.LegacyCatalogKey = "", &scope, key
	return cfg
}

func TestScopeCompetingAppClaims(t *testing.T) {
	ctx := t.Context()
	pool := pgtest.Pool(t, nil)
	schema := engineSchema(t, pool)
	deps := Deps{Postgres: pool, Auth: testauth.Auth{Authz: testauth.All{}}, Authorization: scopeTestAuthorization{}}
	start := make(chan struct{})
	result := make(chan error, 2)
	for range 2 {
		cfg := scopeConfig(schema, Scope{TenantID: NewTenantID(), AppID: NewAppID()}, "same_catalog")
		go func() {
			<-start
			e, err := New(ctx, cfg, deps)
			if e != nil {
				_ = e.Close(ctx)
			}
			result <- err
		}()
	}
	close(start)
	var successes, conflicts int
	for range 2 {
		switch err := <-result; {
		case err == nil:
			successes++
		case errors.Is(err, ErrScopeConflict):
			conflicts++
		default:
			t.Fatal(err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("competing ownership claims: successes %d, conflicts %d", successes, conflicts)
	}
}

func TestScopeSharedAccountsAcrossCatalogSchemas(t *testing.T) {
	ctx := t.Context()
	pool := pgtest.Pool(t, nil)
	accountSchema := engineSchema(t, pool)
	deps := Deps{Postgres: pool, Auth: testauth.Auth{Authz: testauth.All{}}, Authorization: scopeTestAuthorization{}}
	business := NewTenantID()
	makeConfig := func(owner TenantID) Config {
		cfg := scopeConfig(engineSchema(t, pool), Scope{TenantID: owner, AppID: NewAppID()}, "catalog")
		cfg.Accounts = &AccountsConfig{Schema: accountSchema}
		return cfg
	}
	a, err := New(ctx, makeConfig(business), deps)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close(context.Background()) })
	b, err := New(ctx, makeConfig(business), deps)
	if err != nil {
		t.Fatalf("same business cannot share account store: %v", err)
	}
	t.Cleanup(func() { _ = b.Close(context.Background()) })
	split := makeConfig(business)
	split.Accounts.Namespace = "independent"
	if got, err := New(ctx, split, deps); err == nil {
		if got != nil {
			_ = got.Close(ctx)
		}
		t.Fatal("scoped namespace override accepted")
	}
	foreign := makeConfig(NewTenantID())
	other, err := New(ctx, foreign, deps)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close(ctx)
	if a.notices == other.notices {
		t.Fatal("foreign tenant shared an inbox")
	}

}

func TestScopeCompetingAccountClaimsAcrossSchemas(t *testing.T) {
	ctx := t.Context()
	pool := pgtest.Pool(t, nil)
	accountSchema := engineSchema(t, pool)
	configs := []Config{
		scopeConfig(engineSchema(t, pool), Scope{TenantID: NewTenantID(), AppID: NewAppID()}, "app_a"),
		scopeConfig(engineSchema(t, pool), Scope{TenantID: NewTenantID(), AppID: NewAppID()}, "app_b"),
	}
	type outcome struct {
		index int
		err   error
	}
	start := make(chan struct{})
	results := make(chan outcome, len(configs))
	for i, cfg := range configs {
		cfg.Accounts = &AccountsConfig{Schema: accountSchema}
		go func() {
			<-start
			err := MigratePostgres(ctx, pool, cfg, MigrationOptions{LegacyAccountKey: "shared"})
			results <- outcome{i, err}
		}()
	}
	close(start)
	var successes, conflicts int
	for range configs {
		got := <-results
		switch {
		case got.err == nil:
			successes++
		case errors.Is(got.err, ErrScopeConflict):
			conflicts++
			var count int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+pgx.Identifier{configs[got.index].Database.Schema, "content_app_bindings"}.Sanitize()).Scan(&count); err != nil || count != 0 {
				t.Fatalf("losing app claim survived account conflict: %d, %v", count, err)
			}
		default:
			t.Fatal(got.err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("competing account claims: successes %d, conflicts %d", successes, conflicts)
	}
}
