package authkit_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/open-rails/authkit/auth"
	"github.com/open-rails/authkit/iam"
	"github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/access"
	ckauthkit "github.com/open-rails/contentkit/adapters/authkit"
)

type tenantActorsFunc func(context.Context, auth.Verified) (iam.TenantActor, error)

func (f tenantActorsFunc) ResolveTenantActor(ctx context.Context, v auth.Verified) (iam.TenantActor, error) {
	return f(ctx, v)
}

func TestTenantActorsSeparateSourceFromTargetMembership(t *testing.T) {
	ctx := t.Context()
	scope := contentkit.Scope{TenantID: contentkit.NewTenantID(), AppID: contentkit.NewAppID()}
	targetTenant, _ := iam.ParseTenantID(scope.TenantID.String())
	sourceTenant := iam.NewTenantID()
	app := auth.Scope{Authority: "https://accounts.test", ID: scope.AppID.String()}
	credential := &permissionCredential{
		bindingCredential: bindingCredential{id: auth.Identity{Issuer: app.Authority, Subject: "source-actor", SubjectKind: auth.SubjectUser}},
		wantScope:         app, wantPermission: ckauthkit.AppPermission(access.CommentsModerate),
	}
	lookup := tenantActorsFunc(func(context.Context, auth.Verified) (iam.TenantActor, error) {
		return iam.TenantActor{AccountID: credential.id.Subject, TenantID: sourceTenant}, nil
	})
	policy, err := ckauthkit.NewAuthorization(ctx, authorizationGroups{app, scope.TenantID}, ckauthkit.AuthorizationConfig{
		Scope: scope, Issuer: app.Authority, AppScope: app, TenantActors: lookup,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, local := range []bool{false, true} {
		if local {
			sourceTenant = targetTenant
		}
		actor, err := policy.ResolveActor(ctx, scope, credential)
		if err != nil || actor != "source-actor" {
			t.Fatalf("source identity changed: %q %v", actor, err)
		}
		account, member, err := policy.(contentkit.TenantAccountAuthorization).ResolveTenantAccount(ctx, scope, credential)
		if err != nil || member != local || member && account != actor || !member && account != "" {
			t.Fatalf("target membership local=%v: account=%q member=%v err=%v", local, account, member, err)
		}
		allowed, err := policy.Authorize(ctx, scope, credential, contentkit.PermissionTarget{Permission: access.CommentsModerate.String()})
		if err != nil || !allowed {
			t.Fatalf("effective permission was incorrectly tied to audience membership: %v %v", allowed, err)
		}
	}
	credential.id.TenantID = iam.NewTenantID().String()
	if _, err := policy.ResolveActor(ctx, scope, credential); !errors.Is(err, auth.ErrForbidden) {
		t.Fatalf("signed and live ownership mismatch was accepted: %v", err)
	}
	credential.id.TenantID = ""
	credential.bound = auth.Scope{Authority: app.Authority, ID: "unrelated-app"}
	if _, err := policy.ResolveActor(ctx, scope, credential); !errors.Is(err, auth.ErrForbidden) {
		t.Fatalf("foreign credential binding was accepted: %v", err)
	}
}

func TestTenantActorsRejectUnverifiableOwnership(t *testing.T) {
	scope := contentkit.Scope{TenantID: contentkit.NewTenantID(), AppID: contentkit.NewAppID()}
	app := auth.Scope{Authority: "https://accounts.test", ID: scope.AppID.String()}
	v := bindingCredential{id: auth.Identity{Issuer: app.Authority, Subject: "source", SubjectKind: auth.SubjectUser}}
	for _, result := range []iam.TenantActor{{}, {AccountID: "other", TenantID: iam.NewTenantID()}, {AccountID: "source"}} {
		policy, err := ckauthkit.NewAuthorization(t.Context(), authorizationGroups{app, scope.TenantID}, ckauthkit.AuthorizationConfig{
			Scope: scope, Issuer: app.Authority, AppScope: app,
			TenantActors: tenantActorsFunc(func(context.Context, auth.Verified) (iam.TenantActor, error) { return result, nil }),
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := policy.ResolveActor(t.Context(), scope, v); !errors.Is(err, auth.ErrForbidden) {
			t.Fatalf("invalid live ownership accepted: %+v %v", result, err)
		}
	}
}

type personaGroups struct {
	authorizationGroups
	persona string
}

func (g personaGroups) Group(ctx context.Context, ref iam.GroupRef) (iam.Group, error) {
	group, err := g.authorizationGroups.Group(ctx, ref)
	if err == nil {
		err = group.Persona.UnmarshalText([]byte(g.persona))
	}
	return group, err
}

func TestAuthorizationRequiresExplicitLegacyPersonaMigration(t *testing.T) {
	scope := contentkit.Scope{TenantID: contentkit.NewTenantID(), AppID: contentkit.NewAppID()}
	app := auth.Scope{Authority: "https://accounts.test", ID: scope.AppID.String()}
	cfg := ckauthkit.AuthorizationConfig{Scope: scope, Issuer: app.Authority, AppScope: app,
		Accounts: ckauthkit.IssuerAccounts(scope.TenantID, app.Authority)}
	_, err := ckauthkit.NewAuthorization(t.Context(), personaGroups{authorizationGroups{app, scope.TenantID}, "contentkit"}, cfg)
	if !errors.Is(err, ckauthkit.ErrScopeMigrationRequired) {
		t.Fatalf("legacy group was silently adopted: %v", err)
	}
	_, err = ckauthkit.NewAuthorization(t.Context(), authorizationGroups{app, contentkit.NewTenantID()}, cfg)
	if err == nil {
		t.Fatal("a canonical app ID with foreign tenant ownership was accepted")
	}
}

type liveAuthorizationGroups struct {
	authorizationGroups
	groups map[string]iam.Group
	err    error
}

func (g *liveAuthorizationGroups) Group(_ context.Context, ref iam.GroupRef) (iam.Group, error) {
	if g.err != nil {
		return iam.Group{}, g.err
	}
	group, ok := g.groups[ref.ID()]
	if !ok {
		return iam.Group{}, iam.ErrGroupNotFound
	}
	return group, nil
}

func (g *liveAuthorizationGroups) Scope(ctx context.Context, ref iam.GroupRef) (auth.Scope, error) {
	if ref.IsRoot() {
		return g.authorizationGroups.Scope(ctx, ref)
	}
	return auth.Scope{Authority: g.scope.Authority, ID: ref.ID()}, nil
}

func TestAuthorizationRevalidatesLiveGroupOwnership(t *testing.T) {
	ctx := t.Context()
	scope := contentkit.Scope{TenantID: contentkit.NewTenantID(), AppID: contentkit.NewAppID()}
	tenant, _ := iam.ParseTenantID(scope.TenantID.String())
	app := auth.Scope{Authority: "https://accounts.test", ID: scope.AppID.String()}
	tenantScope := auth.Scope{Authority: app.Authority, ID: scope.TenantID.String()}
	contentID, err := ckauthkit.GroupScopeID(scope, "series")
	if err != nil {
		t.Fatal(err)
	}
	var appPersona, tenantPersona, contentPersona iam.Persona
	_ = appPersona.UnmarshalText([]byte("app"))
	_ = tenantPersona.UnmarshalText([]byte("tenant"))
	_ = contentPersona.UnmarshalText([]byte("series"))
	bindings := &liveAuthorizationGroups{
		authorizationGroups: authorizationGroups{app, scope.TenantID},
		groups: map[string]iam.Group{
			app.ID:         {ID: app.ID, TenantID: tenant, Persona: appPersona},
			tenantScope.ID: {ID: tenantScope.ID, TenantID: tenant, Persona: tenantPersona},
			contentID:      {ID: contentID, TenantID: tenant, Persona: contentPersona},
		},
	}
	policy, err := ckauthkit.NewAuthorization(ctx, bindings, ckauthkit.AuthorizationConfig{
		Scope: scope, Issuer: app.Authority, AppScope: app, TenantAdminScope: tenantScope,
		Accounts: ckauthkit.IssuerAccounts(scope.TenantID, app.Authority),
	})
	if err != nil {
		t.Fatal(err)
	}
	credential := &permissionCredential{
		bindingCredential: bindingCredential{id: auth.Identity{Issuer: app.Authority, Subject: "actor", SubjectKind: auth.SubjectUser}},
		wantScope:         app, wantPermission: ckauthkit.AppPermission(access.CommentsModerate),
	}
	if ok, err := policy.Authorize(ctx, scope, credential, contentkit.PermissionTarget{Permission: access.CommentsModerate.String()}); err != nil || !ok {
		t.Fatalf("initial live binding: %v %v", ok, err)
	}
	for _, test := range []struct {
		name   string
		id     string
		target contentkit.PermissionTarget
	}{
		{"app", app.ID, contentkit.PermissionTarget{Permission: access.CommentsModerate.String()}},
		{"tenant", tenantScope.ID, contentkit.PermissionTarget{Permission: access.AvatarsWrite.String()}},
		{"content", contentID, contentkit.PermissionTarget{Permission: "series:settings:manage", GroupID: "series"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			original := bindings.groups[test.id]
			for _, change := range []string{"foreign tenant", "deleted", "missing"} {
				group := original
				switch change {
				case "foreign tenant":
					group.TenantID = iam.NewTenantID()
				case "deleted":
					now := time.Now()
					group.DeletedAt = &now
				}
				bindings.groups[test.id] = group
				if change == "missing" {
					delete(bindings.groups, test.id)
				}
				before := len(credential.checked)
				if ok, err := policy.Authorize(ctx, scope, credential, test.target); ok || !errors.Is(err, auth.ErrForbidden) || len(credential.checked) != before {
					t.Fatalf("%s binding reached Can: %v %v", change, ok, err)
				}
				bindings.groups[test.id] = original
			}
		})
	}
	bindings.err = errors.New("directory offline")
	before := len(credential.checked)
	if ok, err := policy.Authorize(ctx, scope, credential, contentkit.PermissionTarget{Permission: access.CommentsModerate.String()}); ok || !errors.Is(err, auth.ErrUnavailable) || errors.Is(err, auth.ErrUnauthenticated) || len(credential.checked) != before {
		t.Fatalf("directory outage changed credential semantics: %v %v", ok, err)
	}
	if _, _, err := policy.(contentkit.TenantAccountAuthorization).ResolveTenantAccount(ctx, scope, credential); !errors.Is(err, auth.ErrUnavailable) || errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("membership lookup outage changed credential semantics: %v", err)
	}
}
