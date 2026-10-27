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
	query, args, err := e.accountErasureSubjectsQuery(ctx, false)
	if err != nil {
		return err
	}
	if e.scope == nil {
		return nil
	}
	if err := e.checkAccountErasureCompletion(ctx); err != nil {
		return err
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

// ReplayErasureFences reapplies canonical shared account/app tombstones to
// this app after a content-only restore. The restored app can have lost its
// local fences and personal-data deletions; the shared account store must not
// be restored with it. Run before serving and retry until the report completes.
// Purged accounts resume anonymization, including this app's avatar cleanup.
func (e *Engine) ReplayErasureFences(ctx context.Context) (signal.ErasureReport, error) {
	report := signal.ErasureReport{Remaining: map[string]uint64{}}
	if e.erasureRecovery && e.signals != nil {
		if conn, _ := e.signals.state(); conn == nil {
			e.signals.attempt(ctx)
		}
	}
	for after := ""; ; {
		recipients, err := e.erasedAccountRecipients(ctx, after, min(1000, signal.MaxErasureSubjects))
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
		var subjects []signal.Subject
		for _, recipient := range recipients {
			if recipient.Anonymize {
				if recipient.EraseApp {
					pass, err := e.EraseAppSubjects(ctx, []signal.Subject{{UserID: recipient.ID}})
					for plane, n := range pass.Remaining {
						report.Remaining[plane] += n
					}
					report.PairsRemoved += pass.PairsRemoved
					if err != nil || !pass.Complete() {
						report.Remaining["account_plane"]++
						return report, errors.Join(err, ctx.Err())
					}
				}
				if err := e.AnonymizeAccount(ctx, recipient.ID); err != nil {
					report.Remaining["account_plane"]++
					return report, err
				}
			} else {
				subjects = append(subjects, signal.Subject{UserID: recipient.ID})
			}
		}
		if len(subjects) == 0 {
			after = recipients[len(recipients)-1].ID
			continue
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
// Explicit erasure still requires the avatar primary's completion receipt.
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
	query, args, err := e.accountErasureSubjectsQuery(ctx, true)
	if err != nil {
		return report, err
	}
	anonymizations, err := e.accountAnonymizationQuery(ctx)
	if err != nil {
		return report, err
	}
	schema, _ := e.accountStorage()
	for after := ""; ; {
		rows, err := e.pool.Query(ctx, `WITH anonymizations AS (`+anonymizations+`), subjects AS (`+query+` UNION SELECT actor_id FROM anonymizations)
SELECT r.actor_id, EXISTS(SELECT 1 FROM anonymizations a WHERE a.actor_id=r.actor_id),
EXISTS(SELECT 1 FROM `+pgx.Identifier{schema, "content_account_erasure_intents"}.Sanitize()+` i WHERE i.tenant_id=$1 AND i.app_id=$2::text AND i.actor_id=r.actor_id)
AND NOT EXISTS(SELECT 1 FROM `+pgx.Identifier{schema, "content_account_erasure_receipts"}.Sanitize()+` c WHERE c.tenant_id=$1 AND c.app_id=$2::uuid AND c.actor_id=r.actor_id) FROM subjects r
WHERE r.actor_id>$5 AND ((EXISTS(SELECT 1 FROM anonymizations a WHERE a.actor_id=r.actor_id)
AND NOT EXISTS(SELECT 1 FROM `+pgx.Identifier{e.schema, "content_account_lifecycle"}.Sanitize()+` a WHERE a.tenant_id=$4 AND a.actor_id=r.actor_id AND a.state='purged'))
OR NOT EXISTS(SELECT 1 FROM `+pgx.Identifier{schema, "content_account_erasure_receipts"}.Sanitize()+` c WHERE c.tenant_id=$1 AND c.app_id=$2::uuid AND c.actor_id=r.actor_id))
ORDER BY r.actor_id LIMIT $6`, append(args, after, min(1000, signal.MaxErasureSubjects))...)
		if err != nil {
			return report, err
		}
		var recipients []erasedAccountRecipient
		for rows.Next() {
			var recipient erasedAccountRecipient
			if err := rows.Scan(&recipient.ID, &recipient.Anonymize, &recipient.EraseApp); err != nil {
				rows.Close()
				return report, err
			}
			recipients = append(recipients, recipient)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return report, err
		}
		if len(recipients) == 0 {
			if err := e.checkAccountErasureCompletion(ctx); err != nil {
				report.Remaining["account_plane"] = 1
				return report, err
			}
			return report, nil
		}
		var subjects []signal.Subject
		for _, recipient := range recipients {
			if recipient.Anonymize {
				if recipient.EraseApp {
					pass, err := e.EraseAppSubjects(ctx, []signal.Subject{{UserID: recipient.ID}})
					for plane, n := range pass.Remaining {
						report.Remaining[plane] += n
					}
					report.PairsRemoved += pass.PairsRemoved
					if err != nil || !pass.Complete() {
						report.Remaining["account_plane"]++
						return report, errors.Join(err, ctx.Err())
					}
				}
				if err := e.AnonymizeAccount(ctx, recipient.ID); err != nil {
					report.Remaining["account_plane"]++
					return report, err
				}
			} else {
				subjects = append(subjects, signal.Subject{UserID: recipient.ID})
			}
		}
		if len(subjects) == 0 {
			after = recipients[len(recipients)-1].ID
			continue
		}
		pass, err := e.EraseSubjects(ctx, subjects)
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

// Explicit erasure requires both this app's and the avatar primary's receipt.
// AuthKit purges are completed independently by each host's event reader.
func (e *Engine) checkAccountErasureCompletion(ctx context.Context) error {
	query, args, err := e.accountErasureSubjectsQuery(ctx, true)
	if err != nil {
		return err
	}
	if e.scope == nil {
		return nil
	}
	schema, _ := e.accountStorage()
	var missing string
	err = e.pool.QueryRow(ctx, `SELECT required.app_id::text FROM (`+query+`) r
CROSS JOIN (SELECT $2::uuid AS app_id UNION SELECT primary_app_id FROM `+pgx.Identifier{schema, "content_account_avatar_placement"}.Sanitize()+` WHERE tenant_id=$1) required
WHERE EXISTS(SELECT 1 FROM `+pgx.Identifier{schema, "content_account_erasure_intents"}.Sanitize()+` i WHERE i.tenant_id=$1 AND i.app_id='' AND i.actor_id=r.actor_id)
AND NOT EXISTS(SELECT 1 FROM `+pgx.Identifier{schema, "content_account_erasure_receipts"}.Sanitize()+` c WHERE c.tenant_id=$1 AND c.app_id=required.app_id AND c.actor_id=r.actor_id)
LIMIT 1`, args...).Scan(&missing)
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
