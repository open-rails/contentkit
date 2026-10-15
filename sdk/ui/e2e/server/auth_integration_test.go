package main

import (
	"context"
	"testing"
	"time"

	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	"github.com/open-rails/contentkit/internal/pgtest"
)

// Exercise the actual browser-harness constructor and control user helper.
// A successful compile alone cannot detect an unbound native account directory.
func TestNativeBrowserFixtureBootsAndSignsIn(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	pool := pgtest.Pool(t, nil)
	schema := pgtest.EmptySchema(t, ctx, pool)
	client, _, err := newAuthInDatabase(ctx, "http://localhost:4790", pool, authkit.DatabaseConfig{Schema: schema, RiverSchema: schema})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 15*time.Second)
		defer stop()
		if err := client.Close(cleanup); err != nil {
			t.Error(err)
		}
	})
	created, err := client.CreateUser(ctx, iam.NewUser{Username: "nativefixture", Email: "nativefixture@example.test", EmailVerified: true, Password: authtest.Password})
	if err != nil {
		t.Fatalf("fixture must create an account in its explicit tenant: %v", err)
	}
	tenantID, err := iam.ParseTenantID("00000000-0000-4000-8000-000000000201")
	if err != nil {
		t.Fatal(err)
	}
	if created.TenantID != tenantID {
		t.Fatalf("account owner %s, want explicit fixture tenant %s", created.TenantID, tenantID)
	}
	tokens := authtest.SignIn(t, client, authtest.User{User: created, Email: "nativefixture@example.test", Password: authtest.Password})
	if tokens.AccessToken == "" {
		t.Fatal("native password sign-in returned no access token")
	}
	view, err := client.ForTenant(ctx, tenantID)
	if err != nil {
		t.Fatal(err)
	}
	lookup, ok := view.UserInfo().(authkit.TransactionalUserInfo)
	if !ok {
		t.Fatal("native fixture directory lacks transaction-aware lookup")
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	users, err := lookup.GetTx(ctx, tx, []string{created.ID})
	if err != nil || users[created.ID].ID != created.ID {
		t.Fatalf("native transaction directory lookup: %+v, %v", users, err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	control := &harness{auth: client}
	user, err := control.newUser("editor")
	if err != nil || user.ID == "" || user.AccessToken == "" {
		t.Fatalf("browser control must create and sign in its role-bearing user: id=%q, token present=%t, error=%v", user.ID, user.AccessToken != "", err)
	}
}
