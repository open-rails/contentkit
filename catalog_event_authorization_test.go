package contentkit

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/authkit/auth"
	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/kinds"
	"github.com/open-rails/helpers/eventlog"
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
	qs := pgx.Identifier{schema}.Sanitize()
	if _, err := pool.Exec(t.Context(), `INSERT INTO `+qs+`.content_event_streams(tenant_id,position) VALUES ('catalog-app',1);
 INSERT INTO `+qs+`.content_events(tenant_id,position,type,entity_id,payload) VALUES ('catalog-app',1,'content.created','retired-content','{"kind":"gallery","content_id":"retired-content","future_data":42}')`); err != nil {
		t.Fatal(err)
	}
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
			server := httptest.NewServer(mux)
			defer server.Close()
			clients := map[string]*Client{
				"HTTP":     NewClient(server.URL, server.Client()),
				"embedded": (&Engine{inproc: mux}).Client(),
			}
			for name, client := range clients {
				t.Run(name, func(t *testing.T) {
					want := tc.want
					if name == "embedded" {
						want = http.StatusOK
					} // Engine possession is host authority.
					reader := client.EventReader()
					page, err := reader.ListEvents(t.Context(), eventlog.Query{Limit: 1})
					if want != http.StatusOK {
						var refusal *Error
						if !errors.As(err, &refusal) || refusal.Status != want {
							t.Fatalf("authorization: %v", err)
						}
						return
					}
					if err != nil || page.NextCursor == "" {
						t.Fatalf("read: %+v %v", page, err)
					}
					if len(page.Events) != 1 || page.Events[0].Source != "contentkit" || page.Events[0].Scope != "catalog-app" || page.Events[0].Subject.ID != "retired-content" || page.Events[0].RecordedAt.IsZero() || !strings.Contains(string(page.Events[0].Data), "future_data") {
						t.Fatalf("envelope: %+v", page)
					}
					if _, err := reader.ListEvents(t.Context(), eventlog.Query{After: "bad"}); !errors.Is(err, eventlog.ErrInvalidCursor) {
						t.Fatalf("invalid cursor: %v", err)
					}
					if _, err := reader.ListEvents(t.Context(), eventlog.Query{Limit: 501}); !errors.Is(err, eventlog.ErrInvalidQuery) {
						t.Fatalf("invalid query: %v", err)
					}
					body, _ := base64.RawURLEncoding.DecodeString(page.NextCursor)
					var cursor map[string]any
					if err := json.Unmarshal(body, &cursor); err != nil {
						t.Fatal(err)
					}
					cursor["e"] = "abandoned-epoch"
					body, _ = json.Marshal(cursor)
					_, err = reader.ListEvents(t.Context(), eventlog.Query{After: base64.RawURLEncoding.EncodeToString(body)})
					var refusal *Error
					if !errors.Is(err, eventlog.ErrExpiredCursor) || !errors.As(err, &refusal) || refusal.Status != http.StatusGone {
						t.Fatalf("expired cursor: %v", err)
					}
					unknown, err := reader.ListEvents(t.Context(), eventlog.Query{Types: []string{"future,kind", "future,kind"}})
					if err != nil || len(unknown.Events) != 0 || unknown.NextCursor == "" {
						t.Fatalf("future type: %+v %v", unknown, err)
					}
					encoded, _ := base64.RawURLEncoding.DecodeString(unknown.NextCursor)
					var exact struct {
						Types []string `json:"t"`
					}
					if err := json.Unmarshal(encoded, &exact); err != nil || len(exact.Types) != 1 || exact.Types[0] != "future,kind" {
						t.Fatalf("exact filter was split: %s %v", encoded, err)
					}
					if _, err := reader.ListEvents(t.Context(), eventlog.Query{After: unknown.NextCursor, Types: []string{"future,kind"}}); err != nil {
						t.Fatalf("exact future type cursor: %v", err)
					}
				})
			}
		})
	}
}
