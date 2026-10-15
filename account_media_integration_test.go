package contentkit

import (
	"errors"
	"testing"

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
