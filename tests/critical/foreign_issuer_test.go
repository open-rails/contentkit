package critical_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/open-rails/authkit/auth"
	"github.com/open-rails/authkit/authtest"
	ck "github.com/open-rails/contentkit"
	ckauth "github.com/open-rails/contentkit/adapters/authkit"
)

// remoteDirectory is a credential as a foreign account directory's verifier
// reports it: AuthKit verified the user, and that directory's issuer and
// tenant claim are what the verifier attests. It grants nothing else.
type remoteDirectory struct {
	auth.Verified
	issuer, tenant string
}

func (r remoteDirectory) Identity() auth.Identity {
	id := r.Verified.Identity()
	id.Issuer, id.TenantID = r.issuer, r.tenant
	return id
}

// A foreign identity issuer's user is admitted only with a verified tenant
// claim naming the app's tenant: no issuer is trusted as a whole.
func TestForeignIssuerTenantClaim(t *testing.T) {
	w := newWorld(t)
	a := w.apps[0]
	ctx := t.Context()
	user, token := w.person(t, 0)
	r := httptest.NewRequest(http.MethodGet, "https://critical.invalid/", nil)
	authtest.Authorize(t, w.auth, r, token)
	verified, err := w.auth.Authenticator().Authenticate(r)
	must(t, err)
	scope := *a.cfg.Scope
	appScope, err := a.tenant.Scope(ctx, a.group)
	must(t, err)
	const foreign = "https://foreign-directory.critical.invalid"
	policy, err := ckauth.NewAuthorization(ctx, w.auth, ckauth.AuthorizationConfig{Scope: scope, Issuer: foreign, PermissionAuthority: appScope.Authority, AppScope: appScope})
	must(t, err)
	t.Run("CK-J02-S31_foreign_user_with_the_apps_tenant_claim_is_its_subject", func(t *testing.T) {
		v := remoteDirectory{verified, foreign, scope.TenantID.String()}
		actor, err := policy.ResolveActor(ctx, scope, v)
		must(t, err)
		account, member, err := policy.(ck.TenantAccountAuthorization).ResolveTenantAccount(ctx, scope, v)
		must(t, err)
		if actor != user.ID || account != user.ID || !member {
			t.Fatalf("foreign tenant member resolved as %q / %q member=%v", actor, account, member)
		}
	})
	t.Run("CK-J02-S31_foreign_user_of_another_tenant_or_without_a_claim_is_refused", func(t *testing.T) {
		for _, tenant := range []string{w.apps[2].cfg.Scope.TenantID.String(), ""} {
			if _, err := policy.ResolveActor(ctx, scope, remoteDirectory{verified, foreign, tenant}); !errors.Is(err, auth.ErrForbidden) {
				t.Fatalf("foreign user claiming tenant %q was admitted: %v", tenant, err)
			}
		}
	})
	t.Run("CK-J02-S31_native_users_keep_their_live_ownership", func(t *testing.T) {
		actor, err := a.deps.Authorization.ResolveActor(ctx, scope, verified)
		must(t, err)
		if actor != user.ID {
			t.Fatalf("native user resolved as %q", actor)
		}
	})
}
