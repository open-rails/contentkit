package critical_test

import (
	"testing"

	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/access"
	ckauth "github.com/open-rails/contentkit/adapters/authkit"
	"github.com/open-rails/contentkit/catalog"
)

func TestInheritedAuthorityCredentialCeilings(t *testing.T) {
	var read iam.Perm
	must(t, read.UnmarshalText([]byte(ckauth.AppPermission(access.ContentViewRestricted))))
	var rootReader, tenantReader iam.Role
	w := newWorldAuth(t, func(c *authkit.Config) {
		rootReader = c.Roles.Root.Role("critical-reader", read)
		tenantReader = c.Roles.Tenant.Role("critical-reader", read)
	})
	a := w.apps[0]
	tenantID, err := iam.ParseTenantID(a.cfg.Scope.TenantID.String())
	must(t, err)
	_, err = w.auth.CreateGroup(t.Context(), iam.NewGroup{ID: tenantID.String(), TenantID: tenantID, Persona: iam.TenantPersona()})
	must(t, err)
	tenantScope, err := w.auth.Scope(t.Context(), iam.GroupByID(tenantID.String()))
	must(t, err)
	appScope, err := w.auth.Scope(t.Context(), a.group)
	must(t, err)
	policy, err := ckauth.NewAuthorization(t.Context(), w.auth, ckauth.AuthorizationConfig{Scope: *a.cfg.Scope, Issuer: authtest.Issuer, AppScope: appScope, TenantAdminScope: tenantScope})
	must(t, err)
	a.server.Close()
	must(t, a.engine.Close(t.Context()))
	a.deps.Authorization = policy
	a.cfg.Database.SkipMigrations = true
	a.engine, err = ck.New(t.Context(), a.cfg, a.deps)
	must(t, err)
	a.server = mountEngine(t, a.engine)
	appPersona, err := w.auth.Persona("app")
	must(t, err)

	t.Run("CK-J03-S04_root_tenant_and_app_keys_cannot_exceed_their_role", func(t *testing.T) {
		for _, sample := range []struct {
			name    string
			group   iam.GroupRef
			role    iam.Role
			limited iam.Role
		}{
			{"root", iam.RootGroup(), iam.RootPersona().OwnerRole(), rootReader},
			{"tenant", iam.GroupByID(tenantID.String()), iam.TenantPersona().OwnerRole(), tenantReader},
			{"app", a.group, appPersona.OwnerRole(), w.viewer},
		} {
			t.Run(sample.name, func(t *testing.T) {
				user := authtest.NewUser(t, w.tenant[0])
				user.TOTP = authtest.EnrollTOTP(t, w.tenant[0], user)
				credential := authtest.SignIn(t, w.tenant[0], user).AccessToken
				authtest.GrantRole(t, w.auth, sample.group, iam.UserSubject(user.ID), sample.role)
				who := authtest.Identity(t, w.tenant[0], credential)
				limited, err := w.auth.CreateAPIKey(t.Context(), who, sample.group, iam.NewAPIKey{Name: "Read-only critical key", Role: sample.limited})
				must(t, err)
				full, narrow := a.client(credential), a.client(limited.Secret)
				input := catalog.ContentInput{Kind: "article", Versions: []catalog.VersionInput{{Languages: []string{"en"}, Text: map[string]catalog.VersionText{"en": {Title: sample.name + " writer", Body: "Real owned content"}}}}}
				item, err := full.PostContents(t.Context(), input)
				must(t, err)
				events, err := narrow.ListEvents(t.Context(), catalog.EventQuery{})
				must(t, err)
				if len(events.Events) == 0 {
					t.Fatal("read-only token did not read its granted event stream")
				}
				before, err := full.GetManage(t.Context(), ck.GetManageQuery{})
				must(t, err)
				_, err = narrow.PostContents(t.Context(), input)
				requireError(t, err, 403, "forbidden")
				title := "Credential exceeded its ceiling"
				_, err = narrow.PatchContent(t.Context(), item.ID, catalog.ContentPatch{OriginalTitle: &title})
				requireError(t, err, 403, "forbidden")
				after, err := full.GetManage(t.Context(), ck.GetManageQuery{})
				must(t, err)
				if len(before) != len(after) {
					t.Fatal("ceiling refusal still created content")
				}
				stored, err := full.GetContent(t.Context(), item.ID, ck.GetContentQuery{})
				must(t, err)
				if stored.OriginalTitle == title {
					t.Fatal("creator ownership bypassed the credential ceiling")
				}
				_, err = w.apps[1].client(limited.Secret).ListEvents(t.Context(), catalog.EventQuery{})
				if sample.name == "root" {
					must(t, err)
				} else {
					requireError(t, err, 403, "forbidden")
				}
				must(t, w.auth.RevokeAPIKey(t.Context(), who, sample.group, limited.APIKey.ID))
				_, err = narrow.ListEvents(t.Context(), catalog.EventQuery{})
				requireError(t, err, 401, "unauthorized")
			})
		}
	})
}
