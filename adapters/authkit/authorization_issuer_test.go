package authkit_test

import (
	"context"
	"testing"

	"github.com/open-rails/authkit/auth"
	"github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/access"
	ckauthkit "github.com/open-rails/contentkit/adapters/authkit"
)

type permissionCredential struct {
	bindingCredential
	wantScope      auth.Scope
	wantPermission string
	checked        []auth.Scope
}

func (v *permissionCredential) CanDirect(_ context.Context, scope auth.Scope, permission string) (bool, error) {
	v.checked = append(v.checked, scope)
	return scope == v.wantScope && permission == v.wantPermission, nil
}

func TestAuthorizationSeparatesIdentityIssuerFromPermissionAuthority(t *testing.T) {
	const identityIssuer = "https://accounts.example"
	const permissionAuthority = "https://content.example"
	scope := contentkit.Scope{TenantID: contentkit.NewTenantID(), AppID: contentkit.NewAppID()}
	app := auth.Scope{Authority: permissionAuthority, ID: "app"}
	cfg := ckauthkit.AuthorizationConfig{
		Scope: scope, Issuer: identityIssuer, PermissionAuthority: permissionAuthority,
		AppScope: app, Accounts: ckauthkit.IssuerAccounts(scope.TenantID, identityIssuer),
	}
	policy, err := ckauthkit.NewAuthorization(t.Context(), authorizationGroups{app}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	credential := &permissionCredential{
		bindingCredential: bindingCredential{
			id: auth.Identity{Issuer: identityIssuer, Subject: "account", SubjectKind: auth.SubjectUser}, bound: app,
		},
		wantScope: app, wantPermission: ckauthkit.AppPermission(access.CommentsModerate),
	}
	if actor, err := policy.ResolveActor(t.Context(), scope, credential); err != nil || actor != "account" {
		t.Fatalf("trusted remote account: actor=%q err=%v", actor, err)
	}
	if allowed, err := policy.Authorize(t.Context(), scope, credential, contentkit.PermissionTarget{Permission: access.CommentsModerate.String()}); err != nil || !allowed {
		t.Fatalf("app grant at local authority: allowed=%v err=%v", allowed, err)
	}
	groupID, err := ckauthkit.GroupScopeID(scope, "group")
	if err != nil {
		t.Fatal(err)
	}
	credential.wantScope = auth.Scope{Authority: permissionAuthority, ID: groupID}
	credential.wantPermission = access.CommentsModerate.String()
	if allowed, err := policy.Authorize(t.Context(), scope, credential, contentkit.PermissionTarget{Permission: access.CommentsModerate.String(), GroupID: "group"}); err != nil || !allowed {
		t.Fatalf("content group at local authority: allowed=%v err=%v", allowed, err)
	}
	for _, checked := range credential.checked {
		if checked.Authority != permissionAuthority {
			t.Fatalf("permission check used identity issuer: %+v", checked)
		}
	}
	for _, bad := range []bindingCredential{
		{id: auth.Identity{Issuer: permissionAuthority, Subject: "account", SubjectKind: auth.SubjectUser}, bound: app},
		{id: credential.id, bound: auth.Scope{Authority: identityIssuer, ID: app.ID}},
	} {
		if _, err := policy.ResolveActor(t.Context(), scope, bad); err == nil {
			t.Fatalf("accepted wrong identity issuer or permission binding: %+v", bad)
		}
	}

	// The separate authority must be explicit; it is never inferred from a
	// credential, app scope, request header, or upstream account identifier.
	cfg.PermissionAuthority = ""
	if _, err := ckauthkit.NewAuthorization(t.Context(), authorizationGroups{app}, cfg); err == nil {
		t.Fatal("inferred a different permission authority without configuration")
	}
	cfg.PermissionAuthority = " https://content.example"
	if _, err := ckauthkit.NewAuthorization(t.Context(), authorizationGroups{app}, cfg); err == nil {
		t.Fatal("accepted malformed permission authority")
	}
	cfg.PermissionAuthority = permissionAuthority
	cfg.Groups = func(context.Context, contentkit.Scope, string) (auth.Scope, error) {
		return auth.Scope{Authority: identityIssuer, ID: groupID}, nil
	}
	policy, err = ckauthkit.NewAuthorization(t.Context(), authorizationGroups{app}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	before := len(credential.checked)
	if allowed, err := policy.Authorize(t.Context(), scope, credential, contentkit.PermissionTarget{Permission: access.CommentsModerate.String(), GroupID: "group"}); err == nil || allowed || len(credential.checked) != before {
		t.Fatalf("foreign content-group authority reached permission checker: allowed=%v err=%v", allowed, err)
	}
}
