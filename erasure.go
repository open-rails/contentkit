package contentkit

import (
	"context"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/signal"
)

var ErrErasureReplayRequired = errors.New("contentkit: erasure replay required; run tools replay-content-erasures before serving restored content")

// checkErasureReplay is a read-only startup barrier. Missing local fences can
// mean a pre-erasure catalog restore or a still-pending normal deletion. Both
// fail closed; the operator can retry or explicitly finish erasure replay.
func (e *Engine) checkErasureReplay(ctx context.Context) error {
	if e.scope == nil {
		return nil
	}
	if err := e.checkAccountErasureCompletion(ctx); err != nil {
		return err
	}
	schema, namespace := e.accountStorage()
	var pending bool
	err := e.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM `+pgx.Identifier{schema, "notify_recipients"}.Sanitize()+` r
WHERE r.scope=$1 AND r.audience='user' AND (r.erased_at IS NOT NULL OR EXISTS(SELECT 1 FROM `+pgx.Identifier{schema, "notify_app_erasures"}.Sanitize()+` x
WHERE x.scope=r.scope AND x.audience=r.audience AND x.recipient=r.recipient AND ((x.app_id<>'' AND x.app_id=$2) OR (x.source='contentkit' AND x.source_scope=$3))))
AND NOT EXISTS(SELECT 1 FROM `+pgx.Identifier{e.schema, "content_erased_subjects"}.Sanitize()+` f WHERE f.tenant_id=$3 AND f.actor_id=r.recipient AND f.engine_cleanup_complete))`, "contentkit:"+namespace, e.scope.AppID.String(), e.cfg.Tenant).Scan(&pending)
	if err != nil {
		return fmt.Errorf("contentkit: check erasure replay: %w", err)
	}
	if pending {
		return ErrErasureReplayRequired
	}
	return nil
}

// ReplayErasureFences reapplies canonical shared account/app tombstones to
// this app after a content-only restore. The restored app can have lost its
// local fences and personal-data deletions; the shared account store must not
// be restored with it. Run before serving and retry until the report completes.
// Shared account avatars, receipts and identity records are not modified.
func (e *Engine) ReplayErasureFences(ctx context.Context) (signal.ErasureReport, error) {
	report := signal.ErasureReport{Remaining: map[string]uint64{}}
	if e.erasureRecovery && e.signals != nil {
		if conn, _ := e.signals.state(); conn == nil {
			e.signals.attempt(ctx)
		}
	}
	for after := ""; ; {
		recipients, err := e.notices.ErasedRecipients(ctx, after, min(1000, signal.MaxErasureSubjects))
		if err != nil {
			return report, err
		}
		if len(recipients) == 0 {
			if err := e.checkAccountErasureCompletion(ctx); err != nil {
				report.Remaining["account_plane"] = 1
				return report, err
			}
			return report, nil
		}
		subjects := make([]signal.Subject, len(recipients))
		for i, recipient := range recipients {
			subjects[i] = signal.Subject{UserID: recipient.ID}
		}
		pass, err := e.EraseAppSubjects(ctx, subjects)
		for plane, n := range pass.Remaining {
			report.Remaining[plane] += n
		}
		report.PairsRemoved += pass.PairsRemoved
		if err != nil || !pass.Complete() {
			return report, errors.Join(err, ctx.Err())
		}
		after = recipients[len(recipients)-1].ID
	}
}

// ReplayAccountErasures explicitly retries this app's unfinished full account
// cleanup, including its avatar responsibility. It is for the identity purge
// worker or a privileged recovery command, never an automatic catalog restore.
// The avatar primary must finish before a secondary can declare recovery done.
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
	schema, namespace := e.accountStorage()
	for after := ""; ; {
		rows, err := e.pool.Query(ctx, `SELECT r.recipient FROM `+pgx.Identifier{schema, "notify_recipients"}.Sanitize()+` r
WHERE r.scope=$1 AND r.audience='user' AND r.erased_at IS NOT NULL AND r.recipient>$2
AND NOT EXISTS(SELECT 1 FROM `+pgx.Identifier{schema, "content_account_erasure_receipts"}.Sanitize()+` c WHERE c.tenant_id=$3 AND c.app_id=$4 AND c.actor_id=r.recipient)
ORDER BY r.recipient LIMIT $5`, "contentkit:"+namespace, after, e.scope.TenantID.String(), e.scope.AppID.String(), min(1000, signal.MaxErasureSubjects))
		if err != nil {
			return report, err
		}
		var subjects []signal.Subject
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return report, err
			}
			subjects = append(subjects, signal.Subject{UserID: id})
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return report, err
		}
		if len(subjects) == 0 {
			if err := e.checkAccountErasureCompletion(ctx); err != nil {
				report.Remaining["account_plane"] = 1
				return report, err
			}
			return report, nil
		}
		pass, err := e.EraseSubjects(ctx, subjects)
		for plane, n := range pass.Remaining {
			report.Remaining[plane] += n
		}
		report.PairsRemoved += pass.PairsRemoved
		if err != nil || !pass.Complete() {
			return report, errors.Join(err, ctx.Err())
		}
		after = subjects[len(subjects)-1].UserID
	}
}

// A catalog-local completion cannot prove shared account cleanup finished.
// Both this app and the configured avatar primary have independent durable
// obligations; the primary's receipt gates every app exposing shared avatars.
func (e *Engine) checkAccountErasureCompletion(ctx context.Context) error {
	if e.scope == nil {
		return nil
	}
	schema, namespace := e.accountStorage()
	var missing string
	err := e.pool.QueryRow(ctx, `SELECT required.app_id::text FROM `+pgx.Identifier{schema, "notify_recipients"}.Sanitize()+` r
CROSS JOIN (SELECT $3::uuid AS app_id UNION SELECT primary_app_id FROM `+pgx.Identifier{schema, "content_account_avatar_placement"}.Sanitize()+` WHERE tenant_id=$2) required
WHERE r.scope=$1 AND r.audience='user' AND r.erased_at IS NOT NULL
AND NOT EXISTS(SELECT 1 FROM `+pgx.Identifier{schema, "content_account_erasure_receipts"}.Sanitize()+` c WHERE c.tenant_id=$2 AND c.app_id=required.app_id AND c.actor_id=r.recipient)
LIMIT 1`, "contentkit:"+namespace, e.scope.TenantID.String(), e.scope.AppID.String()).Scan(&missing)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("contentkit: check account erasure completion: %w", err)
	}
	return fmt.Errorf("%w; unfinished account cleanup for app %s: run that app's tools replay-content-erasures --complete-accounts", ErrErasureReplayRequired, missing)
}

func (e *Engine) completeErasure(ctx context.Context, actors []string, account bool) error {
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `UPDATE `+pgx.Identifier{e.schema, "content_erased_subjects"}.Sanitize()+` SET engine_cleanup_complete=true WHERE tenant_id=$1 AND actor_id=ANY($2)`, e.cfg.Tenant, actors); err != nil {
		return err
	}
	if account && e.scope != nil {
		schema, _ := e.accountStorage()
		if _, err = tx.Exec(ctx, `INSERT INTO `+pgx.Identifier{schema, "content_account_erasure_receipts"}.Sanitize()+` (tenant_id,app_id,actor_id)
SELECT $1,$2,unnest($3::text[]) ON CONFLICT (tenant_id,app_id,actor_id) DO NOTHING`, e.scope.TenantID.String(), e.scope.AppID.String(), actors); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
