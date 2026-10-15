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
	r := authkit.NewRoles()
	app := r.Persona("contentkit")
	moderate := app.Declare(ckauthkit.AppPermission(access.CommentsModerate))[0]
	appRole := app.Role("moderator", moderate)
	r.Root.Role("global", moderate)
	tenantPersona := r.Persona("contentkit-tenant")
	tenantPermission := tenantPersona.Declare(ckauthkit.TenantPermission(access.CommentsModerate))[0]
	tenantRole := tenantPersona.Role("moderator", tenantPermission)
	ak, _ := authtest.New(t, authtest.WithConfig(func(c *authkit.Config) { c.Roles = r }))
	person := authtest.NewUser(t, ak)
	token := authtest.SignIn(t, ak, person).AccessToken
	request := httptest.NewRequest("GET", "https://example.com/contentkit", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	v, err := ak.Authenticator().Authenticate(request)
	if err != nil {
		t.Fatal(err)
	}
	tenantID := contentkit.NewTenantID()
	appPersona, err := ak.Persona("contentkit")
	if err != nil {
		t.Fatal(err)
	}
	tenantKind, err := ak.Persona("contentkit-tenant")
	if err != nil {
		t.Fatal(err)
	}
	var policies []contentkit.Authorization
	var scopes []contentkit.Scope
	var groups []iam.GroupRef
	tenantGroup, err := ak.CreateGroup(ctx, iam.NewGroup{Persona: tenantKind})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		group, err := ak.CreateGroup(ctx, iam.NewGroup{Persona: appPersona})
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
		scope := contentkit.Scope{TenantID: tenantID, AppID: contentkit.NewAppID()}
		policy, err := ckauthkit.NewAuthorization(ctx, ak, ckauthkit.AuthorizationConfig{Scope: scope, Issuer: authtest.Issuer, AppScope: groupScope, TenantAdminScope: tenantScope, Accounts: ckauthkit.IssuerAccounts(tenantID, authtest.Issuer)})
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
		if err != nil || allowed {
			t.Fatalf("inherited root role admitted app %d: %v %v", i, allowed, err)
		}
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

type authorizationGroups struct{ scope auth.Scope }

func (g authorizationGroups) Group(context.Context, iam.GroupRef) (iam.Group, error) {
	var p iam.Persona
	_ = p.UnmarshalText([]byte("contentkit"))
	return iam.Group{ID: g.scope.ID, Persona: p}, nil
}
func (g authorizationGroups) Scope(context.Context, iam.GroupRef) (auth.Scope, error) {
	return g.scope, nil
}

func TestAuthorizationRejectsForeignIdentityAndBinding(t *testing.T) {
	scope := contentkit.Scope{TenantID: contentkit.NewTenantID(), AppID: contentkit.NewAppID()}
	app := auth.Scope{Authority: "https://id.test", ID: "app"}
	policy, err := ckauthkit.NewAuthorization(t.Context(), authorizationGroups{app}, ckauthkit.AuthorizationConfig{Scope: scope, Issuer: app.Authority, AppScope: app, Accounts: ckauthkit.IssuerAccounts(scope.TenantID, app.Authority)})
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []bindingCredential{
		{id: auth.Identity{Issuer: "https://foreign.test", Subject: "same", SubjectKind: auth.SubjectUser}},
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
		t.Fatalf("missing direct capability did not fail closed: %v %v", allowed, err)
	}
}
