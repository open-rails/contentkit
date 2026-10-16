package authkit_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/auth"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	"github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/access"
	ckauthkit "github.com/open-rails/contentkit/adapters/authkit"
)

type publishingNarrowed struct{ auth.Verified }

func (v publishingNarrowed) BoundScope() auth.Scope { return v.Verified.(auth.Bound).BoundScope() }
func (v publishingNarrowed) Can(ctx context.Context, scope auth.Scope, permission string) (bool, error) {
	if permission != ckauthkit.AppPermission(access.ContentWrite) {
		return false, nil
	}
	return v.Verified.(auth.PermissionChecker).Can(ctx, scope, permission)
}

type publishingIdentityOnly struct{ bindingCredential }

func (publishingIdentityOnly) Can(context.Context, auth.Scope, string) (bool, error) {
	return true, nil
}

func TestPublishingAuthorityNativeAPIKey(t *testing.T) {
	ctx := t.Context()
	tenant := contentkit.NewTenantID()
	nativeTenant, _ := iam.ParseTenantID(tenant.String())
	roles := authkit.NewRoles(authkit.APIKeys)
	appPersona := roles.Persona("app", authkit.APIKeys)
	permissions := appPersona.Declare(ckauthkit.AppPermission(access.ContentWrite), ckauthkit.AppPermission(access.EntitiesWrite))
	publisher := appPersona.Role("publisher", permissions[0], permissions[1])
	globalPublisher := roles.Root.Role("publisher", permissions[0], permissions[1])
	ak, _ := authtest.New(t, authtest.WithConfig(func(c *authkit.Config) {
		c.Roles, c.DefaultTenantID = roles, nativeTenant
	}))
	persona, err := ak.Persona("app")
	if err != nil {
		t.Fatal(err)
	}
	type target struct {
		scope  contentkit.Scope
		group  iam.GroupRef
		policy contentkit.PublishingAuthorization
	}
	newTarget := func() target {
		scope := contentkit.Scope{TenantID: tenant, AppID: contentkit.NewAppID()}
		group, err := ak.CreateGroup(ctx, iam.NewGroup{ID: scope.AppID.String(), TenantID: nativeTenant, Persona: persona})
		if err != nil {
			t.Fatal(err)
		}
		ref := iam.GroupByID(group.ID)
		bound, err := ak.Scope(ctx, ref)
		if err != nil {
			t.Fatal(err)
		}
		policy, err := ckauthkit.NewAuthorization(ctx, ak, ckauthkit.AuthorizationConfig{Scope: scope, Issuer: authtest.Issuer, AppScope: bound})
		if err != nil {
			t.Fatal(err)
		}
		return target{scope, ref, policy.(contentkit.PublishingAuthorization)}
	}
	one, two := newTarget(), newTarget()
	key, err := ak.CreateAPIKey(ctx, iam.SystemIdentity(), one.group, iam.NewAPIKey{Name: "publisher fixture", Role: publisher})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("PUT", "https://example.com/v1/publishing-operations/test", nil)
	request.Header.Set("Authorization", "Bearer "+key.Secret)
	verified, err := ak.Authenticator().Authenticate(request)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := one.policy.CapturePublishing(ctx, one.scope, publishingNarrowed{verified})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(stored, []byte(key.Secret)) {
		t.Fatal("stored the API key secret")
	}
	resumed, err := one.policy.ResumePublishing(ctx, one.scope, stored)
	if err != nil {
		t.Fatal(err)
	}
	bound := resumed.(auth.Bound).BoundScope()
	for _, tc := range []struct {
		permission access.Permission
		want       bool
	}{{access.ContentWrite, true}, {access.EntitiesWrite, false}} {
		allowed, err := resumed.(auth.PermissionChecker).Can(ctx, bound, ckauthkit.AppPermission(tc.permission))
		if err != nil || allowed != tc.want {
			t.Fatalf("original ceiling %s: allowed=%v err=%v", tc.permission, allowed, err)
		}
	}
	if _, err := two.policy.CapturePublishing(ctx, two.scope, verified); !errors.Is(err, auth.ErrForbidden) {
		t.Fatalf("foreign app captured key: %v", err)
	}
	if _, err := two.policy.ResumePublishing(ctx, two.scope, stored); !errors.Is(err, auth.ErrForbidden) {
		t.Fatalf("foreign app resumed delegation: %v", err)
	}
	var tampered map[string]any
	if err := json.Unmarshal(stored, &tampered); err != nil {
		t.Fatal(err)
	}
	tampered["app"] = two.scope.AppID.String()
	wrong, _ := json.Marshal(tampered)
	if _, err := one.policy.ResumePublishing(ctx, one.scope, wrong); !errors.Is(err, auth.ErrForbidden) {
		t.Fatalf("different app in stored delegation accepted: %v", err)
	}
	// Contact/identity fields cannot stand in for the provider-owned state.
	bare := publishingIdentityOnly{bindingCredential{id: verified.Identity(), bound: bound}}
	bare.id.Credential = auth.Credential{Kind: auth.CredentialAPIKey, ID: key.APIKey.ID}
	if _, err := one.policy.CapturePublishing(ctx, one.scope, bare); !errors.Is(err, auth.ErrForbidden) {
		t.Fatalf("identity-only key captured: %v", err)
	}
	user := authtest.NewUser(t, ak)
	request.Header.Set("Authorization", "Bearer "+authtest.SignIn(t, ak, user).AccessToken)
	human, err := ak.Authenticator().Authenticate(request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := one.policy.CapturePublishing(ctx, one.scope, human); !errors.Is(err, auth.ErrForbidden) {
		t.Fatalf("interactive credential captured as a durable key: %v", err)
	}
	global, err := ak.CreateAPIKey(ctx, iam.SystemIdentity(), iam.RootGroup(), iam.NewAPIKey{Name: "global publisher fixture", Role: globalPublisher})
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+global.Secret)
	rootCredential, err := ak.Authenticator().Authenticate(request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := one.policy.CapturePublishing(ctx, one.scope, rootCredential); !errors.Is(err, auth.ErrForbidden) {
		t.Fatalf("global key implicitly delegated into an app: %v", err)
	}
	until := time.Now().Add(2 * time.Second)
	expiring, err := ak.CreateAPIKey(ctx, iam.SystemIdentity(), one.group, iam.NewAPIKey{Name: "expiring publisher fixture", Role: publisher, ExpiresAt: &until})
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+expiring.Secret)
	soonExpired, err := ak.Authenticator().Authenticate(request)
	if err != nil {
		t.Fatal(err)
	}
	expiringDelegation, err := one.policy.CapturePublishing(ctx, one.scope, soonExpired)
	if err != nil {
		t.Fatal(err)
	}
	timer := time.NewTimer(time.Until(until) + 25*time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	case <-timer.C:
	}
	if _, err := one.policy.ResumePublishing(ctx, one.scope, expiringDelegation); !errors.Is(err, auth.ErrForbidden) {
		t.Fatalf("expired key resumed: %v", err)
	}
	if err := ak.RevokeAPIKey(ctx, iam.SystemIdentity(), one.group, key.APIKey.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := one.policy.ResumePublishing(ctx, one.scope, stored); !errors.Is(err, auth.ErrForbidden) {
		t.Fatalf("revoked key resumed: %v", err)
	}
	if allowed, err := resumed.(auth.PermissionChecker).Can(ctx, bound, ckauthkit.AppPermission(access.ContentWrite)); err != nil || allowed {
		t.Fatalf("resumed actor retained revoked authority: allowed=%v err=%v", allowed, err)
	}
}
