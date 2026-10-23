package server

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	"github.com/open-rails/contentkit/access"
	ckauthkit "github.com/open-rails/contentkit/adapters/authkit"
	"github.com/open-rails/contentkit/catalog"
)

func scopedServerFixture(t *testing.T, identityIssuer string) (Config, *pgxpool.Pool, *authkit.Client) {
	t.Helper()
	dsn := os.Getenv("CONTENTKIT_TEST_URL")
	if dsn == "" {
		t.Skip("CONTENTKIT_TEST_URL not set")
	}
	pool, err := pgxpool.New(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(t.Context(), `SELECT pg_advisory_lock(hashtext('contentkit:pgtest:extensions'));
CREATE EXTENSION IF NOT EXISTS pg_trgm SCHEMA public; CREATE EXTENSION IF NOT EXISTS pgroonga SCHEMA public;
SELECT pg_advisory_unlock(hashtext('contentkit:pgtest:extensions'))`); err != nil {
		t.Fatal(err)
	}
	schema := "ck_scoped_srv_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for _, s := range []string{schema, schema + "_river", schema + "_auth", schema + "_accounts"} {
			_, _ = pool.Exec(ctx, "DROP SCHEMA IF EXISTS "+pgx.Identifier{s}.Sanitize()+" CASCADE")
		}
		_, _ = pool.Exec(ctx, "DELETE FROM public.migrations WHERE schema = ANY($1)", []string{schema, schema + "_auth", schema + "_accounts"})
	})
	app, tenant := uuid.NewString(), uuid.NewString()
	cfg, err := Load(fmt.Appendf(nil, `
engine:
  scope: {tenant_id: %[3]s, app_id: %[2]s}
  database: {schema: %[1]s, river_schema: %[1]s_river}
  accounts: {schema: %[1]s_accounts}
  content:
    languages: [en]
    kinds:
      gallery: {route: g, presentation: reader, versions: {languages: one}, entities: [team]}
    entity_kinds:
      team: {route: team, auth_scope: true}
auth:
  default_tenant_id: %[3]s
  database: {schema: %[1]s_auth, river_schema: %[1]s_river}
  token: {issuer: https://contentkit.test, issued_audiences: [contentkit], expected_audiences: [contentkit]}
  resource: {id: https://contentkit.test}
authorization:
  issuer: %[4]s
  app_group_id: %[2]s
  tenant_group_id: %[3]s
prefix: /api/contentkit
`, schema, app, tenant, identityIssuer), func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	cfg.Auth.Keys.Path = filepath.Join(t.TempDir(), "keys")
	cfg.Auth.Keys.AllowEphemeralDevKeys = true
	cfg.Auth.HTTP = &authkit.HTTPConfig{DirectPeerIP: true}
	cfg.Auth.Roles = serverRoles(cfg.Engine)
	bootstrap, err := authkit.New(t.Context(), cfg.Auth, authkit.Deps{Postgres: pool})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bootstrap.Close(context.Background()) })
	for id, name := range map[string]string{app: "app", tenant: "tenant"} {
		persona, err := bootstrap.Persona(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := bootstrap.CreateGroup(t.Context(), iam.NewGroup{ID: id, Persona: persona, TenantID: cfg.Auth.DefaultTenantID}); err != nil {
			t.Fatal(err)
		}
	}
	return cfg, pool, bootstrap
}

func scopedStatus(s *Server, path, token string) int {
	r := httptest.NewRequest(http.MethodGet, "/api/contentkit/v1"+path, nil)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w.Code
}

func TestScopedServerLocalAuthorization(t *testing.T) {
	cfg, pool, bootstrap := scopedServerFixture(t, "https://contentkit.test")
	ordinary := authtest.NewUser(t, bootstrap)
	rootOnly := authtest.NewUser(t, bootstrap)
	rootOnly.TOTP = authtest.EnrollTOTP(t, bootstrap, rootOnly)
	staff := authtest.NewUser(t, bootstrap)
	sibling := authtest.NewUser(t, bootstrap)
	rootRole, _ := bootstrap.Role("root:owner")
	staffRole, _ := bootstrap.Role("app:staff")
	authtest.GrantRole(t, bootstrap, iam.RootGroup(), iam.UserSubject(rootOnly.ID), rootRole)
	authtest.GrantRole(t, bootstrap, iam.GroupByID(cfg.Authorization.AppGroupID), iam.UserSubject(staff.ID), staffRole)
	persona, _ := bootstrap.Persona("app")
	other, err := bootstrap.CreateGroup(t.Context(), iam.NewGroup{Persona: persona})
	if err != nil {
		t.Fatal(err)
	}
	authtest.GrantRole(t, bootstrap, iam.GroupByID(other.ID), iam.UserSubject(sibling.ID), staffRole)
	// Runtime must derive this from the configured ContentKit scope, rather
	// than accidentally relying on the bootstrap fixture's AuthKit default.
	cfg.Auth.DefaultTenantID = iam.TenantID{}
	s, err := New(t.Context(), cfg, Deps{Postgres: pool})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	native, err := s.auth.CreateUser(t.Context(), iam.NewUser{Email: "scoped-default@example.test", Username: "scopeddefault"})
	if err != nil || native.TenantID.String() != cfg.Engine.Scope.TenantID.String() {
		t.Fatalf("standalone runtime did not bind the canonical tenant: %v", err)
	}
	if got := scopedStatus(s, "/contents", ""); got != 200 {
		t.Fatalf("public contents: %d", got)
	}
	for _, tc := range []struct {
		name string
		user authtest.User
		want int
	}{{"ordinary", ordinary, 403}, {"root owner", rootOnly, 200}, {"app staff", staff, 200}, {"sibling staff", sibling, 403}} {
		t.Run(tc.name, func(t *testing.T) {
			token := authtest.SignIn(t, bootstrap, tc.user).AccessToken
			if got := scopedStatus(s, "/me/stats", token); got != 200 {
				t.Fatalf("ordinary account access: %d", got)
			}
			if got := scopedStatus(s, "/comments/admin", token); got != tc.want {
				t.Fatalf("staff access: %d, want %d", got, tc.want)
			}
		})
	}
	// The independently released server module must expose the same deletion
	// surface as embedded ContentKit, even without a sibling checkout.
	item, err := s.engine.Client().PostContents(t.Context(), catalog.ContentInput{
		Kind: "gallery", Text: map[string]catalog.Text{"en": {Title: "Version deletion"}},
	})
	if err != nil || len(item.Versions) != 1 {
		t.Fatalf("create edition: %+v %v", item, err)
	}
	for _, tc := range []struct {
		name  string
		token string
		want  int
	}{
		{"anonymous", "", http.StatusUnauthorized},
		{"ordinary", authtest.SignIn(t, bootstrap, ordinary).AccessToken, http.StatusForbidden},
		{"staff", authtest.SignIn(t, bootstrap, staff).AccessToken, http.StatusNoContent},
	} {
		t.Run("version deletion routes/"+tc.name, func(t *testing.T) {
			request := func(path string) int {
				r := httptest.NewRequest(http.MethodDelete, "/api/contentkit/v1"+path, nil)
				if tc.token != "" {
					r.Header.Set("Authorization", "Bearer "+tc.token)
				}
				w := httptest.NewRecorder()
				s.Handler().ServeHTTP(w, r)
				return w.Code
			}
			if got := request("/contents/" + item.ID); got != http.StatusMethodNotAllowed {
				t.Fatalf("parent deletion route remains: %d", got)
			}
			if got := request("/contents/" + item.ID + "/versions/" + item.Versions[0].ID); got != tc.want {
				t.Fatalf("version deletion authorization: %d, want %d", got, tc.want)
			}
		})
	}
}

func TestScopedServerRemoteAuthorization(t *testing.T) {
	const issuer = "https://users.test"
	cfg, pool, bootstrap := scopedServerFixture(t, issuer)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	role, err := bootstrap.Role("app:staff")
	if err != nil {
		t.Fatal(err)
	}
	remote := iam.RemoteApplication{Issuer: issuer, Enabled: true, Role: role, PublicKeys: []iam.RemoteApplicationKey{{KID: "remote", PublicKeyPEM: string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))}}}
	registered, err := bootstrap.UpsertRemoteApplication(t.Context(), iam.SystemIdentity(), iam.GroupByID(cfg.Authorization.AppGroupID), remote)
	if err != nil {
		t.Fatal(err)
	}
	authtest.GrantRole(t, bootstrap, iam.GroupByID(cfg.Authorization.AppGroupID), iam.RemoteApplicationSubject(registered.ID), role)
	s, err := New(t.Context(), cfg, Deps{Postgres: pool})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	sign := func(iss string, permissions []string) string {
		t.Helper()
		now := time.Now()
		token := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{"iss": iss, "sub": "remote-person", "aud": cfg.Auth.Resource.ID, "client_id": "remote-client", "iat": now.Unix(), "exp": now.Add(5 * time.Minute).Unix(), "jti": uuid.NewString(), "preferred_username": "remoteuser", "permissions": permissions})
		token.Header["typ"], token.Header["kid"] = "at+jwt", "remote"
		encoded, err := token.SignedString(key)
		if err != nil {
			t.Fatal(err)
		}
		return encoded
	}
	permission := ckauthkit.AppPermission(access.CommentsModerate)
	for _, tc := range []struct {
		name        string
		permissions []string
		want        int
	}{{"ordinary", nil, 403}, {"direct app permission", []string{permission}, 200}, {"root permission", []string{access.CommentsModerate.String()}, 403}} {
		t.Run(tc.name, func(t *testing.T) {
			token := sign(issuer, tc.permissions)
			if got := scopedStatus(s, "/me/stats", token); got != 200 {
				t.Fatalf("remote account access: %d", got)
			}
			if got := scopedStatus(s, "/comments/admin", token); got != tc.want {
				t.Fatalf("remote staff access: %d, want %d", got, tc.want)
			}
		})
	}
	if got := scopedStatus(s, "/me/stats", sign("https://untrusted.test", []string{permission})); got != 401 {
		t.Fatalf("untrusted issuer accepted: %d", got)
	}
	users, err := s.auth.RemoteUserInfo(iam.GroupByID(cfg.Authorization.AppGroupID), issuer).Get(t.Context(), []string{"remote-person"})
	if err != nil || users["remote-person"].Username != "remoteuser" {
		t.Fatalf("app-bound remote directory: %v %v", users, err)
	}
}

func TestScopedServerRequiresProvisionedBindings(t *testing.T) {
	cfg, pool, bootstrap := scopedServerFixture(t, "https://users.test")
	if _, err := bootstrap.UpsertRemoteApplication(t.Context(), iam.SystemIdentity(), iam.RootGroup(), iam.RemoteApplication{Issuer: cfg.Authorization.Issuer, Enabled: true, JWKSURI: "https://users.test/jwks"}); err != nil {
		t.Fatal(err)
	}
	if _, err := New(t.Context(), cfg, Deps{Postgres: pool}); err == nil || !strings.Contains(err.Error(), "remote application") {
		t.Fatalf("root-only remote application accepted: %v", err)
	}
	cfg.Authorization.Issuer = cfg.Auth.Token.Issuer
	cfg.Authorization.AppGroupID = uuid.NewString()
	missing := *cfg.Engine.Scope
	if err := missing.AppID.UnmarshalText([]byte(cfg.Authorization.AppGroupID)); err != nil {
		t.Fatal(err)
	}
	cfg.Engine.Scope = &missing
	if _, err := New(t.Context(), cfg, Deps{Postgres: pool}); err == nil || !strings.Contains(err.Error(), "must be provisioned") {
		t.Fatalf("missing app group accepted: %v", err)
	}
	if _, err := bootstrap.Group(t.Context(), iam.GroupByID(cfg.Authorization.AppGroupID)); err == nil {
		t.Fatal("startup silently provisioned the missing group")
	}
}
