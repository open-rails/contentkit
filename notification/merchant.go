package notification

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/helpers/notify"
)

// ErrMerchantBinding rejects a source that is not the tenant's verified billing
// association. It never reassigns a tenant or merchant during normal startup.
var ErrMerchantBinding = errors.New("notification: merchant is bound to another tenant or source")

// BindMerchant pins one verified producer/merchant pair to an already registered
// tenant account store. Matching concurrent app startups are idempotent; neither
// another tenant's merchant nor another merchant for this tenant can be attached.
func (s *Store) BindMerchant(ctx context.Context, tenantID string, origin notify.OriginScope) error {
	id, err := uuid.Parse(tenantID)
	if err != nil || id == uuid.Nil || id.String() != tenantID || strings.TrimSpace(origin.System) == "" || strings.TrimSpace(origin.Scope) == "" || len(origin.System) > 512 || len(origin.Scope) > 512 || strings.ContainsRune(origin.System, 0) || strings.ContainsRune(origin.Scope, 0) {
		return fmt.Errorf("%w: tenant and source identity required", notify.ErrInvalid)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, s.schema+"/contentkit/notification-merchants"); err != nil {
		return err
	}
	accounts := pgx.Identifier{s.schema, "content_account_bindings"}.Sanitize()
	var verified bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM `+accounts+` WHERE tenant_id=$1 AND account_namespace=$2)`, tenantID, s.namespace).Scan(&verified); err != nil {
		return err
	}
	if !verified {
		return ErrMerchantBinding
	}
	table := pgx.Identifier{s.schema, "content_notification_merchants"}.Sanitize()
	if _, err := tx.Exec(ctx, `INSERT INTO `+table+` (tenant_id,source_system,merchant_id) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, tenantID, origin.System, origin.Scope); err != nil {
		return err
	}
	var source, merchant string
	err = tx.QueryRow(ctx, `SELECT source_system,merchant_id FROM `+table+` WHERE tenant_id=$1`, tenantID).Scan(&source, &merchant)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrMerchantBinding
	}
	if err != nil {
		return err
	}
	if source != origin.System || merchant != origin.Scope {
		return ErrMerchantBinding
	}
	return tx.Commit(ctx)
}
