package contentkit

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"strings"

	"github.com/open-rails/contentkit/media/layout"
)

// AccountMediaStorage identifies the tenant account service's durable media
// state and processing queue. Paths and schemas are derived, never supplied by
// an upload request. A secondary forwards writes and owns neither queue.
type AccountMediaStorage struct{ Schema, WorkerSchema string }

// AccountMedia returns the storage the avatar primary's worker must use in
// addition to its app media storage. Legacy engines retain their existing setup.
func (c Config) AccountMedia() (AccountMediaStorage, bool, error) {
	e := Engine{cfg: c}
	if err := e.configureStorage(); err != nil {
		return AccountMediaStorage{}, false, err
	}
	s, ok := e.accountMediaStorage()
	return s, ok, nil
}
func (e *Engine) accountMediaStorage() (AccountMediaStorage, bool) {
	if e.scope == nil || e.cfg.Media == nil || e.cfg.Media.LegacyLayout || e.cfg.Avatars == nil || e.cfg.Avatars.Primary != "" {
		return AccountMediaStorage{}, false
	}
	schema, _ := e.accountStorage()
	return AccountMediaStorage{Schema: schema, WorkerSchema: "ck_account_" + strings.ReplaceAll(e.scope.TenantID.String(), "-", "")}, true
}
func (e *Engine) accountMediaPartition() string {
	return (layout.Scope{TenantID: e.scope.TenantID.String()}).Partition()
}

// ErrAvatarPlacement prevents independent app deployments from becoming
// competing writers to a tenant's one avatar manifest space.
var ErrAvatarPlacement = errors.New("contentkit: tenant avatar primary placement conflicts")

func (e *Engine) avatarPlacement(ctx context.Context, claim bool) error {
	if e.scope == nil || e.cfg.Media == nil || e.cfg.Avatars == nil {
		return nil
	}
	schema, _ := e.accountStorage()
	table := pgx.Identifier{schema, "content_account_avatar_placement"}.Sanitize()
	primary := e.cfg.Avatars.Primary == ""
	if claim && primary {
		if _, err := e.pool.Exec(ctx, "INSERT INTO "+table+" (tenant_id,primary_app_id) VALUES ($1,$2) ON CONFLICT (tenant_id) DO NOTHING", e.scope.TenantID.String(), e.scope.AppID.String()); err != nil {
			return err
		}
	}
	var app string
	err := e.pool.QueryRow(ctx, "SELECT primary_app_id::text FROM "+table+" WHERE tenant_id=$1", e.scope.TenantID.String()).Scan(&app)
	if errors.Is(err, pgx.ErrNoRows) {
		return errors.New("contentkit: tenant avatar placement is missing; explicitly migrate the primary app first")
	}
	if err != nil {
		return err
	}
	if primary && app != e.scope.AppID.String() {
		return ErrAvatarPlacement
	}
	return nil
}
