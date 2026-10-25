package critical_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	ck "github.com/open-rails/contentkit"
	ckauth "github.com/open-rails/contentkit/adapters/authkit"
	"github.com/open-rails/contentkit/catalog"
)

func TestCredentialAndAppBoundary(t *testing.T) {
	w := newWorld(t)
	alice, token := w.person(t, 0)
	_, stranger := w.person(t, 1)
	a := w.apps[0]
	input := catalog.ContentInput{Kind: "article", Text: map[string]catalog.Text{"en": {Title: "Owned draft", Body: "Private app-bound body"}}}
	t.Run("CK-J02-S01_unauthorized_mutation_has_no_visible_effect", func(t *testing.T) {
		before, err := a.engine.Client().GetManage(t.Context(), ck.GetManageQuery{})
		must(t, err)
		_, err = a.client("").PostContents(t.Context(), input)
		requireError(t, err, 401, "unauthorized")
		after, err := a.engine.Client().GetManage(t.Context(), ck.GetManageQuery{})
		must(t, err)
		if len(after) != len(before) {
			t.Fatal("refused write changed the management listing")
		}
	})
	t.Run("CK-J02-S02_invalid_credential_is_not_anonymous", func(t *testing.T) {
		_, err := a.client("not-a-valid-credential").GetContents(t.Context(), ck.GetContentsQuery{})
		requireError(t, err, 401, "unauthorized")
	})
	t.Run("CK-J02-S03_expired_signed_credential_is_refused", func(t *testing.T) {
		claims, err := w.tenant[0].Verify(t.Context(), token)
		must(t, err)
		valid, err := w.tenant[0].MintAccessToken(t.Context(), alice.ID, iam.AccessTokenOptions{SessionID: claims.SessionID, TTL: time.Minute})
		must(t, err)
		_, err = a.client(valid.Value).GetContents(t.Context(), ck.GetContentsQuery{})
		must(t, err)
		// A real issuer signature over the actual session's claims, outside
		// AuthKit's documented 60-second skew. No verifier or store is mocked.
		parts := strings.Split(valid.Value, ".")
		if len(parts) != 3 {
			t.Fatal("issuer did not return a compact JWT")
		}
		payload, err := base64.RawURLEncoding.DecodeString(parts[1])
		must(t, err)
		var old map[string]any
		must(t, json.Unmarshal(payload, &old))
		old["iat"], old["exp"] = time.Now().Add(-time.Hour).Unix(), time.Now().Add(-2*time.Minute).Unix()
		payload, err = json.Marshal(old)
		must(t, err)
		input := parts[0] + "." + base64.RawURLEncoding.EncodeToString(payload)
		signature, err := w.signer.Sign(t.Context(), []byte(input))
		must(t, err)
		expired := input + "." + base64.RawURLEncoding.EncodeToString(signature)
		_, err = a.client(expired).GetContents(t.Context(), ck.GetContentsQuery{})
		requireError(t, err, 401, "unauthorized")
		// Open metadata deliberately does not authenticate even a bad token.
		_, err = a.client(expired).GetKinds(t.Context())
		must(t, err)
	})
	t.Run("CK-J02-S04_revoked_session_does_not_become_anonymous", func(t *testing.T) {
		user, token := w.person(t, 0)
		client := a.client(token)
		_, err := client.GetContents(t.Context(), ck.GetContentsQuery{})
		must(t, err)
		claims, err := w.tenant[0].Verify(t.Context(), token)
		must(t, err)
		who := authtest.Identity(t, w.tenant[0], token)
		must(t, w.tenant[0].RevokeSession(t.Context(), who, user.ID, claims.SessionID))
		_, err = client.GetContents(t.Context(), ck.GetContentsQuery{})
		requireError(t, err, 401, "unauthorized")
	})
	t.Run("CK-J02-S05_wrong_app_binding_is_refused", func(t *testing.T) {
		bound, err := w.tenant[0].CreateAPIKey(t.Context(), iam.SystemIdentity(), a.group, iam.NewAPIKey{Name: "critical app key", Role: w.writer})
		must(t, err)
		_, err = a.client(bound.Secret).GetContents(t.Context(), ck.GetContentsQuery{})
		must(t, err)
		_, err = w.apps[1].client(bound.Secret).GetContents(t.Context(), ck.GetContentsQuery{})
		requireError(t, err, 403, "forbidden")
		_, err = a.client(bound.Secret).GetMeFavorites(t.Context(), ck.GetMeFavoritesQuery{})
		requireError(t, err, 403, "forbidden")
	})
	t.Run("CK-J02-S06_global_operator_does_not_acquire_audience_membership", func(t *testing.T) {
		operator, credential := w.person(t, 0)
		authtest.GrantRole(t, w.auth, iam.RootGroup(), iam.UserSubject(operator.ID), w.globalReviewer)
		foreign := w.apps[2].client(credential)
		_, err := foreign.GetCommentsAdmin(t.Context(), ck.GetCommentsAdminQuery{})
		must(t, err)
		_, err = foreign.GetMeFavorites(t.Context(), ck.GetMeFavoritesQuery{})
		requireError(t, err, 403, "forbidden")
	})
	t.Run("CK-J02-S07_host_application_cannot_use_personal_routes", func(t *testing.T) {
		_, err := a.engine.Client().GetMeFavorites(t.Context(), ck.GetMeFavoritesQuery{})
		requireError(t, err, 403, "forbidden")
	})
	t.Run("CK-J02-S08_wire_headers_cannot_forge_host_authority", func(t *testing.T) {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, a.server.URL+"/content/v1/events", nil)
		must(t, err)
		req.Header.Set("X-ContentKit-Actor", "contentkit")
		req.Header.Set("X-ContentKit-Host", "true")
		req.Header.Set("X-User-ID", "contentkit")
		res, err := http.DefaultClient.Do(req)
		must(t, err)
		defer res.Body.Close()
		if res.StatusCode != 401 {
			t.Fatalf("forged host headers returned %d", res.StatusCode)
		}
	})
	t.Run("CK-J02-S09_auth_directory_outage_fails_closed", func(t *testing.T) {
		dsn, err := url.Parse(os.Getenv("CONTENTKIT_CRITICAL_PG"))
		must(t, err)
		wire := newTCPCut(t, dsn.Host)
		dsn.Host = wire.listener.Addr().String()
		pool, err := pgxpool.New(t.Context(), dsn.String())
		must(t, err)
		defer pool.Close()
		auth := authtest.Replica(t, w.auth, authtest.WithDeps(func(d *authkit.Deps) { d.Postgres = pool }))
		appScope, err := auth.Scope(t.Context(), a.group)
		must(t, err)
		policy, err := ckauth.NewAuthorization(t.Context(), auth, ckauth.AuthorizationConfig{Scope: *a.cfg.Scope, Issuer: authtest.Issuer, AppScope: appScope})
		must(t, err)
		cfg, deps := a.cfg, a.deps
		cfg.Database.SkipMigrations = true
		deps.Auth, deps.Authorization = auth.Authenticator(), policy
		engine, err := ck.New(t.Context(), cfg, deps)
		must(t, err)
		defer func() { must(t, engine.Close(context.Background())) }()
		routes, err := engine.Routes(ck.Routes{Groups: engine.RouteGroups()})
		must(t, err)
		mux := http.NewServeMux()
		for _, route := range routes {
			mux.Handle(route.Path, route.Handler)
		}
		srv := httptest.NewServer(mux)
		defer srv.Close()
		client := ck.NewClient(srv.URL, &http.Client{Transport: bearerTransport{token, http.DefaultTransport}})
		_, err = client.GetContents(t.Context(), ck.GetContentsQuery{})
		must(t, err)
		authtest.GrantRole(t, w.tenant[0], a.group, iam.UserSubject(alice.ID), w.writer)
		defer authtest.RevokeRole(t, w.tenant[0], a.group, iam.UserSubject(alice.ID), w.writer)
		created, err := client.PostContents(t.Context(), input)
		must(t, err)
		before, err := a.engine.Client().GetManage(t.Context(), ck.GetManageQuery{})
		must(t, err)
		wire.cut()
		if _, err := auth.Group(t.Context(), a.group); err == nil {
			t.Fatal("fault did not disconnect the actual AuthKit directory")
		}
		_, err = client.GetContents(t.Context(), ck.GetContentsQuery{})
		requireError(t, err, 503, "unavailable")
		_, err = client.PostContents(t.Context(), input)
		requireError(t, err, 503, "unavailable")
		after, err := a.engine.Client().GetManage(t.Context(), ck.GetManageQuery{})
		must(t, err)
		if len(before) != len(after) {
			t.Fatal("authentication outage still committed a content mutation")
		}
		kept, err := a.engine.Client().GetContent(t.Context(), created.ID, ck.GetContentQuery{})
		must(t, err)
		if kept.Body != "Private app-bound body" {
			t.Fatal("authentication outage changed previously committed content")
		}
		_, err = ck.NewClient(srv.URL, nil).GetContents(t.Context(), ck.GetContentsQuery{})
		must(t, err)
	})
	t.Run("CK-J03-S01_creator_and_app_writer_authority", func(t *testing.T) {
		authtest.GrantRole(t, w.tenant[0], a.group, iam.UserSubject(alice.ID), w.writer)
		if os.Getenv("CK_NEGATIVE_CONTROL") == "foreign-app-grant" {
			// Deliberately make the forbidden write possible. The cross-app
			// assertion below must fail; this is never enabled by normal CI.
			authtest.GrantRole(t, w.tenant[0], w.apps[1].group, iam.UserSubject(alice.ID), w.writer)
		}
		created, err := a.client(token).PostContents(t.Context(), input)
		must(t, err)
		if created.ID == "" || created.Kind != "article" {
			t.Fatalf("unexpected created content: %+v", created)
		}
		read, err := a.client(token).GetContent(t.Context(), created.ID, ck.GetContentQuery{})
		must(t, err)
		if read.ID != created.ID || read.Title != "Owned draft" || read.Body != "Private app-bound body" {
			t.Fatalf("created content did not survive public readback: %+v", read)
		}
		_, err = w.apps[1].client(token).PostContents(t.Context(), input)
		requireError(t, err, 403, "forbidden")
		_, err = w.apps[2].client(stranger).GetContent(t.Context(), created.ID, ck.GetContentQuery{})
		requireError(t, err, 404, "not_found")
		authtest.RevokeRole(t, w.tenant[0], a.group, iam.UserSubject(alice.ID), w.writer)
		ownTitle := "Creator may edit this"
		_, err = a.client(token).PatchContent(t.Context(), created.ID, catalog.ContentPatch{OriginalTitle: &ownTitle})
		must(t, err)
		owned, err := a.client(token).GetContent(t.Context(), created.ID, ck.GetContentQuery{})
		must(t, err)
		if owned.OriginalTitle != ownTitle {
			t.Fatal("accepted creator edit was not persisted")
		}
		bob, _ := w.person(t, 0)
		otherInput := input
		otherInput.Creators = &[]catalog.Creator{{UserID: bob.ID, Role: "author"}}
		other, err := a.engine.Client().PostContents(t.Context(), otherInput)
		must(t, err)
		_, err = a.client(token).PatchContent(t.Context(), other.ID, catalog.ContentPatch{OriginalTitle: &ownTitle})
		requireError(t, err, 403, "forbidden")
		unchanged, err := a.engine.Client().GetContent(t.Context(), other.ID, ck.GetContentQuery{})
		must(t, err)
		if unchanged.OriginalTitle == ownTitle {
			t.Fatal("a creator changed another creator's content")
		}
	})
	t.Run("CK-J03-S05_role_revocation_takes_effect_for_existing_client", func(t *testing.T) {
		user, credential := w.person(t, 0)
		authtest.GrantRole(t, w.tenant[0], a.group, iam.UserSubject(user.ID), w.writer)
		client := a.client(credential)
		_, err := client.PostContents(t.Context(), input)
		must(t, err)
		before, err := a.engine.Client().GetManage(t.Context(), ck.GetManageQuery{})
		must(t, err)
		authtest.RevokeRole(t, w.tenant[0], a.group, iam.UserSubject(user.ID), w.writer)
		_, err = client.PostContents(t.Context(), input)
		requireError(t, err, 403, "forbidden")
		after, err := a.engine.Client().GetManage(t.Context(), ck.GetManageQuery{})
		must(t, err)
		if len(before) != len(after) {
			t.Fatal("revoked writer still created content")
		}
	})
}

func requireError(t testing.TB, err error, status int, code string) {
	t.Helper()
	var got *ck.Error
	if !errors.As(err, &got) || got.Status != status || got.Code != code {
		t.Fatalf("got error %v; want HTTP %d / %s", err, status, code)
	}
}
