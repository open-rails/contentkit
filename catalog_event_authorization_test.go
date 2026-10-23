package contentkit

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/open-rails/authkit/auth"
	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/kinds"
)

type catalogEventsPolicy struct {
	allowed Scope
	stepUp  bool
}

func (p catalogEventsPolicy) ResolveActor(_ context.Context, scope Scope, _ auth.Verified) (string, error) {
	if scope.TenantID != p.allowed.TenantID {
		return "", auth.ErrForbidden
	}
	return "operator", nil
}

func (p catalogEventsPolicy) Authorize(_ context.Context, scope Scope, _ auth.Verified, target PermissionTarget) (bool, error) {
	if scope != p.allowed || target.Permission != access.ContentViewRestricted.String() {
		return false, nil
	}
	if p.stepUp {
		return false, &auth.Challenge{Err: auth.ErrStepUpRequired, Metadata: map[string]any{"methods": []string{"totp"}}}
	}
	return true, nil
}

func TestCatalogEventRouteEnforcesAppScopeAndAuthenticationChallenge(t *testing.T) {
	pool := pgtest.Pool(t, nil)
	schema := pgtest.Schema(t, t.Context(), pool)
	allowed := Scope{NewTenantID(), NewAppID()}
	for _, tc := range []struct {
		name   string
		scope  Scope
		stepUp bool
		want   int
	}{
		{"permitted app", allowed, false, http.StatusOK},
		{"sibling app", Scope{allowed.TenantID, NewAppID()}, false, http.StatusForbidden},
		{"other tenant", Scope{NewTenantID(), NewAppID()}, false, http.StatusForbidden},
		{"MFA step-up", allowed, true, http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := &Engine{scope: &tc.scope}
			a, err := e.authorization(authorizationAuthenticator{"https://accounts.test"}, catalogEventsPolicy{allowed: allowed, stepUp: tc.stepUp})
			if err != nil {
				t.Fatal(err)
			}
			store, err := catalog.New(t.Context(), catalog.Options{Pool: pool, Schema: schema, Tenant: "catalog-app", Auth: a, Kinds: kinds.Config{Languages: []string{"en"}, Kinds: map[string]kinds.Kind{"gallery": {Route: "g", Presentation: "reader", Versions: kinds.VersionRules{Languages: "one"}}}}})
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			r := httptest.NewRequest(http.MethodGet, "/v1/events", nil)
			r.Header.Set("Authorization", "Bearer testing")
			w := httptest.NewRecorder()
			mux := http.NewServeMux()
			mux.Handle("/v1/", http.StripPrefix("/v1", store.Handler()))
			mux.ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("status=%d want=%d: %s", w.Code, tc.want, w.Body.String())
			}
			if tc.stepUp && !strings.Contains(w.Header().Get("WWW-Authenticate"), "insufficient_user_authentication") {
				t.Fatal("event route lost the provider's step-up challenge")
			}
			if tc.want == http.StatusOK {
				server := httptest.NewServer(mux)
				defer server.Close()
				page, err := NewClient(server.URL, server.Client()).ListEvents(t.Context(), catalog.EventQuery{Limit: 1})
				if err != nil || page.NextCursor == "" {
					t.Fatalf("Go client event route: %+v %v", page, err)
				}
			}
		})
	}
}
