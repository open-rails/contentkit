package contentkit

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/authkit/auth"
	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/internal/testauth"
	"github.com/open-rails/contentkit/signal"
	"github.com/open-rails/helpers/notify"
)

type appAccountPolicy struct{ scope Scope }

type replayProvider struct{ fail bool }

func (p *replayProvider) EraseSubjects(context.Context, string, []string) error {
	if p.fail {
		return errors.New("provider erasure unavailable")
	}
	return nil
}

func (p appAccountPolicy) ResolveActor(_ context.Context, scope Scope, v auth.Verified) (string, error) {
	if scope != p.scope || v.Identity().Issuer != testauth.Issuer {
		return "", auth.ErrForbidden
	}
	return v.Identity().Subject, nil
}
func (p appAccountPolicy) Authorize(_ context.Context, scope Scope, v auth.Verified, _ PermissionTarget) (bool, error) {
	return scope == p.scope && v.Identity().Subject == "staff", nil
}

func TestTenantAppErasureAndRestrictions(t *testing.T) {
	ctx := t.Context()
	pool := pgtest.Pool(t, nil)
	var engines []*Engine
	var servers []server
	var contents []string
	var own []notify.Item
	var common []notify.Item
	user := access.Actor{ID: "same-account", Kind: auth.SubjectUser}
	staff := access.Actor{ID: "staff", Kind: auth.SubjectUser}
	recipient := notify.Recipient{Audience: notify.AudienceUser, ID: user.ID}
	accountSchema := engineSchema(t, pool)
	for range 2 {
		tenantID := NewTenantID()
		for appIndex := range 2 {
			scope := Scope{TenantID: tenantID, AppID: NewAppID()}
			cfg := scopeConfig(engineSchema(t, pool), scope, fmt.Sprintf("app_%d", appIndex))
			cfg.Accounts = &AccountsConfig{Schema: accountSchema}
			e, err := New(ctx, cfg, Deps{Postgres: pool, Auth: testauth.Auth{Authz: staffAuthz{}}, Authorization: appAccountPolicy{scope},
				Users: ownedFixtureRecipients{user.ID: {ID: user.ID}}})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = e.Close(context.Background()) })
			created, err := e.Client().PostContents(ctx, catalog.ContentInput{Kind: "gallery", Text: map[string]catalog.Text{"en": {Title: "Own catalog"}}, Versions: []catalog.VersionInput{{Languages: []string{"en"}}}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = e.Client().PostContentPublish(ctx, created.ID, catalog.PublishInput{}); err != nil {
				t.Fatal(err)
			}
			routes, err := e.Routes(Routes{Prefix: "/ck", Groups: e.RouteGroups()})
			if err != nil {
				t.Fatal(err)
			}
			srv := serve(t, routes)
			srv.call(200, user, "POST", "/ck/v1/contents/"+created.ID+"/like", nil, nil)
			n, err := e.notices.Publish(ctx, notify.Input{Recipient: recipient, Origin: notify.Origin{System: "contentkit", Scope: cfg.LegacyCatalogKey, ID: "app-notice"}, Kind: "test", CreatedAt: time.Now()})
			if err != nil {
				t.Fatal(err)
			}
			own = append(own, n)
			if appIndex == 0 {
				n, err := e.notices.Publish(ctx, notify.Input{Recipient: recipient, Origin: notify.Origin{System: "billing", Scope: "merchant", ID: "common"}, Kind: "test", Visibility: notify.VisibilityTenant, CreatedAt: time.Now()})
				if err != nil {
					t.Fatal(err)
				}
				common = append(common, n)
			}
			engines = append(engines, e)
			servers = append(servers, srv)
			contents = append(contents, created.ID)
		}
	}
	// An authorization-provider callback never gets to approve an unknown,
	// retired or sibling-only content-entity scope merely from its string ID.
	for i, e := range engines[:2] {
		_, err := pool.Exec(ctx, `INSERT INTO `+pgx.Identifier{e.schema, "content_entities"}.Sanitize()+`(tenant_id,id,kind,slug,auth_scope) VALUES($1,$2,'tag',$2,$3)`, e.cfg.Tenant, "same-group", "same-scope")
		if err != nil {
			t.Fatal(err)
		}
		if i == 1 {
			_, err = pool.Exec(ctx, `INSERT INTO `+pgx.Identifier{e.schema, "content_entities"}.Sanitize()+`(tenant_id,id,kind,slug,auth_scope) VALUES($1,'sibling','tag','sibling','sibling-scope')`, e.cfg.Tenant)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	verified, err := testauth.Verify(staff, testauth.All{})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		app   int
		scope string
		want  bool
	}{{0, "same-scope", true}, {0, "sibling-scope", false}, {1, "sibling-scope", true}, {2, "same-scope", false}} {
		allowed, err := engines[tc.app].auth.(scopeAuthenticator).Authorize(ctx, verified, tc.scope, "tag:settings:manage")
		if err != nil || allowed != tc.want {
			t.Fatalf("group scope %d/%s=%v %v", tc.app, tc.scope, allowed, err)
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE `+pgx.Identifier{engines[0].schema, "content_entities"}.Sanitize()+` SET state='deleted' WHERE id='same-group'`); err != nil {
		t.Fatal(err)
	}
	if allowed, err := engines[0].auth.(scopeAuthenticator).Authorize(ctx, verified, "same-scope", "tag:settings:manage"); err != nil || allowed {
		t.Fatalf("retired scope admitted: %v %v", allowed, err)
	}
	backup := pgx.Identifier{engines[0].schema, "pre_erasure_reactions"}.Sanitize()
	if _, err := pool.Exec(ctx, `CREATE TABLE `+backup+` AS TABLE `+pgx.Identifier{engines[0].schema, "content_reactions"}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	servers[0].call(200, staff, "PUT", "/ck/v1/global-comment-bans/"+user.ID, content.BanInput{Reason: "only this app"}, nil)
	for i, srv := range servers {
		var standing content.CommentStanding
		srv.call(http.StatusOK, user, "GET", "/ck/v1/contents/"+contents[i]+"/can-comment", nil, &standing)
		if standing.CanComment != (i != 0) {
			t.Fatalf("app %d ban leaked: %+v", i, standing)
		}
	}
	if report, err := engines[0].EraseAppSubjects(ctx, []signal.Subject{{UserID: user.ID}}); err != nil || !report.Complete() {
		t.Fatalf("app erasure %+v %v", report, err)
	}
	for i, e := range engines {
		var reactions int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+pgx.Identifier{e.schema, "content_reactions"}.Sanitize()+` WHERE tenant_id=$1 AND user_id=$2`, e.cfg.Tenant, user.ID).Scan(&reactions); err != nil {
			t.Fatal(err)
		}
		want := 1
		if i == 0 {
			want = 0
		}
		if reactions != want {
			t.Fatalf("app %d reactions=%d want=%d", i, reactions, want)
		}
		if _, err := e.notices.Get(ctx, recipient, common[i/2].ID); err != nil {
			t.Fatalf("shared notice removed from app %d: %v", i, err)
		}
	}
	if _, err := engines[0].notices.Get(ctx, recipient, own[0].ID); err == nil {
		t.Fatal("own app notice survived erasure")
	}
	if _, err := engines[1].notices.Get(ctx, recipient, own[1].ID); err != nil {
		t.Fatal("sibling notice erased", err)
	}
	// Restore actual pre-erasure app rows and its old (empty) local fence set.
	// Canonical shared fences survive, then replay removes the restored residue.
	if _, err := pool.Exec(ctx, `DELETE FROM `+pgx.Identifier{engines[0].schema, "content_erased_subjects"}.Sanitize()+`; INSERT INTO `+pgx.Identifier{engines[0].schema, "content_reactions"}.Sanitize()+` SELECT * FROM `+backup); err != nil {
		t.Fatal(err)
	}
	scope, _ := engines[0].Scope()
	restoredCfg := engines[0].cfg
	restoredCfg.Notifications = nil
	restoredCfg.Accounts = &AccountsConfig{Schema: accountSchema}
	restoredCfg.Scope, restoredCfg.LegacyCatalogKey, restoredCfg.Tenant = &scope, restoredCfg.Tenant, ""
	restoredCfg.Database.SkipMigrations = true
	provider := &replayProvider{fail: true}
	deps := Deps{Postgres: pool, Auth: testauth.Auth{Authz: staffAuthz{}}, Authorization: appAccountPolicy{scope}, ProviderDataEraser: provider}
	if blocked, err := New(ctx, restoredCfg, deps); !errors.Is(err, ErrErasureReplayRequired) {
		if blocked != nil {
			_ = blocked.Close(ctx)
		}
		t.Fatalf("restored data served before erasure replay: %v", err)
	}
	recovery, err := NewForErasureReplay(ctx, restoredCfg, deps)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := recovery.Routes(Routes{Groups: AllRoutes}); !errors.Is(err, ErrErasureReplayRequired) {
		t.Fatalf("recovery served routes: %v", err)
	}
	if _, err := recovery.Client().PostContents(ctx, catalog.ContentInput{Kind: "gallery"}); !errors.Is(err, ErrErasureReplayRequired) {
		t.Fatalf("recovery client mutated content: %v", err)
	}
	if err := recovery.Start(ctx); !errors.Is(err, ErrErasureReplayRequired) {
		t.Fatalf("recovery started workers: %v", err)
	}
	rec := httptest.NewRecorder()
	recovery.inproc.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/contents", nil))
	if rec.Code != 503 {
		t.Fatal("recovery inprocess handler served")
	}
	if report, err := recovery.ReplayErasureFences(ctx); err == nil || report.Complete() {
		t.Fatalf("failed provider cleanup marked complete: %+v %v", report, err)
	}
	if blocked, err := New(ctx, restoredCfg, deps); !errors.Is(err, ErrErasureReplayRequired) {
		if blocked != nil {
			_ = blocked.Close(ctx)
		}
		t.Fatalf("local fence bypassed unfinished full cleanup: %v", err)
	}
	provider.fail = false
	if report, err := recovery.ReplayErasureFences(ctx); err != nil || !report.Complete() {
		t.Fatalf("restore replay %+v %v", report, err)
	}
	_ = recovery.Close(ctx)
	_ = engines[0].Close(ctx)
	engines[0], err = New(ctx, restoredCfg, deps)
	if err != nil {
		t.Fatalf("replayed app did not boot: %v", err)
	}
	t.Cleanup(func() { _ = engines[0].Close(context.Background()) })
	var restored int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+pgx.Identifier{engines[0].schema, "content_reactions"}.Sanitize()+` WHERE user_id=$1`, user.ID).Scan(&restored); err != nil || restored != 0 {
		t.Fatalf("restored erased data survived: %d %v", restored, err)
	}
	if _, err := engines[0].notices.Get(ctx, recipient, common[0].ID); err != nil {
		t.Fatal("restore replay erased tenant receipt", err)
	}
	// The identity provider invokes account cleanup for every app. Only that
	// path erases tenant resources; another tenant with the same ID is untouched.
	for _, e := range engines[:2] {
		if report, err := e.EraseSubjects(ctx, []signal.Subject{{UserID: user.ID}}); err != nil || !report.Complete() {
			t.Fatalf("account erasure %+v %v", report, err)
		}
	}
	for i, e := range engines {
		page, err := e.notices.List(ctx, recipient, notify.ListOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if i < 2 && len(page.Items) != 0 {
			t.Fatal("tenant account inbox survived")
		}
		if i >= 2 && len(page.Items) != 2 {
			t.Fatalf("unrelated tenant affected: %+v", page)
		}
	}
}
