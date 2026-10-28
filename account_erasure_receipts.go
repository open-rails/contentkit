package contentkit

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
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

func (e *Engine) eraseAccountReceipts(ctx context.Context, ids []string) error {
	if err := e.recordAccountErasureIntent(ctx, e.pool, ids); err != nil {
		return err
	}
	store, err := e.accountErasureStore(ctx)
	if err != nil || store == nil {
		return err
	}
	appID := e.scope.AppID.String()
	for _, id := range ids {
		recipient := notify.Recipient{Audience: notify.AudienceUser, ID: id}
		if err := store.EraseAppRecipient(ctx, recipient, appID, notify.OriginScope{}); err != nil {
			return err
		}
	}
	return nil
}

// recordAccountErasureIntent survives content-only restores, even when the
// optional inbox has never existed. Completion never removes these intents.
func (e *Engine) recordAccountErasureIntent(ctx context.Context, db interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	schema, _ := e.accountStorage()
	_, err := db.Exec(ctx, `INSERT INTO `+pgx.Identifier{schema, "content_account_erasure_intents"}.Sanitize()+` (tenant_id,app_id,actor_id)
SELECT $1,$2,unnest($3::text[]) ON CONFLICT (tenant_id,app_id,actor_id) DO NOTHING`, e.scope.TenantID.String(), e.scope.AppID.String(), ids)
	return err
}

// accountErasureSubjectsQuery combines ContentKit's durable intents with any
// existing AuthKit fences. An absent inbox is optional; partial lineage is not.
func (e *Engine) accountErasureSubjectsQuery(ctx context.Context) (string, []any, error) {
	available, err := e.accountErasureAvailable(ctx)
	if err != nil {
		return "", nil, err
	}
	schema, namespace := e.accountStorage()
	tenantID, appID := e.scope.TenantID.String(), e.scope.AppID.String()
	query := `SELECT DISTINCT i.actor_id FROM ` + pgx.Identifier{schema, "content_account_erasure_intents"}.Sanitize() + ` i CROSS JOIN erasure_scope s
WHERE i.tenant_id=s.tenant_id AND i.app_id=s.app_id`
	if available {
		query += ` UNION SELECT r.recipient AS actor_id FROM ` + pgx.Identifier{schema, "notify_recipients"}.Sanitize() + ` r CROSS JOIN erasure_scope s
WHERE r.scope=s.notify_scope AND r.audience='user' AND (r.erased_at IS NOT NULL OR EXISTS(SELECT 1 FROM ` + pgx.Identifier{schema, "notify_app_erasures"}.Sanitize() + ` x
WHERE x.scope=r.scope AND x.audience=r.audience AND x.recipient=r.recipient AND x.app_id=s.app_id))`
	}
	return `WITH erasure_scope AS (SELECT $1::uuid AS tenant_id, $2::text AS app_id, $3::text AS notify_scope, $4::text AS catalog_key) ` + query,
		[]any{tenantID, appID, "contentkit:" + namespace, e.cfg.Scope.AppID.String()}, nil
}

// Without an inbox, a local receipt or an interrupted anonymization still
// identifies a purge.
func (e *Engine) accountAnonymizationQuery(ctx context.Context) (string, error) {
	available, err := e.accountErasureAvailable(ctx)
	if err != nil {
		return "", err
	}
	schema, _ := e.accountStorage()
	query := `SELECT actor_id FROM ` + pgx.Identifier{schema, "content_account_erasure_receipts"}.Sanitize() + ` WHERE tenant_id=$1 AND app_id=$2::uuid
UNION SELECT actor_id FROM ` + pgx.Identifier{e.schema, "content_account_lifecycle"}.Sanitize() + ` WHERE tenant_id=$4 AND state='purging'`
	if available {
		query += ` UNION SELECT recipient AS actor_id FROM ` + pgx.Identifier{schema, "notify_recipients"}.Sanitize() + ` WHERE scope=$3 AND audience='user' AND erased_at IS NOT NULL`
	}
	return query, nil
}

func (e *Engine) checkAccountAnonymization(ctx context.Context) error {
	available, err := e.accountErasureAvailable(ctx)
	if err != nil {
		return err
	}
	schema, namespace := e.accountStorage()
	query := `SELECT i.actor_id FROM ` + pgx.Identifier{schema, "content_account_erasure_intents"}.Sanitize() + ` i
WHERE i.tenant_id=$1 AND i.app_id=$2 AND i.requested_at BETWEEN now()-interval '30 days' AND now()-interval '15 minutes'
AND EXISTS(SELECT 1 FROM ` + pgx.Identifier{e.schema, "content_account_lifecycle"}.Sanitize() + ` a WHERE a.tenant_id=$4 AND a.actor_id=i.actor_id AND a.state='purging')`
	if available {
		query += ` UNION ALL SELECT recipient AS actor_id FROM ` + pgx.Identifier{schema, "notify_recipients"}.Sanitize() + ` WHERE scope=$3 AND audience='user' AND erased_at BETWEEN now()-interval '30 days' AND now()-interval '15 minutes'`
	}
	var pending bool
	err = e.pool.QueryRow(ctx, `WITH erasure_scope AS (SELECT $1::uuid, $2::text, $3::text, $4::text)
SELECT EXISTS(SELECT 1 FROM (`+query+`) r
WHERE NOT EXISTS(SELECT 1 FROM `+pgx.Identifier{schema, "content_account_erasure_receipts"}.Sanitize()+` c WHERE c.tenant_id=$1 AND c.app_id=$2::uuid AND c.actor_id=r.actor_id) LIMIT 1)`,
		e.scope.TenantID.String(), e.scope.AppID.String(), "contentkit:"+namespace, e.cfg.Scope.AppID.String()).Scan(&pending)
	if err != nil {
		return fmt.Errorf("contentkit: check account anonymization: %w", err)
	}
	if pending {
		return fmt.Errorf("contentkit: account anonymization pending for over 15 minutes; check the host's user.purged event reader")
	}
	return nil
}

func (e *Engine) erasedAccountRecipients(ctx context.Context, after string, limit int) ([]string, error) {
	query, args, err := e.accountErasureSubjectsQuery(ctx)
	if err != nil {
		return nil, err
	}
	anonymizations, err := e.accountAnonymizationQuery(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := e.pool.Query(ctx, `WITH subjects AS (`+query+` UNION `+anonymizations+`)
SELECT actor_id FROM subjects WHERE actor_id>$5 ORDER BY actor_id LIMIT $6`, append(args, after, limit)...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}
