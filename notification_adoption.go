package contentkit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/authkit/notify"
	notifypg "github.com/open-rails/authkit/notify/postgres"
)

// adoptCommentNoticeIdentities runs in the explicit migration step. The old
// local inbox is read once to retain its exact reply/like transition identities;
// normal publication and withdrawal only read ContentKit's own source state.
// No payload or user read state is copied into the source ledger.
func (e *Engine) adoptCommentNoticeIdentities(ctx context.Context) error {
	schema, namespace := e.accountStorage()
	available, err := e.accountErasureAvailable(ctx)
	if err != nil {
		return fmt.Errorf("contentkit: prepare the AuthKit account inbox with authkit.MigrateNotifications before adopting comment receipts: %w", err)
	}
	if !available {
		return nil
	}
	identities := pgx.Identifier{e.schema, "content_comment_notifications"}.Sanitize()
	comments := pgx.Identifier{e.schema, "content_comments"}.Sanitize()
	inbox := pgx.Identifier{schema, "notify_items"}.Sanitize()
	store, err := notifypg.New(e.pool, notifypg.Config{Schema: schema, Scope: "contentkit:" + namespace})
	if err != nil {
		return err
	}
	if e.scope != nil {
		if _, err := store.ClassifyLegacyOrigins(ctx, []notifypg.LegacyVisibility{{Origin: notify.OriginScope{System: "contentkit", Scope: e.cfg.Tenant}, Visibility: notify.VisibilityApp, AppID: e.scope.AppID.String()}}); err != nil {
			return fmt.Errorf("contentkit: classify adopted comment receipts: %w", err)
		}
	}
	if err := e.canonicalizeLegacyReplyNotices(ctx, store, inbox, identities, comments, "contentkit:"+namespace); err != nil {
		return err
	}
	_, err = e.pool.Exec(ctx, `INSERT INTO `+identities+` AS previous
	(tenant_id,comment_id,recipient,source_id,kind,revision,withdrawn,created_at)
	SELECT $1,c.id,n.recipient,n.source_id,n.kind,greatest(n.producer_revision,1),n.withdrawn,n.created_at
	FROM `+inbox+` n
	JOIN `+pgx.Identifier{e.schema, "content_comments"}.Sanitize()+` c ON c.tenant_id=$1 AND c.id::text=lower(n.data->>'comment_id')
	WHERE n.scope=$2 AND n.audience='user' AND n.source='contentkit' AND n.source_scope=$1
	 AND n.kind IN ('comment.reply','comment.liked')
	ON CONFLICT (tenant_id,recipient,source_id) DO UPDATE
	SET revision=greatest(previous.revision,EXCLUDED.revision),
	 withdrawn=CASE WHEN EXCLUDED.revision>=previous.revision THEN EXCLUDED.withdrawn ELSE previous.withdrawn END`, e.cfg.Tenant, "contentkit:"+namespace)
	if err != nil {
		return fmt.Errorf("contentkit: adopt comment notification identities: %w", err)
	}
	for {
		changed := 0
		err := pgx.BeginFunc(ctx, e.pool, func(tx pgx.Tx) error {
			// Old writers never withdrew obsolete receipts. Reconcile their exact
			// adopted identities against current comment visibility at cutover.
			rows, err := tx.Query(ctx, `WITH obsolete AS (
			SELECT n.tenant_id,n.recipient,n.source_id,i.visibility,i.app_id FROM `+identities+` n
			JOIN `+comments+` c ON c.tenant_id=n.tenant_id AND c.id=n.comment_id
			JOIN `+inbox+` i ON i.scope=$3 AND i.audience='user' AND i.recipient=n.recipient
			 AND i.source='contentkit' AND i.source_scope=n.tenant_id AND i.source_id=n.source_id
			LEFT JOIN `+comments+` parent ON parent.tenant_id=c.tenant_id AND parent.id=c.reply_to_id
			WHERE n.tenant_id=$1 AND NOT i.withdrawn AND
			 (n.withdrawn OR c.deleted_at IS NOT NULL OR c.moderation<>'approved' OR
			 (n.kind='comment.reply' AND (parent.id IS NULL OR parent.deleted_at IS NOT NULL OR parent.moderation<>'approved')))
			ORDER BY n.recipient,n.source_id LIMIT $2 FOR UPDATE OF n
			)
			UPDATE `+identities+` n SET withdrawn=true,revision=n.revision+1 FROM obsolete o
			WHERE n.tenant_id=o.tenant_id AND n.recipient=o.recipient AND n.source_id=o.source_id
			RETURNING n.recipient,n.source_id,n.revision,o.visibility,o.app_id`, e.cfg.Tenant, notify.MaxMutations, "contentkit:"+namespace)
			if err != nil {
				return err
			}
			batch, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (notify.Mutation, error) {
				m := notify.Mutation{Input: notify.Input{Recipient: notify.Recipient{Audience: notify.AudienceUser}, Origin: notify.Origin{System: "contentkit", Scope: e.cfg.Tenant}}, Withdrawn: true}
				err := row.Scan(&m.Recipient.ID, &m.Origin.ID, &m.Revision, &m.Visibility, &m.AppID)
				return m, err
			})
			if err != nil {
				return err
			}
			changed = len(batch)
			slices.SortFunc(batch, func(a, b notify.Mutation) int { return strings.Compare(a.Recipient.ID, b.Recipient.ID) })
			for _, m := range batch {
				err := store.ApplyTx(ctx, tx, []notify.Mutation{m})
				if errors.Is(err, notify.ErrRecipientErased) {
					// A restored receipt behind a durable erasure fence must not
					// survive or keep this migration looping. Remove only this
					// exact fenced receipt, never another app's account notices.
					_, err = tx.Exec(ctx, `DELETE FROM `+inbox+` WHERE scope=$1 AND audience='user' AND recipient=$2 AND source=$3 AND source_scope=$4 AND source_id=$5`, "contentkit:"+namespace, m.Recipient.ID, m.Origin.System, m.Origin.Scope, m.Origin.ID)
				}
				if err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil || changed == 0 {
			return err
		}
	}
}

// Earlier reply writers used the request's UUID spelling in the origin ID.
// Adopt only that known legacy format. Canonical receipts win collisions;
// otherwise retain the newest old receipt's ID and local read/archive state.
func (e *Engine) canonicalizeLegacyReplyNotices(ctx context.Context, store *notifypg.Store, inbox, identities, comments, scope string) error {
	for {
		found := false
		err := pgx.BeginFunc(ctx, e.pool, func(tx pgx.Tx) error {
			old := notify.Item{Input: notify.Input{Recipient: notify.Recipient{Audience: notify.AudienceUser}, Origin: notify.Origin{System: "contentkit", Scope: e.cfg.Tenant}}}
			var canonical string
			err := tx.QueryRow(ctx, `SELECT n.id,n.recipient,n.source_id,n.kind,n.data,n.created_at,n.visibility,n.app_id,'comment.reply:'||c.id::text
			FROM `+inbox+` n JOIN `+comments+` c ON c.tenant_id=$1 AND c.id::text=lower(n.data->>'comment_id')
			WHERE n.scope=$2 AND n.audience='user' AND n.source='contentkit' AND n.source_scope=$1
			AND n.kind='comment.reply' AND n.producer_revision=0
			AND lower(n.source_id)='comment.reply:'||c.id::text AND n.source_id<>'comment.reply:'||c.id::text
			ORDER BY n.created_at DESC,n.id DESC LIMIT 1 FOR UPDATE OF n`, e.cfg.Tenant, scope).
				Scan(&old.ID, &old.Recipient.ID, &old.Origin.ID, &old.Kind, &old.Data, &old.CreatedAt, &old.Visibility, &old.AppID, &canonical)
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			found = true
			var exists bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM `+inbox+` WHERE scope=$1 AND audience='user' AND recipient=$2 AND source='contentkit' AND source_scope=$3 AND source_id=$4)`, scope, old.Recipient.ID, e.cfg.Tenant, canonical).Scan(&exists); err != nil {
				return err
			}
			if !exists {
				// Keep this a legacy revision-zero row until the canonical source
				// emits its first mutation; its old producer digest is empty.
				var payload map[string]json.RawMessage
				if err := json.Unmarshal(old.Data, &payload); err != nil {
					return err
				}
				payload["comment_id"], _ = json.Marshal(strings.TrimPrefix(canonical, "comment.reply:"))
				data, err := json.Marshal(payload)
				if err != nil {
					return err
				}
				if _, err := tx.Exec(ctx, `UPDATE `+inbox+` SET source_id=$3,data=$4,revision=revision+1,updated_at=clock_timestamp() WHERE scope=$1 AND id=$2`, scope, old.ID, canonical, data); err != nil {
					return err
				}
			}
			var revision int64
			if err := tx.QueryRow(ctx, `SELECT coalesce(max(revision),0)+1 FROM `+identities+` WHERE tenant_id=$1 AND recipient=$2 AND source_id=$3`, e.cfg.Tenant, old.Recipient.ID, old.Origin.ID).Scan(&revision); err != nil {
				return err
			}
			if err := store.ApplyTx(ctx, tx, []notify.Mutation{{Input: old.Input, Revision: revision, Withdrawn: true}}); errors.Is(err, notify.ErrRecipientErased) {
				if _, err := tx.Exec(ctx, `DELETE FROM `+inbox+` WHERE scope=$1 AND audience='user' AND recipient=$2 AND source='contentkit' AND source_scope=$3 AND source_id=ANY($4)`, scope, old.Recipient.ID, e.cfg.Tenant, []string{old.Origin.ID, canonical}); err != nil {
					return err
				}
			} else if err != nil {
				return err
			}
			_, err = tx.Exec(ctx, `UPDATE `+identities+` SET withdrawn=true,revision=$4 WHERE tenant_id=$1 AND recipient=$2 AND source_id=$3`, e.cfg.Tenant, old.Recipient.ID, old.Origin.ID, revision)
			return err
		})
		if err != nil || !found {
			return err
		}
	}
}
