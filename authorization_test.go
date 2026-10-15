package contentkit

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/open-rails/authkit/auth"
	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/groups"
	"github.com/open-rails/contentkit/internal/gate"
	"github.com/open-rails/contentkit/internal/httpapi"
	"github.com/open-rails/contentkit/kinds"
)

type authorizationFixture struct {
	tenant                      TenantID
	app                         AppID
	deny                        bool
	actorCalls, permissionCalls int
}

type boundScopesFixture struct {
	groups.Scopes
	tenant, app string
}

func (s boundScopesFixture) ContentKitScope() (string, string) { return s.tenant, s.app }

func TestScopedGroupProviderBinding(t *testing.T) {
	scope := Scope{TenantID: NewTenantID(), AppID: NewAppID()}
	e := Engine{scope: &scope, cfg: Config{Content: kinds.Config{GroupKinds: map[string]kinds.GroupKind{"channel": {AuthScope: true}}}}}
	if err := e.validateGroupScopes(nil); err == nil {
		t.Fatal("accepted unbound provider")
	}
	for _, p := range []boundScopesFixture{{tenant: scope.TenantID.String(), app: NewAppID().String()}, {tenant: NewTenantID().String(), app: scope.AppID.String()}, {}} {
		if err := e.validateGroupScopes(p); err == nil {
			t.Fatal("accepted foreign provider")
		}
	}
	if err := e.validateGroupScopes(boundScopesFixture{tenant: scope.TenantID.String(), app: scope.AppID.String()}); err != nil {
		t.Fatal(err)
	}
	e.cfg.Content.GroupKinds = nil
	if err := e.validateGroupScopes(nil); err != nil {
		t.Fatal("ordinary app requires unused group provider", err)
	}
}

func (a *authorizationFixture) ResolveActor(_ context.Context, scope Scope, v auth.Verified) (string, error) {
	a.actorCalls++
	if a.deny || scope.TenantID != a.tenant || v.Identity().Issuer != "https://accounts.test" {
		return "", auth.ErrForbidden
	}
	return "account:" + v.Identity().Subject, nil
}

func (a *authorizationFixture) Authorize(_ context.Context, scope Scope, v auth.Verified, target PermissionTarget) (bool, error) {
	a.permissionCalls++
	if v.Identity().Subject != "person" {
		return false, errors.New("original verified identity was lost")
	}
	return scope.TenantID == a.tenant && scope.AppID == a.app && target.Permission == access.CommentsModerate.String(), nil
}

type authorizationCredential struct{ issuer string }

func (v authorizationCredential) Identity() auth.Identity {
	return auth.Identity{Issuer: v.issuer, Subject: "person", SubjectKind: auth.SubjectUser}
}
func (authorizationCredential) Can(context.Context, auth.Scope, string) (bool, error) {
	return true, nil
}

type authorizationAuthenticator struct{ issuer string }

func (a authorizationAuthenticator) Authenticate(*http.Request) (auth.Verified, error) {
	return authorizationCredential{a.issuer}, nil
}
func (authorizationAuthenticator) RootScope(context.Context) (auth.Scope, error) {
	return auth.Scope{Authority: "root", ID: "root"}, nil
}

func TestScopedAuthorizationSeparatesAccountAndAppAuthority(t *testing.T) {
	tenant := NewTenantID()
	app := NewAppID()
	policy := &authorizationFixture{tenant: tenant, app: app}
	for _, tc := range []struct {
		name   string
		scope  Scope
		issuer string
		want   int
	}{
		{"app with role", Scope{tenant, app}, "https://accounts.test", 204},
		{"sibling app without role", Scope{tenant, NewAppID()}, "https://accounts.test", 403},
		{"unrelated tenant same subject", Scope{NewTenantID(), NewAppID()}, "https://accounts.test", 403},
		{"foreign issuer same subject", Scope{tenant, app}, "https://foreign.test", 403},
		{"missing issuer", Scope{tenant, app}, "", 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := Engine{scope: &tc.scope}
			a, err := e.authorization(authorizationAuthenticator{tc.issuer}, policy)
			if err != nil {
				t.Fatal(err)
			}
			if err := gate.Check(a, access.CommentsModerate); err != nil {
				t.Fatal(err)
			}
			g := gate.Gate{Auth: a}
			h := g.Guard(httpapi.Spec{Auth: httpapi.Staff, Perm: access.CommentsModerate.String()}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				actor := gate.Actor(r.Context())
				if actor.ID != "account:person" || actor.Verified.Identity().Subject != "person" {
					t.Errorf("account mapping changed signed identity: %+v", actor)
				}
				before := policy.permissionCalls
				if ok, err := gate.Can(r.Context(), a, actor, access.CommentsModerate.String()); !ok || err != nil {
					t.Errorf("handler permission: %v %v", ok, err)
				}
				if policy.permissionCalls != before {
					t.Error("permission check was not memoized")
				}
				w.WriteHeader(204)
			}))
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
			if w.Code != tc.want {
				t.Fatalf("status=%d want=%d: %s", w.Code, tc.want, w.Body.String())
			}
		})
	}
}

func TestScopedAuthorizationOrdinaryAccountNeedsNoAppRole(t *testing.T) {
	scope := Scope{NewTenantID(), NewAppID()}
	policy := &authorizationFixture{tenant: scope.TenantID, app: NewAppID()}
	e := Engine{scope: &scope}
	a, err := e.authorization(authorizationAuthenticator{"https://accounts.test"}, policy)
	if err != nil {
		t.Fatal(err)
	}
	g := gate.Gate{Auth: a}
	w := httptest.NewRecorder()
	g.Guard(httpapi.Spec{Auth: httpapi.User}, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })).ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != 204 || policy.permissionCalls != 0 {
		t.Fatalf("ordinary account required app role: status=%d checks=%d", w.Code, policy.permissionCalls)
	}
}

func TestScopedAuthorizationRequiresExplicitBinding(t *testing.T) {
	scope := Scope{NewTenantID(), NewAppID()}
	e := Engine{scope: &scope}
	var nilPolicy *authorizationFixture
	for _, policy := range []Authorization{nil, nilPolicy} {
		if _, err := e.authorization(authorizationAuthenticator{}, policy); err == nil {
			t.Error("accepted absent authorization")
		}
	}
	policy := &authorizationFixture{}
	if _, err := e.authorization(nil, policy); err == nil {
		t.Error("accepted nil authenticator")
	}
	legacy := Engine{}
	if _, err := legacy.authorization(authorizationAuthenticator{}, policy); err == nil {
		t.Error("accepted scoped authorization without scope")
	}
}
