package contentkit

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/internal/testauth"
	"github.com/open-rails/contentkit/notification"
	"github.com/open-rails/helpers/notify"
	notifypg "github.com/open-rails/helpers/notify/postgres"
)

func TestScopedAccountKeyAdoptionPreservesReceiptsAndErasure(t *testing.T) {
	ctx := t.Context()
	pool := pgtest.Pool(t, nil)
	schema := engineSchema(t, pool)
	accounts := engineSchema(t, pool)
	if err := notification.Migrate(ctx, pool, accounts); err != nil {
		t.Fatal(err)
	}
	old, err := notifypg.New(pool, notifypg.Config{Schema: accounts, Scope: "contentkit:accounts"})
	if err != nil {
		t.Fatal(err)
	}
	user := notify.Recipient{Audience: notify.AudienceUser, ID: "alice"}
	input := notify.Input{Recipient: user, Origin: notify.Origin{System: "contentkit", Scope: tenant, ID: "existing"}, Kind: "example", CreatedAt: time.Now()}
	item, err := old.Publish(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = old.SetRead(ctx, user, item.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err = old.SetDismissed(ctx, user, item.ID, true); err != nil {
		t.Fatal(err)
	}
	gone := notify.Recipient{Audience: notify.AudienceUser, ID: "gone"}
	if err = old.EraseRecipient(ctx, gone); err != nil {
		t.Fatal(err)
	}
	cfg := scopeConfig(schema, Scope{TenantID: NewTenantID(), AppID: NewAppID()}, tenant)
	cfg.Accounts = &AccountsConfig{Schema: accounts}
	cfg.Database.SkipMigrations = true
	if err = MigratePostgres(ctx, pool, cfg, MigrationOptions{LegacyAccountKey: "accounts"}); err != nil {
		t.Fatal(err)
	}
	deps := Deps{Postgres: pool, Auth: testauth.Auth{Authz: testauth.All{}}, Authorization: scopeTestAuthorization{}}
	recovery, err := NewForErasureReplay(ctx, cfg, deps)
	if err != nil {
		t.Fatal(err)
	}
	if report, err := recovery.ReplayAccountErasures(ctx); err != nil || !report.Complete() {
		t.Fatalf("adopted full erasure replay: %+v %v", report, err)
	}
	if report, err := recovery.ReplayErasureFences(ctx); err != nil || !report.Complete() {
		t.Fatalf("adopted erasure replay: %+v %v", report, err)
	}
	_ = recovery.Close(ctx)
	engine, err := New(ctx, cfg, deps)
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close(context.Background())
	if engine.notices.Namespace() != "accounts" {
		t.Fatal("runtime ignored recorded account key")
	}
	kept, err := engine.notices.Get(ctx, user, item.ID)
	if err != nil || kept.ReadAt == nil || kept.DismissedAt == nil {
		t.Fatalf("receipt lost: %+v %v", kept, err)
	}
	input.Recipient = gone
	input.Origin.ID = "after-adoption"
	if _, err = engine.notices.Publish(ctx, input); !errors.Is(err, notify.ErrRecipientErased) {
		t.Fatalf("erasure lost: %v", err)
	}
	if err = MigratePostgres(ctx, pool, cfg, MigrationOptions{LegacyAccountKey: "other"}); !errors.Is(err, ErrScopeConflict) {
		t.Fatalf("binding reassigned: %v", err)
	}
}
