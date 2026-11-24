package contentkit

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/internal/hub"
	mediastore "github.com/open-rails/contentkit/internal/mediastore"
	"github.com/open-rails/contentkit/internal/subjectfence"
	"github.com/open-rails/contentkit/signal"
)

// SetAccountDeleted hides account attribution during AuthKit's recovery
// period. Restoring the account clears the marker and restores attribution;
// comments, favorites, votes and reading data are untouched.
func (e *Engine) SetAccountDeleted(ctx context.Context, accountID string, deleted bool) error {
	return e.content.SetAccountDeleted(ctx, accountID, deleted)
}

// SetAccountHidden hides an account's comments during a ban. Unhiding restores
// approved, undeleted comments; moderation and deletion state are preserved.
func (e *Engine) SetAccountHidden(ctx context.Context, accountID string, hidden bool) error {
	return e.content.SetAccountHidden(ctx, accountID, hidden)
}

// AnonymizeAccount is the host consumer's idempotent response to user.purged.
// It keeps comments and anonymous contributions, unlinks the account and
// removes its private inbox, avatars, searches and provider data. Errors keep
// the consumer's checkpoint pending for retry; no periodic job is needed.
func (e *Engine) AnonymizeAccount(ctx context.Context, accountID string) error {
	if strings.TrimSpace(accountID) == "" {
		return errors.New("contentkit: account id is required")
	}
	table := pgx.Identifier{e.schema, "content_account_lifecycle"}.Sanitize()
	var state, anonymous string
	err := pgx.BeginFunc(ctx, e.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, e.schema+"\x1ferasure\x1f"+e.cfg.Scope.AppID.String()+"\x1f"+accountID); err != nil {
			return err
		}
		// The intent and the purging row commit together, so an app intent
		// without either lifecycle state can only come from a catalog restore.
		if err := e.recordAccountErasureIntent(ctx, tx, []string{accountID}); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `INSERT INTO `+table+` AS current (tenant_id,actor_id,state,anonymous_key)
VALUES ($1,$2,'purging',gen_random_uuid()) ON CONFLICT (tenant_id,actor_id) DO UPDATE
SET state=CASE WHEN current.state='purged' THEN 'purged' ELSE 'purging' END,
anonymous_key=CASE WHEN current.state='purged' THEN NULL ELSE coalesce(current.anonymous_key,gen_random_uuid()) END
RETURNING state,coalesce(anonymous_key::text,'')`, e.cfg.Scope.AppID.String(), accountID).Scan(&state, &anonymous)
	})
	if err != nil || state == "purged" {
		return err
	}
	if err := e.content.AnonymizeAccount(ctx, accountID, anonymous); err != nil {
		return err
	}
	if err := e.anonymizeSignals(ctx, accountID, anonymous); err != nil {
		return err
	}
	if err := e.discover.EraseSearches(ctx, []signal.Subject{{UserID: accountID}}); err != nil {
		return err
	}
	if err := e.eraseAvatars(ctx, []string{accountID}); err != nil {
		return err
	}
	if err := e.eraseMediaBookkeeping(ctx, accountID); err != nil {
		return err
	}
	if err := e.completeErasure(ctx, []string{accountID}); err != nil {
		return err
	}
	_, err = e.pool.Exec(ctx, `UPDATE `+table+` SET state='purged',anonymous_key=NULL WHERE tenant_id=$1 AND actor_id=$2 AND state='purging'`, e.cfg.Scope.AppID.String(), accountID)
	return err
}

// ErrSignalPlaneRequired keeps an account erasure pending on an engine without
// a signal plane when the app's plane was provisioned (MigrateClickHouse):
// its signals still hold the account.
var ErrSignalPlaneRequired = errors.New("contentkit: this app's signal plane is provisioned; configure ClickHouse to finish account erasure")

// anonymizeSignals moves the account's signals to its anonymous key. Only an
// app whose signal plane was never provisioned has none to move.
func (e *Engine) anonymizeSignals(ctx context.Context, accountID, anonymous string) error {
	err := e.hub.AnonymizeSubject(ctx, signal.Subject{UserID: accountID}, signal.Subject{AnonKey: anonymous})
	if err == nil || errors.Is(err, hub.ErrSignalPlaneDown) || !errors.Is(err, hub.ErrSignalPlaneDisabled) {
		return err
	}
	schema, _ := e.accountStorage()
	var provisioned bool
	if err := e.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM `+pgx.Identifier{schema, "content_signal_planes"}.Sanitize()+` WHERE tenant_id=$1 AND app_id=$2)`,
		e.scope.TenantID.String(), e.scope.AppID.String()).Scan(&provisioned); err != nil {
		return err
	}
	if provisioned {
		return ErrSignalPlaneRequired
	}
	return nil
}

// eraseMediaBookkeeping removes the account from this app's media journal and
// quota ledger, and from the tenant's account media when this app holds it.
func (e *Engine) eraseMediaBookkeeping(ctx context.Context, accountID string) error {
	return pgx.BeginFunc(ctx, e.pool, func(tx pgx.Tx) error {
		if _, err := subjectfence.Lock(ctx, tx, e.schema, e.cfg.Scope.AppID.String(), accountID); err != nil {
			return err
		}
		if err := mediastore.EraseAccountTx(ctx, tx, e.schema, e.cfg.Scope.AppID.String(), accountID); err != nil {
			return err
		}
		if account, ok := e.accountMediaStorage(); ok && account.Schema != e.schema {
			return mediastore.EraseAccountTx(ctx, tx, account.Schema, e.accountMediaPartition(), accountID)
		}
		return nil
	})
}

// AccountErasure is one account's lifecycle in this app: its state, when its
// erasure was requested and when it completed.
func (e *Engine) AccountErasure(ctx context.Context, accountID string) (content.AccountErasure, error) {
	out := content.AccountErasure{AccountID: accountID, State: content.AccountStateActive}
	if strings.TrimSpace(accountID) == "" {
		return out, errors.New("contentkit: account id is required")
	}
	schema, _ := e.accountStorage()
	var state string
	err := e.pool.QueryRow(ctx, `SELECT coalesce((SELECT state FROM `+pgx.Identifier{e.schema, "content_account_lifecycle"}.Sanitize()+` WHERE tenant_id=$3 AND actor_id=$4),''),
 (SELECT requested_at FROM `+pgx.Identifier{schema, "content_account_erasure_intents"}.Sanitize()+` WHERE tenant_id=$1 AND app_id=$2::text AND actor_id=$4),
 (SELECT completed_at FROM `+pgx.Identifier{schema, "content_account_erasure_receipts"}.Sanitize()+` WHERE tenant_id=$1 AND app_id=$2::uuid AND actor_id=$4)`,
		e.scope.TenantID.String(), e.scope.AppID.String(), e.cfg.Scope.AppID.String(), accountID).Scan(&state, &out.RequestedAt, &out.CompletedAt)
	if err != nil {
		return out, err
	}
	if state != "" {
		out.State = state
	}
	return out, nil
}
