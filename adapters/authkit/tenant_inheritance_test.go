package authkit_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/auth"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	"github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/access"
	ckauthkit "github.com/open-rails/contentkit/adapters/authkit"
)

func TestNativeTenantInheritanceAndCredentialBounds(t *testing.T) {
	ctx := t.Context()
	roles := authkit.NewRoles(authkit.APIKeys)
	app := roles.Persona("app", authkit.APIKeys)
	moderate := app.Declare(ckauthkit.AppPermission(access.CommentsModerate))[0]
	app.Declare(ckauthkit.AppPermission(access.ContentWrite))
	roles.Tenant.Declare(ckauthkit.TenantPermission(access.AvatarsWrite))
	appModerator := app.Role("moderator", moderate)
	tenantModerator := roles.Tenant.Role("moderator", moderate)
	globalReviewer := roles.Root.Role("reviewer", moderate)
	shared, _ := authtest.New(t, authtest.WithConfig(func(c *authkit.Config) {
		c.Roles, c.DefaultTenantID = roles, iam.TenantID{}
	}))
	type target struct {
		scope  contentkit.Scope
		group  iam.GroupRef
		policy contentkit.Authorization
	}
	var targets []target
	var tenants []*authkit.Client
	var tenantGroups []iam.GroupRef
	for range 2 {
		tid := iam.NewTenantID()
		if err := shared.CreateTenant(ctx, iam.Tenant{ID: tid}); err != nil {
			t.Fatal(err)
		}
		client, err := shared.ForTenant(ctx, tid)
		if err != nil {
			t.Fatal(err)
		}
		tenants = append(tenants, client)
		tenantPersona, _ := shared.Persona("tenant")
		if _, err := client.CreateGroup(ctx, iam.NewGroup{ID: tid.String(), TenantID: tid, Persona: tenantPersona}); err != nil {
			t.Fatal(err)
		}
		tenantGroup := iam.GroupByID(tid.String())
		tenantGroups = append(tenantGroups, tenantGroup)
		tenantScope, err := client.Scope(ctx, tenantGroup)
		if err != nil {
			t.Fatal(err)
		}
		for range 2 {
			aid := contentkit.NewAppID()
			appPersona, _ := shared.Persona("app")
			if _, err := client.CreateGroup(ctx, iam.NewGroup{ID: aid.String(), TenantID: tid, Persona: appPersona}); err != nil {
				t.Fatal(err)
			}
			group := iam.GroupByID(aid.String())
			appScope, err := client.Scope(ctx, group)
			if err != nil {
				t.Fatal(err)
			}
			ctid, _ := contentkit.ParseTenantID(tid.String())
			scope := contentkit.Scope{TenantID: ctid, AppID: aid}
			policy, err := ckauthkit.NewAuthorization(ctx, client, ckauthkit.AuthorizationConfig{
				Scope: scope, Issuer: authtest.Issuer, AppScope: appScope, TenantAdminScope: tenantScope,
			})
			if err != nil {
				t.Fatal(err)
			}
			targets = append(targets, target{scope, group, policy})
		}
	}
	verify := func(t *testing.T, token string) auth.Verified {
		t.Helper()
		r := httptest.NewRequest("GET", "https://content.test/admin", nil)
		r.Header.Set("Authorization", "Bearer "+token)
		v, err := shared.Authenticator().Authenticate(r)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	check := func(t *testing.T, v auth.Verified, permission access.Permission, allowed ...bool) {
		t.Helper()
		for i, target := range targets {
			got, err := target.policy.Authorize(ctx, target.scope, v, contentkit.PermissionTarget{Permission: permission.String()})
			if got != allowed[i] || err != nil && (allowed[i] || !errors.Is(err, auth.ErrForbidden)) {
				t.Fatalf("target %d %s: allowed=%v want=%v err=%v", i, permission, got, allowed[i], err)
			}
		}
	}
	person := authtest.NewUser(t, tenants[0])
	v := verify(t, authtest.SignIn(t, tenants[0], person).AccessToken)
	check(t, v, access.CommentsModerate, false, false, false, false)
	for i, target := range targets {
		account, member, err := target.policy.(contentkit.TenantAccountAuthorization).ResolveTenantAccount(ctx, target.scope, v)
		if err != nil || member != (i < 2) || member && account != person.ID || !member && account != "" {
			t.Fatalf("target %d membership: %q %v %v", i, account, member, err)
		}
	}
	otherPerson := authtest.NewUser(t, tenants[1])
	otherCredential := verify(t, authtest.SignIn(t, tenants[1], otherPerson).AccessToken)
	authtest.GrantRole(t, tenants[1], targets[2].group, iam.UserSubject(otherPerson.ID), appModerator)
	check(t, otherCredential, access.CommentsModerate, false, false, true, false)
	authtest.GrantRole(t, tenants[1], tenantGroups[1], iam.UserSubject(otherPerson.ID), tenantModerator)
	check(t, otherCredential, access.CommentsModerate, false, false, true, true)
	authtest.GrantRole(t, tenants[0], targets[0].group, iam.UserSubject(person.ID), appModerator)
	check(t, v, access.CommentsModerate, true, false, false, false)
	authtest.GrantRole(t, tenants[0], tenantGroups[0], iam.UserSubject(person.ID), tenantModerator)
	check(t, v, access.CommentsModerate, true, true, false, false)
	authtest.GrantRole(t, shared, iam.RootGroup(), iam.UserSubject(person.ID), globalReviewer)
	check(t, v, access.CommentsModerate, true, true, true, true)
	check(t, v, access.ContentWrite, false, false, false, false)
	check(t, v, access.AvatarsWrite, false, false, false, false)

	tenantOwner := authtest.NewUser(t, tenants[0])
	tenantOwner.TOTP = authtest.EnrollTOTP(t, tenants[0], tenantOwner)
	tenantOwnerRole, _ := shared.Role("tenant:owner")
	authtest.GrantRole(t, tenants[0], tenantGroups[0], iam.UserSubject(tenantOwner.ID), tenantOwnerRole)
	tenantCredential := verify(t, authtest.SignIn(t, tenants[0], tenantOwner).AccessToken)
	check(t, tenantCredential, access.ContentWrite, true, true, false, false)
	check(t, tenantCredential, access.AvatarsWrite, true, true, false, false)

	owner := authtest.NewUser(t, tenants[0])
	owner.TOTP = authtest.EnrollTOTP(t, tenants[0], owner)
	ownerRole, _ := shared.Role("root:owner")
	authtest.GrantRole(t, shared, iam.RootGroup(), iam.UserSubject(owner.ID), ownerRole)
	ownerCredential := verify(t, authtest.SignIn(t, tenants[0], owner).AccessToken)
	check(t, ownerCredential, access.ContentWrite, true, true, true, true)
	check(t, ownerCredential, access.AvatarsWrite, true, true, true, true)
	if account, member, err := targets[2].policy.(contentkit.TenantAccountAuthorization).ResolveTenantAccount(ctx, targets[2].scope, ownerCredential); err != nil || member || account != "" {
		t.Fatalf("global owner acquired an audience account: %q %v %v", account, member, err)
	}

	for _, tc := range []struct {
		name   string
		client *authkit.Client
		group  iam.GroupRef
		role   iam.Role
		want   []bool
	}{
		{"app", tenants[0], targets[0].group, appModerator, []bool{true, false, false, false}},
		{"tenant", tenants[0], tenantGroups[0], tenantModerator, []bool{true, true, false, false}},
		{"global limited", shared, iam.RootGroup(), globalReviewer, []bool{true, true, true, true}},
	} {
		t.Run(tc.name+" key", func(t *testing.T) {
			key, err := tc.client.CreateAPIKey(context.Background(), iam.SystemIdentity(), tc.group, iam.NewAPIKey{Name: "fixture", Role: tc.role})
			if err != nil {
				t.Fatal(err)
			}
			credential := verify(t, key.Secret)
			check(t, credential, access.CommentsModerate, tc.want...)
			check(t, credential, access.ContentWrite, false, false, false, false)
			if err := tc.client.RevokeAPIKey(ctx, iam.SystemIdentity(), tc.group, key.APIKey.ID); err != nil {
				t.Fatal(err)
			}
			for i, wasAllowed := range tc.want {
				if !wasAllowed {
					continue
				}
				target := targets[i]
				if allowed, _ := target.policy.Authorize(ctx, target.scope, credential, contentkit.PermissionTarget{Permission: access.CommentsModerate.String()}); allowed {
					t.Fatalf("target %d retained revoked key authority", i)
				}
			}
		})
	}
}
