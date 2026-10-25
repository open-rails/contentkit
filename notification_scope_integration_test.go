package contentkit

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/internal/testauth"
	"github.com/open-rails/contentkit/notification"
	source "github.com/open-rails/contentkit/notification/openrails"
)

type emptyNotificationBackend struct{}

func (emptyNotificationBackend) ListEvents(context.Context, string, int) (source.EventPage, error) {
	return source.EventPage{Events: []source.Event{}, NextCursor: "empty", RetentionSeconds: 90 * 86400}, nil
}

func TestScopedAppsShareOnlyTheirVerifiedMerchant(t *testing.T) {
	ctx := t.Context()
	pool := pgtest.Pool(t, nil)
	accountSchema := engineSchema(t, pool)
	tenantID := NewTenantID()
	makeApp := func(tenant TenantID, _ string, key string) *Engine {
		t.Helper()
		scope := Scope{TenantID: tenant, AppID: NewAppID()}
		cfg := scopeConfig(engineSchema(t, pool), scope, key)
		cfg.Accounts = &AccountsConfig{Schema: accountSchema}
		e, err := New(ctx, cfg, Deps{Postgres: pool, Auth: testauth.Auth{Authz: testauth.All{}}, Authorization: scopeTestAuthorization{}})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = e.Close(context.Background()) })
		if e.notices.AppID() != scope.AppID.String() {
			t.Fatalf("scope app not wired: %q", e.notices.AppID())
		}
		return e
	}
	a, b := makeApp(tenantID, "accounts", "a"), makeApp(tenantID, "accounts", "b")
	makeSource := func(merchant string) *source.Source {
		t.Helper()
		s, err := source.New(source.Options{Name: "openrails:test", MerchantID: merchant, Backend: emptyNotificationBackend{}})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	merchant := uuid.NewString()
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, e := range []*Engine{a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := e.notices.BindMerchant(ctx, e.scope.TenantID.String(), makeSource(merchant).Origin())
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := a.notices.BindMerchant(ctx, a.scope.TenantID.String(), makeSource(uuid.NewString()).Origin()); !errors.Is(err, notification.ErrMerchantBinding) {
		t.Fatalf("foreign merchant attached: %v", err)
	}
	otherTenant := makeApp(NewTenantID(), "other-accounts", "other")
	if err := otherTenant.notices.BindMerchant(ctx, otherTenant.scope.TenantID.String(), makeSource(merchant).Origin()); !errors.Is(err, notification.ErrMerchantBinding) {
		t.Fatalf("merchant reassigned to foreign tenant: %v", err)
	}
}
