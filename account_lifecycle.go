package contentkit

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/contentkit/internal/hub"
	"github.com/open-rails/contentkit/signal"
)

// SetAccountDeleted hides account attribution during AuthKit's recovery
// period. Restoring the account clears the marker and restores attribution;
// comments, favorites, votes and reading data are untouched.
func (e *Engine) SetAccountDeleted(ctx context.Context, accountID string, deleted bool) error {
	return e.content.SetAccountDeleted(ctx, accountID, deleted)
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
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, e.schema+"\x1ferasure\x1f"+e.cfg.Tenant+"\x1f"+accountID); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `INSERT INTO `+table+` AS current (tenant_id,actor_id,state,anonymous_key)
VALUES ($1,$2,'purging',gen_random_uuid()) ON CONFLICT (tenant_id,actor_id) DO UPDATE
SET state=CASE WHEN current.state='purged' THEN 'purged' ELSE 'purging' END,
anonymous_key=CASE WHEN current.state='purged' THEN NULL ELSE coalesce(current.anonymous_key,gen_random_uuid()) END
RETURNING state,coalesce(anonymous_key::text,'')`, e.cfg.Tenant, accountID).Scan(&state, &anonymous)
	})
	if err != nil || state == "purged" {
		return err
	}
	if err := e.content.AnonymizeAccount(ctx, accountID); err != nil {
		return err
	}
	if err := e.hub.AnonymizeSubject(ctx, signal.Subject{UserID: accountID}, signal.Subject{AnonKey: anonymous}); err != nil && !errors.Is(err, hub.ErrSignalPlaneDisabled) {
		return err
	} else if errors.Is(err, hub.ErrSignalPlaneDown) {
		return err
	}
	if err := e.discover.EraseSearches(ctx, []signal.Subject{{UserID: accountID}}); err != nil {
		return err
	}
	if err := e.eraseAvatars(ctx, []string{accountID}); err != nil {
		return err
	}
	if err := e.completeErasure(ctx, []string{accountID}, true); err != nil {
		return err
	}
	_, err = e.pool.Exec(ctx, `UPDATE `+table+` SET state='purged',anonymous_key=NULL WHERE tenant_id=$1 AND actor_id=$2 AND state='purging'`, e.cfg.Tenant, accountID)
	return err
}
