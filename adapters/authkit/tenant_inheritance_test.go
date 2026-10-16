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
	contentWrite := app.Declare(ckauthkit.AppPermission(access.ContentWrite))[0]
	entitiesWrite := app.Declare(ckauthkit.AppPermission(access.EntitiesWrite))[0]
	pollsWrite := app.Declare(ckauthkit.AppPermission(access.PollsWrite))[0]
	console := app.Permission("admin-console", "access")
	app.RequireMFA(console)
	contentAdmin := []iam.Grant{console, contentWrite, entitiesWrite, pollsWrite, moderate}
	rootAdmin := roles.Root.Role("content-admin", contentAdmin...)
	tenantAdmin := roles.Tenant.Role("content-admin", contentAdmin...)
	appAdmin := app.Role("content-admin", contentAdmin...)
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

	// The host's explicit human content bundle follows the same native walk.
	// Saving a grant before MFA enrollment does not make it usable early.
	for _, tc := range []struct {
		name  string
		group iam.GroupRef
		role  iam.Role
		want  []bool
	}{
		{"root content admin", iam.RootGroup(), rootAdmin, []bool{true, true, true, true}},
		{"tenant content admin", tenantGroups[0], tenantAdmin, []bool{true, true, false, false}},
		{"app content admin", targets[0].group, appAdmin, []bool{true, false, false, false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			user := authtest.NewUser(t, tenants[0])
			previousCredential := verify(t, authtest.SignIn(t, tenants[0], user).AccessToken)
			authtest.GrantRole(t, shared, tc.group, iam.UserSubject(user.ID), tc.role)
			for _, permission := range []iam.Perm{contentWrite, entitiesWrite, pollsWrite} {
				allowed, err := shared.Can(ctx, previousCredential.Identity(), targets[0].group, permission)
				if allowed || !errors.Is(err, iam.ErrTwoFAEnrollmentRequired) {
					t.Fatalf("pending enrollment %s: %v %v", permission, allowed, err)
				}
			}
			for _, permission := range []access.Permission{access.ContentWrite, access.EntitiesWrite, access.PollsWrite} {
				allowed, err := targets[0].policy.Authorize(ctx, targets[0].scope, previousCredential, contentkit.PermissionTarget{Permission: permission.String()})
				if allowed || !errors.Is(err, iam.ErrTwoFAEnrollmentRequired) {
					t.Fatalf("pending adapter enrollment %s: %v %v", permission, allowed, err)
				}
			}
			credential := verify(t, authtest.SignIn(t, tenants[0], user).AccessToken)
			check(t, credential, access.ContentWrite, tc.want...)
			check(t, credential, access.EntitiesWrite, tc.want...)
			check(t, credential, access.PollsWrite, tc.want...)
			check(t, credential, access.CommentsModerate, tc.want...)
			// Even these content administrators do not gain shared avatars.
			check(t, credential, access.AvatarsWrite, false, false, false, false)
			for _, permission := range []iam.Perm{roles.Root.Members.Manage, roles.Root.Credentials.Manage} {
				allowed, err := shared.Can(ctx, credential.Identity(), iam.RootGroup(), permission)
				if err != nil || allowed {
					t.Fatalf("content admin gained %s: %v %v", permission, allowed, err)
				}
			}
			if err := shared.RemoveGroupMember(ctx, iam.SystemIdentity(), tc.group, iam.UserSubject(user.ID)); err != nil {
				t.Fatal(err)
			}
			check(t, credential, access.ContentWrite, false, false, false, false)
			check(t, credential, access.CommentsModerate, false, false, false, false)
		})
	}

	t.Run("stored parent wins over caller configuration", func(t *testing.T) {
		appScope, err := shared.Scope(ctx, targets[0].group)
		if err != nil {
			t.Fatal(err)
		}
		wrongParent, err := shared.Scope(ctx, tenantGroups[1])
		if err != nil {
			t.Fatal(err)
		}
		forged := targets[0].scope
		forged.TenantID = targets[2].scope.TenantID
		for _, config := range []ckauthkit.AuthorizationConfig{
			{Scope: forged, Issuer: authtest.Issuer, AppScope: appScope, TenantAdminScope: wrongParent},
			{Scope: targets[0].scope, Issuer: authtest.Issuer, AppScope: appScope, TenantAdminScope: wrongParent},
		} {
			if _, err := ckauthkit.NewAuthorization(ctx, shared, config); err == nil {
				t.Fatal("forged parent was accepted")
			}
		}
		missing := targets[0].scope
		missing.AppID = contentkit.NewAppID()
		missingApp := appScope
		missingApp.ID = missing.AppID.String()
		if _, err := ckauthkit.NewAuthorization(ctx, shared, ckauthkit.AuthorizationConfig{Scope: missing, Issuer: authtest.Issuer, AppScope: missingApp}); err == nil {
			t.Fatal("missing authoritative app group was accepted")
		}
	})

}
