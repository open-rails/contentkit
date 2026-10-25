package contentkit

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/authkit/notify"
	"github.com/open-rails/authkit/notify/postgres"
)

// Account cleanup consumes the canonical AuthKit fences independently of the
// optional publication channel. It never creates inbox storage or exposes an
// inbox reader/writer through ContentKit's runtime API.
func (e *Engine) accountErasureAvailable(ctx context.Context) (bool, error) {
	schema, _ := e.accountStorage()
	var present int
	err := e.pool.QueryRow(ctx, `SELECT (to_regclass($1) IS NOT NULL)::int + (to_regclass($2) IS NOT NULL)::int + (to_regclass($3) IS NOT NULL)::int + (to_regclass($4) IS NOT NULL)::int`,
		pgx.Identifier{schema, "notify_recipients"}.Sanitize(),
		pgx.Identifier{schema, "notify_app_erasures"}.Sanitize(),
		pgx.Identifier{schema, "notify_items"}.Sanitize(),
		pgx.Identifier{schema, "notify_migrations"}.Sanitize()).Scan(&present)
	if err != nil {
		return false, err
	}
	if present == 0 {
		return false, nil // A fresh account directory may have no optional inbox.
	}
	if present != 4 {
		return false, fmt.Errorf("contentkit: incomplete AuthKit account erasure storage")
	}
	return true, postgres.Validate(ctx, e.pool, schema)
}

func (e *Engine) accountErasureStore(ctx context.Context) (*postgres.Store, error) {
	if available, err := e.accountErasureAvailable(ctx); err != nil || !available {
		return nil, err
	}
	schema, namespace := e.accountStorage()
	return postgres.New(e.pool, postgres.Config{Schema: schema, Scope: "contentkit:" + namespace})
}

func (e *Engine) eraseAccountReceipts(ctx context.Context, ids []string, account bool) error {
	store, err := e.accountErasureStore(ctx)
	if err != nil || store == nil {
		return err
	}
	appID := ""
	if e.scope != nil {
		appID = e.scope.AppID.String()
	}
	for _, id := range ids {
		recipient := notify.Recipient{Audience: notify.AudienceUser, ID: id}
		if account {
			err = store.EraseRecipient(ctx, recipient)
		} else {
			err = store.EraseAppRecipient(ctx, recipient, appID, notify.OriginScope{System: "contentkit", Scope: e.cfg.Tenant})
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) erasedAccountRecipients(ctx context.Context, after string, limit int) ([]notify.Recipient, error) {
	store, err := e.accountErasureStore(ctx)
	if err != nil || store == nil {
		return nil, err
	}
	appID := ""
	if e.scope != nil {
		appID = e.scope.AppID.String()
	}
	return store.AppErasedRecipients(ctx, notify.AudienceUser, appID, notify.OriginScope{System: "contentkit", Scope: e.cfg.Tenant}, after, limit)
}
