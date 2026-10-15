package authkit_test

import (
	"context"
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

func TestAuthorizationAppIsolation(t *testing.T) {
	ctx := context.Background()
	tenantID := contentkit.NewTenantID()
	accountTenant, _ := iam.ParseTenantID(tenantID.String())
	r := authkit.NewRoles()
	app := r.Persona("app")
	moderate := app.Declare(ckauthkit.AppPermission(access.CommentsModerate))[0]
	appRole := app.Role("moderator", moderate)
	r.Root.Role("global", moderate)
	tenantPersona := r.Tenant
	tenantRole := tenantPersona.Role("moderator", moderate)
	ak, _ := authtest.New(t, authtest.WithConfig(func(c *authkit.Config) {
		c.Roles, c.DefaultTenantID = r, accountTenant
	}))
	person := authtest.NewUser(t, ak)
	token := authtest.SignIn(t, ak, person).AccessToken
	request := httptest.NewRequest("GET", "https://example.com/contentkit", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	v, err := ak.Authenticator().Authenticate(request)
	if err != nil {
		t.Fatal(err)
	}
	appPersona, err := ak.Persona("app")
	if err != nil {
		t.Fatal(err)
	}
	tenantKind, err := ak.Persona("tenant")
	if err != nil {
		t.Fatal(err)
	}
	var policies []contentkit.Authorization
	var scopes []contentkit.Scope
	var groups []iam.GroupRef
	tenantGroup, err := ak.CreateGroup(ctx, iam.NewGroup{ID: tenantID.String(), TenantID: accountTenant, Persona: tenantKind})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		appID := contentkit.NewAppID()
		group, err := ak.CreateGroup(ctx, iam.NewGroup{ID: appID.String(), TenantID: accountTenant, Persona: appPersona})
		if err != nil {
			t.Fatal(err)
		}
		groupScope, err := ak.Scope(ctx, iam.GroupByID(group.ID))
		if err != nil {
			t.Fatal(err)
		}
		tenantScope, err := ak.Scope(ctx, iam.GroupByID(tenantGroup.ID))
		if err != nil {
			t.Fatal(err)
		}
		scope := contentkit.Scope{TenantID: tenantID, AppID: appID}
		policy, err := ckauthkit.NewAuthorization(ctx, ak, ckauthkit.AuthorizationConfig{Scope: scope, Issuer: authtest.Issuer, AppScope: groupScope, TenantAdminScope: tenantScope})
		if err != nil {
			t.Fatal(err)
		}
		policies, scopes, groups = append(policies, policy), append(scopes, scope), append(groups, iam.GroupByID(group.ID))
	}
	authtest.GrantRole(t, ak, iam.RootGroup(), iam.UserSubject(person.ID), mustRole(t, ak, "root:global"))
	target := contentkit.PermissionTarget{Permission: access.CommentsModerate.String()}
	for i, policy := range policies {
		account, err := policy.ResolveActor(ctx, scopes[i], v)
		if err != nil || account != person.ID {
			t.Fatalf("ordinary account in app %d: %q %v", i, account, err)
		}
		allowed, err := policy.Authorize(ctx, scopes[i], v, target)
		if err != nil || !allowed {
			t.Fatalf("native global root grant missing in app %d: %v %v", i, allowed, err)
		}
	}
	if err := ak.RemoveGroupMember(ctx, iam.SystemIdentity(), iam.RootGroup(), iam.UserSubject(person.ID)); err != nil {
		t.Fatal(err)
	}
	authtest.GrantRole(t, ak, groups[0], iam.UserSubject(person.ID), appRole)
	for i, policy := range policies {
		allowed, err := policy.Authorize(ctx, scopes[i], v, target)
		if err != nil || allowed != (i == 0) {
			t.Fatalf("app role leaked: app=%d allowed=%v err=%v", i, allowed, err)
		}
	}
	authtest.GrantRole(t, ak, iam.GroupByID(tenantGroup.ID), iam.UserSubject(person.ID), tenantRole)
	for i, policy := range policies {
		allowed, err := policy.Authorize(ctx, scopes[i], v, target)
		if err != nil || !allowed {
			t.Fatalf("explicit tenant grant missing: %v %v", allowed, err)
		}
	}
	if _, err := policies[0].ResolveActor(ctx, contentkit.Scope{TenantID: contentkit.NewTenantID(), AppID: scopes[0].AppID}, v); err == nil {
		t.Fatal("unrelated tenant admitted")
	}
	rootScope, err := ak.Scope(ctx, iam.RootGroup())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ckauthkit.NewAuthorization(ctx, ak, ckauthkit.AuthorizationConfig{Scope: scopes[0], Issuer: authtest.Issuer, AppScope: rootScope, Accounts: ckauthkit.IssuerAccounts(tenantID, authtest.Issuer)}); err == nil {
		t.Fatal("deployment root accepted as app group")
	}
}

func mustRole(t *testing.T, ak *authkit.Client, name string) iam.Role {
	t.Helper()
	role, err := ak.Role(name)
	if err != nil {
		t.Fatal(err)
	}
	return role
}

type bindingCredential struct {
	id    auth.Identity
	bound auth.Scope
}

func (v bindingCredential) Identity() auth.Identity { return v.id }
func (v bindingCredential) BoundScope() auth.Scope  { return v.bound }

type authorizationGroups struct {
	scope  auth.Scope
	tenant contentkit.TenantID
}

func (g authorizationGroups) Group(_ context.Context, ref iam.GroupRef) (iam.Group, error) {
	var p iam.Persona
	_ = p.UnmarshalText([]byte("app"))
	tenant, _ := iam.ParseTenantID(g.tenant.String())
	return iam.Group{ID: ref.ID(), Persona: p, TenantID: tenant}, nil
}
func (g authorizationGroups) Scope(_ context.Context, ref iam.GroupRef) (auth.Scope, error) {
	if ref == iam.RootGroup() {
		return auth.Scope{Authority: g.scope.Authority, ID: "root"}, nil
	}
	return g.scope, nil
}

func TestAuthorizationRejectsForeignIdentityAndBinding(t *testing.T) {
	scope := contentkit.Scope{TenantID: contentkit.NewTenantID(), AppID: contentkit.NewAppID()}
	app := auth.Scope{Authority: "https://id.test", ID: scope.AppID.String()}
	policy, err := ckauthkit.NewAuthorization(t.Context(), authorizationGroups{app, scope.TenantID}, ckauthkit.AuthorizationConfig{Scope: scope, Issuer: app.Authority, AppScope: app, Accounts: ckauthkit.IssuerAccounts(scope.TenantID, app.Authority)})
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []bindingCredential{
		{id: auth.Identity{Issuer: "https://foreign.test", Subject: "same", SubjectKind: auth.SubjectUser}},
		{id: auth.Identity{Issuer: app.Authority, Subject: "same", SubjectKind: auth.SubjectUser, TenantID: iam.NewTenantID().String()}},
		{id: auth.Identity{Issuer: app.Authority, Subject: "same", SubjectKind: auth.SubjectUser}, bound: auth.Scope{Authority: app.Authority, ID: "another-app"}},
		{id: auth.Identity{Issuer: app.Authority, Subject: "service", SubjectKind: auth.SubjectApplication}},
	} {
		if _, err := policy.ResolveActor(t.Context(), scope, v); err == nil {
			t.Errorf("accepted foreign identity/binding: %+v", v)
		}
	}
	v := bindingCredential{id: auth.Identity{Issuer: app.Authority, Subject: "same", SubjectKind: auth.SubjectUser}}
	allowed, err := policy.Authorize(t.Context(), scope, v, contentkit.PermissionTarget{Permission: access.CommentsModerate.String()})
	if err != nil || allowed {
		t.Fatalf("missing permission capability did not fail closed: %v %v", allowed, err)
	}
}
