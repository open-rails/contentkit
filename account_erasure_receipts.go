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
	if err := e.recordAccountErasureIntent(ctx, ids, account); err != nil {
		return err
	}
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

// recordAccountErasureIntent survives content-only restores, even when the
// optional inbox has never existed. Completion never removes these intents.
func (e *Engine) recordAccountErasureIntent(ctx context.Context, ids []string, account bool) error {
	if e.scope == nil || len(ids) == 0 {
		return nil
	}
	schema, _ := e.accountStorage()
	appID := e.scope.AppID.String()
	if account {
		appID = ""
	}
	_, err := e.pool.Exec(ctx, `INSERT INTO `+pgx.Identifier{schema, "content_account_erasure_intents"}.Sanitize()+` (tenant_id,app_id,actor_id)
SELECT $1,$2,unnest($3::text[]) ON CONFLICT (tenant_id,app_id,actor_id) DO NOTHING`, e.scope.TenantID.String(), appID, ids)
	return err
}

// accountErasureSubjectsQuery combines ContentKit's durable intents with any
// existing AuthKit fences. An absent inbox is optional; partial lineage is not.
// The typed scope also keeps the same query parameters for legacy runtimes.
func (e *Engine) accountErasureSubjectsQuery(ctx context.Context, account bool) (string, []any, error) {
	available, err := e.accountErasureAvailable(ctx)
	if err != nil {
		return "", nil, err
	}
	schema, namespace := e.accountStorage()
	var tenantID any
	appID := ""
	query := `SELECT NULL::text AS actor_id WHERE false`
	if e.scope != nil {
		tenantID, appID = e.scope.TenantID.String(), e.scope.AppID.String()
		predicate := `i.app_id=''`
		if !account {
			predicate = `(` + predicate + ` OR i.app_id=s.app_id)`
		}
		query = `SELECT DISTINCT i.actor_id FROM ` + pgx.Identifier{schema, "content_account_erasure_intents"}.Sanitize() + ` i CROSS JOIN erasure_scope s
WHERE i.tenant_id=s.tenant_id AND ` + predicate
	}
	if available {
		predicate := `r.erased_at IS NOT NULL`
		if !account {
			predicate = `(` + predicate + ` OR EXISTS(SELECT 1 FROM ` + pgx.Identifier{schema, "notify_app_erasures"}.Sanitize() + ` x
WHERE x.scope=r.scope AND x.audience=r.audience AND x.recipient=r.recipient AND ((x.app_id<>'' AND x.app_id=s.app_id) OR (x.source='contentkit' AND x.source_scope=s.catalog_key))))`
		}
		query += ` UNION SELECT r.recipient AS actor_id FROM ` + pgx.Identifier{schema, "notify_recipients"}.Sanitize() + ` r CROSS JOIN erasure_scope s
WHERE r.scope=s.notify_scope AND r.audience='user' AND ` + predicate
	}
	return `WITH erasure_scope AS (SELECT $1::uuid AS tenant_id, $2::text AS app_id, $3::text AS notify_scope, $4::text AS catalog_key) ` + query,
		[]any{tenantID, appID, "contentkit:" + namespace, e.cfg.Tenant}, nil
}

func (e *Engine) erasedAccountRecipients(ctx context.Context, after string, limit int) ([]notify.Recipient, error) {
	query, args, err := e.accountErasureSubjectsQuery(ctx, false)
	if err != nil {
		return nil, err
	}
	rows, err := e.pool.Query(ctx, `SELECT actor_id FROM (`+query+`) r WHERE actor_id>$5 ORDER BY actor_id LIMIT $6`, append(args, after, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var recipients []notify.Recipient
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		recipients = append(recipients, notify.Recipient{Audience: notify.AudienceUser, ID: id})
	}
	return recipients, rows.Err()
}
