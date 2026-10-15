package authkit_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/auth"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	"github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/access"
	ckauthkit "github.com/open-rails/contentkit/adapters/authkit"
)

func TestLegacyContentPersonasMigrateWithoutWideningOrReplacingKey(t *testing.T) {
	ctx := t.Context()
	dsn := os.Getenv("AUTHKIT_TEST_DATABASE_URL")
	if dsn == "" {
		if os.Getenv("AUTHKIT_TEST_REQUIRE_DB") == "1" {
			t.Fatal("AUTHKIT_TEST_DATABASE_URL required")
		}
		t.Skip("AUTHKIT_TEST_DATABASE_URL not set")
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	tenant := iam.NewTenantID()
	ctenant, _ := contentkit.ParseTenantID(tenant.String())
	scope := contentkit.Scope{TenantID: ctenant, AppID: contentkit.NewAppID()}
	schema := "ck_persona_" + strings.ReplaceAll(tenant.String(), "-", "")
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := pool.Exec(cleanup, "DROP SCHEMA IF EXISTS "+pgx.Identifier{schema}.Sanitize()+" CASCADE"); err != nil {
			t.Error(err)
		}
		if _, err := pool.Exec(cleanup, "DELETE FROM public.migrations WHERE app='authkit' AND schema=$1", schema); err != nil {
			t.Error(err)
		}
	})
	roles := authkit.NewRoles(authkit.APIKeys)
	oldApp := roles.Persona("contentkit", authkit.APIKeys)
	oldModerate := oldApp.Declare("contentkit:comments:moderate")[0]
	oldModerator := oldApp.Role("moderator", oldModerate)
	app := roles.Persona("app", authkit.APIKeys)
	moderate := app.Declare(ckauthkit.AppPermission(access.CommentsModerate))[0]
	moderator := app.Role("moderator", moderate)
	oldTenant := roles.Persona("contentkit-tenant", authkit.APIKeys)
	oldAvatar := oldTenant.Declare("contentkit-tenant:avatars:write")[0]
	oldWriter := oldTenant.Role("avatar-writer", oldAvatar)
	avatar := roles.Tenant.Declare(ckauthkit.TenantPermission(access.AvatarsWrite))[0]
	writer := roles.Tenant.Role("avatar-writer", avatar)
	client, _ := authtest.New(t, authtest.WithConfig(func(c *authkit.Config) {
		c.Roles, c.DefaultTenantID = roles, tenant
		c.Database.Schema, c.Database.RiverSchema = schema, schema
	}), authtest.WithDeps(func(d *authkit.Deps) { d.Postgres = pool }))
	// This isolated seed models the old persisted persona after explicit tenant
	// ownership adoption. Production uses the migration API below, never SQL.
	table := pgx.Identifier{schema, "permission_groups"}.Sanitize()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, "ALTER TABLE "+table+" DISABLE TRIGGER permission_group_identity_immutable"); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, "INSERT INTO "+table+"(id,persona,tenant_id) VALUES($1,'contentkit-tenant',$1) ON CONFLICT(id) DO UPDATE SET persona='contentkit-tenant'", tenant.String()); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, "ALTER TABLE "+table+" ENABLE TRIGGER permission_group_identity_immutable"); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	oldAppPersona, _ := client.Persona("contentkit")
	if _, err = client.CreateGroup(ctx, iam.NewGroup{ID: scope.AppID.String(), TenantID: tenant, Persona: oldAppPersona}); err != nil {
		t.Fatal(err)
	}
	tenantRef, appRef := iam.GroupByID(tenant.String()), iam.GroupByID(scope.AppID.String())
	user := authtest.NewUser(t, client)
	authtest.GrantRole(t, client, appRef, iam.UserSubject(user.ID), oldModerator)
	key, err := client.CreateAPIKey(ctx, iam.SystemIdentity(), tenantRef, iam.NewAPIKey{Name: "retained avatar writer", Role: oldWriter})
	if err != nil {
		t.Fatal(err)
	}
	appScope, _ := client.Scope(ctx, appRef)
	tenantScope, _ := client.Scope(ctx, tenantRef)
	cfg := ckauthkit.AuthorizationConfig{Scope: scope, Issuer: authtest.Issuer, AppScope: appScope, TenantAdminScope: tenantScope}
	if _, err = ckauthkit.NewAuthorization(ctx, client, cfg); !errors.Is(err, ckauthkit.ErrScopeMigrationRequired) {
		t.Fatalf("legacy persona admitted: %v", err)
	}
	oldOwner, _ := client.Role("contentkit-tenant:owner")
	newOwner, _ := client.Role("tenant:owner")
	unsafe := iam.GroupPersonaMigration{Group: tenantRef, From: oldWriter.Persona(), To: writer.Persona(), Roles: map[iam.Role]iam.Role{oldOwner: newOwner}, Permissions: map[iam.Perm]iam.Perm{oldAvatar: avatar}}
	for _, name := range []string{"members:read", "members:manage", "credentials:read", "credentials:manage"} {
		a, _ := client.Permission("contentkit-tenant:" + name)
		b, _ := client.Permission("tenant:" + name)
		unsafe.Permissions[a] = b
	}
	if _, err = client.MigrateGroupPersona(ctx, unsafe); !errors.Is(err, iam.ErrGroupPersonaMigration) {
		t.Fatalf("old content-only owner widened to account owner: %v", err)
	}
	apply := func(in iam.GroupPersonaMigration) {
		t.Helper()
		report, err := client.MigrateGroupPersona(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		if report.Applied || report.Fingerprint == "" {
			t.Fatalf("bad dry-run: %+v", report)
		}
		in.Apply, in.ExpectedFingerprint = true, report.Fingerprint
		report, err = client.MigrateGroupPersona(ctx, in)
		if err != nil || !report.Applied {
			t.Fatalf("apply: %+v %v", report, err)
		}
	}
	apply(iam.GroupPersonaMigration{Group: appRef, From: oldModerator.Persona(), To: moderator.Persona(), Roles: map[iam.Role]iam.Role{oldModerator: moderator}, Permissions: map[iam.Perm]iam.Perm{oldModerate: moderate}})
	apply(iam.GroupPersonaMigration{Group: tenantRef, From: oldWriter.Persona(), To: writer.Persona(), Roles: map[iam.Role]iam.Role{oldWriter: writer}, Permissions: map[iam.Perm]iam.Perm{oldAvatar: avatar}})
	policy, err := ckauthkit.NewAuthorization(ctx, client, cfg)
	if err != nil {
		t.Fatal(err)
	}
	verify := func(token string) auth.Verified {
		t.Helper()
		r := httptest.NewRequest("GET", "https://content.test/admin", nil)
		r.Header.Set("Authorization", "Bearer "+token)
		v, err := client.Authenticator().Authenticate(r)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	actor := verify(authtest.SignIn(t, client, user).AccessToken)
	if allowed, err := policy.Authorize(ctx, scope, actor, contentkit.PermissionTarget{Permission: access.CommentsModerate.String()}); err != nil || !allowed {
		t.Fatalf("moderator authority lost: %v %v", allowed, err)
	}
	credential := verify(key.Secret)
	if credential.Identity().Credential.ID != key.APIKey.ID || credential.Identity().Subject != tenantRef.ID() {
		t.Fatal("migration replaced the key identity")
	}
	if allowed, err := policy.Authorize(ctx, scope, credential, contentkit.PermissionTarget{Permission: access.AvatarsWrite.String()}); err != nil || !allowed {
		t.Fatalf("retained avatar key: %v %v", allowed, err)
	}
	if allowed, err := policy.Authorize(ctx, scope, credential, contentkit.PermissionTarget{Permission: access.CommentsModerate.String()}); err != nil || allowed {
		t.Fatalf("avatar key gained app moderation: %v %v", allowed, err)
	}
	keys, err := client.ListAPIKeys(ctx, tenantRef, iam.PageRequest{})
	if err != nil || len(keys.Items) != 1 || keys.Items[0].ID != key.APIKey.ID || keys.Items[0].Role != writer {
		t.Fatalf("key binding changed: %v", err)
	}
}
