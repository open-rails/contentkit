package contentkit

import (
	"context"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/signal"
)

var ErrErasureReplayRequired = errors.New("contentkit: erasure replay required; run tools replay-content-erasures before serving restored content")

// Pending purges need a running event reader. A receipt without local
// completion proves a catalog restore, since both commit atomically.
func (e *Engine) checkErasureReplay(ctx context.Context) error {
	query, args, err := e.accountErasureSubjectsQuery(ctx)
	if err != nil {
		return err
	}
	if e.scope == nil {
		return nil
	}
	anonymizations, err := e.accountAnonymizationQuery(ctx)
	if err != nil {
		return err
	}
	schema, _ := e.accountStorage()
	var pending bool
	err = e.pool.QueryRow(ctx, `WITH anonymizations AS (`+anonymizations+`), subjects AS (`+query+` UNION SELECT actor_id FROM anonymizations)
SELECT EXISTS(SELECT 1 FROM subjects r
WHERE NOT EXISTS(SELECT 1 FROM `+pgx.Identifier{e.schema, "content_erased_subjects"}.Sanitize()+` f WHERE f.tenant_id=$4 AND f.actor_id=r.actor_id AND f.engine_cleanup_complete)
AND ((EXISTS(SELECT 1 FROM `+pgx.Identifier{schema, "content_account_erasure_intents"}.Sanitize()+` i WHERE i.tenant_id=$1 AND i.app_id=$2::text AND i.actor_id=r.actor_id)
AND NOT EXISTS(SELECT 1 FROM `+pgx.Identifier{e.schema, "content_account_lifecycle"}.Sanitize()+` a WHERE a.tenant_id=$4 AND a.actor_id=r.actor_id AND a.state IN ('purging','purged')))
OR (EXISTS(SELECT 1 FROM anonymizations a WHERE a.actor_id=r.actor_id)
AND EXISTS(SELECT 1 FROM `+pgx.Identifier{schema, "content_account_erasure_receipts"}.Sanitize()+` c WHERE c.tenant_id=$1 AND c.app_id=$2::uuid AND c.actor_id=r.actor_id)
AND NOT EXISTS(SELECT 1 FROM `+pgx.Identifier{e.schema, "content_account_lifecycle"}.Sanitize()+` a WHERE a.tenant_id=$4 AND a.actor_id=r.actor_id AND a.state='purged'))
OR (NOT EXISTS(SELECT 1 FROM anonymizations a WHERE a.actor_id=r.actor_id)
AND NOT EXISTS(SELECT 1 FROM `+pgx.Identifier{e.schema, "content_account_lifecycle"}.Sanitize()+` a WHERE a.tenant_id=$4 AND a.actor_id=r.actor_id AND a.state IN ('purging','purged')))))`, args...).Scan(&pending)
	if err != nil {
		return fmt.Errorf("contentkit: check erasure replay: %w", err)
	}
	if pending {
		return ErrErasureReplayRequired
	}
	return nil
}

// ReplayErasureFences repeats this app's recorded account deletions after a
// content-only restore. The restored app can have lost its local fences and
// anonymization; the shared account store must not be restored with it. Run
// before serving and retry until the report completes.
func (e *Engine) ReplayErasureFences(ctx context.Context) (signal.ErasureReport, error) {
	report := signal.ErasureReport{Remaining: map[string]uint64{}}
	if e.erasureRecovery && e.signals != nil {
		if conn, _ := e.signals.state(); conn == nil {
			e.signals.attempt(ctx)
		}
	}
	for after := ""; ; {
		actors, err := e.erasedAccountRecipients(ctx, after, min(1000, signal.MaxErasureSubjects))
		if err != nil || len(actors) == 0 {
			return report, err
		}
		if err := e.replayAnonymize(ctx, actors, report); err != nil {
			return report, err
		}
		after = actors[len(actors)-1]
	}
}

// ReplayAccountErasures finishes this app's purges that never completed,
// including its avatar responsibility. It is for the identity purge worker or
// a privileged recovery command, never an automatic catalog restore.
func (e *Engine) ReplayAccountErasures(ctx context.Context) (signal.ErasureReport, error) {
	report := signal.ErasureReport{Remaining: map[string]uint64{}}
	if e.scope == nil {
		return report, errors.New("contentkit: account erasure replay requires Scope")
	}
	if e.erasureRecovery && e.signals != nil {
		if conn, _ := e.signals.state(); conn == nil {
			e.signals.attempt(ctx)
		}
	}
	anonymizations, err := e.accountAnonymizationQuery(ctx)
	if err != nil {
		return report, err
	}
	_, args, err := e.accountErasureSubjectsQuery(ctx)
	if err != nil {
		return report, err
	}
	for after := ""; ; {
		rows, err := e.pool.Query(ctx, `WITH erasure_scope AS (SELECT $1::uuid, $2::text, $3::text, $4::text)
SELECT r.actor_id FROM (`+anonymizations+`) r
WHERE r.actor_id>$5 AND NOT EXISTS(SELECT 1 FROM `+pgx.Identifier{e.schema, "content_account_lifecycle"}.Sanitize()+` a WHERE a.tenant_id=$4 AND a.actor_id=r.actor_id AND a.state='purged')
ORDER BY r.actor_id LIMIT $6`, append(args, after, min(1000, signal.MaxErasureSubjects))...)
		if err != nil {
			return report, err
		}
		actors, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil || len(actors) == 0 {
			return report, err
		}
		if err := e.replayAnonymize(ctx, actors, report); err != nil {
			return report, err
		}
		after = actors[len(actors)-1]
	}
}

// replayAnonymize repeats the live deletion, so a restored catalog ends
// exactly as live deletion left it.
func (e *Engine) replayAnonymize(ctx context.Context, actors []string, report signal.ErasureReport) error {
	for _, id := range actors {
		if err := e.AnonymizeAccount(ctx, id); err != nil {
			report.Remaining["account_plane"]++
			return err
		}
	}
	return nil
}

func (e *Engine) completeErasure(ctx context.Context, actors []string) error {
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `UPDATE `+pgx.Identifier{e.schema, "content_erased_subjects"}.Sanitize()+` SET engine_cleanup_complete=true WHERE tenant_id=$1 AND actor_id=ANY($2)`, e.cfg.Tenant, actors); err != nil {
		return err
	}
	if e.scope != nil {
		schema, _ := e.accountStorage()
		if _, err = tx.Exec(ctx, `INSERT INTO `+pgx.Identifier{schema, "content_account_erasure_receipts"}.Sanitize()+` (tenant_id,app_id,actor_id)
SELECT $1,$2,unnest($3::text[]) ON CONFLICT (tenant_id,app_id,actor_id) DO NOTHING`, e.scope.TenantID.String(), e.scope.AppID.String(), actors); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
