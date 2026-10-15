package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	"github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/notification"
	"github.com/open-rails/helpers/notify"
)

func TestScopedServerNotificationTargetMembership(t *testing.T) {
	cfg, pool, bootstrap := scopedServerFixture(t, "https://contentkit.test")
	member := authtest.NewUser(t, bootstrap)
	foreignTenant := iam.NewTenantID()
	if err := bootstrap.CreateTenant(t.Context(), iam.Tenant{ID: foreignTenant}); err != nil {
		t.Fatal(err)
	}
	foreign, err := bootstrap.ForTenant(t.Context(), foreignTenant)
	if err != nil {
		t.Fatal(err)
	}
	operator := authtest.NewUser(t, foreign)
	operator.TOTP = authtest.EnrollTOTP(t, foreign, operator)
	owner, err := bootstrap.Role("root:owner")
	if err != nil {
		t.Fatal(err)
	}
	authtest.GrantRole(t, bootstrap, iam.RootGroup(), iam.UserSubject(operator.ID), owner)
	token := authtest.SignIn(t, foreign, operator).AccessToken
	setup, cancelSetup := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancelSetup()
	if err := contentkit.MigratePostgres(setup, pool, cfg.Engine); err != nil {
		t.Fatal(err)
	}
	cfg.Engine.Database.SkipMigrations = true
	cfg.Auth.Database.SkipMigrations = true
	poolConfig := pool.Config()
	poolConfig.MaxConns = 1
	oneConnection, err := pgxpool.NewWithConfig(t.Context(), poolConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(oneConnection.Close)
	s, err := New(setup, cfg, Deps{Postgres: oneConnection})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	// A source-tenant root operator can publish to the configured target tenant,
	// but neither their source account nor an invented recipient belongs to it.
	for _, tc := range []struct {
		id   string
		want int
	}{{member.ID, 200}, {operator.ID, 400}, {iam.NewTenantID().String(), 400}} {
		body, err := json.Marshal(notification.Publication{Notifications: []notify.Input{{Recipient: notify.Recipient{Audience: notify.AudienceUser, ID: tc.id}, Origin: notify.Origin{System: "test", Scope: "server", ID: tc.id}, Kind: "test", CreatedAt: time.Now()}}})
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, "/api/contentkit/v1/notifications/publish", bytes.NewReader(body))
		ctx, cancel := context.WithTimeout(req.Context(), 3*time.Second)
		req = req.WithContext(ctx)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		cancel()
		if rec.Code != tc.want {
			t.Fatalf("target membership status=%d want=%d body=%s", rec.Code, tc.want, rec.Body)
		}
	}
	if got := scopedStatus(s, "/notifications/inbox", token); got != http.StatusForbidden {
		t.Fatalf("foreign operator acquired personal inbox: %d", got)
	}
	memberToken := authtest.SignIn(t, bootstrap, member).AccessToken
	req := httptest.NewRequest(http.MethodGet, "/api/contentkit/v1/notifications/inbox", nil)
	req.Header.Set("Authorization", "Bearer "+memberToken)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	var page notify.Page
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil || rec.Code != 200 || len(page.Items) != 1 || page.Items[0].Recipient.ID != member.ID {
		t.Fatalf("target inbox %d %s %v", rec.Code, rec.Body, err)
	}
}
