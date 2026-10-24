package contentkit

import (
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/helpers/eventlog"
	eventpg "github.com/open-rails/helpers/eventlog/postgres"
	"testing"
	"time"

	"github.com/open-rails/contentkit/internal/pgtest"
)

func TestScopedAvatarPrimaryPlacementIsExplicitAndUnique(t *testing.T) {
	ctx := t.Context()
	pool := pgtest.Pool(t, nil)
	accounts := engineSchema(t, pool)
	owner := NewTenantID()
	cfg := scopeConfig(engineSchema(t, pool), Scope{TenantID: owner, AppID: NewAppID()}, "primary")
	cfg.Media = &MediaConfig{}
	cfg.Accounts = &AccountsConfig{Schema: accounts, Avatars: &AvatarsConfig{}}
	if err := MigratePostgres(ctx, pool, cfg); err != nil {
		t.Fatal(err)
	}
	replica := &Engine{pool: pool, cfg: cfg}
	if err := replica.configureStorage(); err != nil {
		t.Fatal(err)
	}
	if err := replica.avatarPlacement(ctx, false); err != nil {
		t.Fatalf("matching replica refused: %v", err)
	}
	if err := MigratePostgres(ctx, pool, cfg); err != nil {
		t.Fatalf("matching provisioning retry: %v", err)
	}
	other := scopeConfig(engineSchema(t, pool), Scope{TenantID: owner, AppID: NewAppID()}, "other")
	other.Media = &MediaConfig{}
	other.Accounts = &AccountsConfig{Schema: accounts, Avatars: &AvatarsConfig{}}
	if err := MigratePostgres(ctx, pool, other); !errors.Is(err, ErrAvatarPlacement) {
		t.Fatalf("competing primary accepted: %v", err)
	}
	other.Accounts.Avatars = &AvatarsConfig{Primary: "https://primary.test/api/contentkit", Key: "test"}
	if err := MigratePostgres(ctx, pool, other); err != nil {
		t.Fatalf("forwarding secondary refused: %v", err)
	}
	secondary := &Engine{pool: pool, cfg: other}
	if err := secondary.configureStorage(); err != nil {
		t.Fatal(err)
	}
	if err := secondary.avatarPlacement(ctx, false); err != nil {
		t.Fatal(err)
	}
	missing := scopeConfig(engineSchema(t, pool), Scope{TenantID: NewTenantID(), AppID: NewAppID()}, "unprovisioned")
	missing.Media = &MediaConfig{}
	missing.Accounts = &AccountsConfig{Schema: accounts, Avatars: &AvatarsConfig{}}
	unprovisioned := &Engine{pool: pool, cfg: missing}
	if err := unprovisioned.configureStorage(); err != nil {
		t.Fatal(err)
	}
	if err := unprovisioned.avatarPlacement(ctx, false); err == nil {
		t.Fatal("startup silently claimed avatar placement")
	}
}

func TestAccountMediaEventsRemainTenantScoped(t *testing.T) {
	ctx := t.Context()
	pool := pgtest.Pool(t, nil)
	accounts := engineSchema(t, pool)
	owner := NewTenantID()
	makeEngine := func(tenant TenantID, key string) *Engine {
		t.Helper()
		cfg := scopeConfig(engineSchema(t, pool), Scope{TenantID: tenant, AppID: NewAppID()}, key)
		cfg.Accounts = &AccountsConfig{Schema: accounts}
		if err := MigratePostgres(ctx, pool, cfg); err != nil {
			t.Fatal(err)
		}
		e := &Engine{pool: pool, cfg: cfg}
		if err := e.configureStorage(); err != nil {
			t.Fatal(err)
		}
		return e
	}
	first, replica, other := makeEngine(owner, "first"), makeEngine(owner, "replica"), makeEngine(NewTenantID(), "other")
	writer, err := eventpg.New(pool, eventpg.Config{Schema: accounts, Source: "contentkit", Scope: first.accountMediaPartition()})
	if err != nil {
		t.Fatal(err)
	}
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		return writer.AppendTx(ctx, tx, eventlog.Event{ID: "1", Source: "contentkit", Type: media.EventProcessingCompleted, Version: 1, OccurredAt: time.Now(), Subject: eventlog.Subject{Type: "user", ID: "avatar-owner"}, Data: []byte(`{"kind":"user","content_id":"avatar-owner","state":"ready"}`)})
	}); err != nil {
		t.Fatal(err)
	}
	var cursor string
	for i, e := range []*Engine{first, replica, other} {
		reader, err := e.AccountMediaEvents()
		if err != nil {
			t.Fatal(err)
		}
		page, err := reader.ListEvents(ctx, eventlog.Query{})
		if err != nil {
			t.Fatal(err)
		}
		want := 1
		if i == 2 {
			want = 0
		}
		if len(page.Events) != want {
			t.Fatalf("reader %d events=%+v", i, page)
		}
		if i == 0 {
			cursor = page.NextCursor
		}
		if i == 2 {
			if _, err := reader.ListEvents(ctx, eventlog.Query{After: cursor}); !errors.Is(err, eventlog.ErrInvalidCursor) {
				t.Fatalf("foreign cursor accepted: %v", err)
			}
		}
	}
}
