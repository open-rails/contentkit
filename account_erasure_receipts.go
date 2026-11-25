package contentkit

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ContentKit's erasure records live in the account schema, which a catalog
// restore leaves alone. AuthKit's purge erases the account inbox itself, in
// the transaction that deletes the account, before user.purged.

// recordAccountErasureIntent survives content-only restores. Completion never
// removes these intents.
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

// accountErasureSubjectsQuery is this app's durable erasure intents. Its
// arguments are $1 tenant, $2 app and $3 catalog key.
func (e *Engine) accountErasureSubjectsQuery() (string, []any) {
	schema, _ := e.accountStorage()
	return `SELECT DISTINCT actor_id FROM ` + pgx.Identifier{schema, "content_account_erasure_intents"}.Sanitize() + ` WHERE tenant_id=$1::uuid AND app_id=$2::text`,
		[]any{e.scope.TenantID.String(), e.scope.AppID.String(), e.cfg.Scope.AppID.String()}
}

// accountAnonymizationQuery is a local receipt or an interrupted
// anonymization, either of which identifies a purge.
func (e *Engine) accountAnonymizationQuery() string {
	schema, _ := e.accountStorage()
	return `SELECT actor_id FROM ` + pgx.Identifier{schema, "content_account_erasure_receipts"}.Sanitize() + ` WHERE tenant_id=$1::uuid AND app_id=$2::uuid
UNION SELECT actor_id FROM ` + pgx.Identifier{e.schema, "content_account_lifecycle"}.Sanitize() + ` WHERE tenant_id=$3 AND state='purging'`
}

func (e *Engine) checkAccountAnonymization(ctx context.Context) error {
	schema, _ := e.accountStorage()
	var pending bool
	err := e.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM `+pgx.Identifier{schema, "content_account_erasure_intents"}.Sanitize()+` i
WHERE i.tenant_id=$1::uuid AND i.app_id=$2::text AND i.requested_at BETWEEN now()-interval '30 days' AND now()-interval '15 minutes'
AND EXISTS(SELECT 1 FROM `+pgx.Identifier{e.schema, "content_account_lifecycle"}.Sanitize()+` a WHERE a.tenant_id=$3 AND a.actor_id=i.actor_id AND a.state='purging')
AND NOT EXISTS(SELECT 1 FROM `+pgx.Identifier{schema, "content_account_erasure_receipts"}.Sanitize()+` c WHERE c.tenant_id=$1 AND c.app_id=$2::uuid AND c.actor_id=i.actor_id))`,
		e.scope.TenantID.String(), e.scope.AppID.String(), e.cfg.Scope.AppID.String()).Scan(&pending)
	if err != nil {
		return fmt.Errorf("contentkit: check account anonymization: %w", err)
	}
	if pending {
		return fmt.Errorf("contentkit: account anonymization pending for over 15 minutes; check the host's user.purged event reader")
	}
	return nil
}

func (e *Engine) erasedAccountRecipients(ctx context.Context, after string, limit int) ([]string, error) {
	query, args := e.accountErasureSubjectsQuery()
	rows, err := e.pool.Query(ctx, `WITH subjects AS (`+query+` UNION `+e.accountAnonymizationQuery()+`)
SELECT actor_id FROM subjects WHERE actor_id>$4 ORDER BY actor_id LIMIT $5`, append(args, after, limit)...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}
