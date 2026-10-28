package contentstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/authkit/notify"
	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
)

// commentNotice carries references, never copied comment bodies, private
// moderation details or anonymous IP addresses. The app resolves content with
// its current access checks when presenting or following a notification.
type commentNotice struct {
	contentref.ContentRef
	ActorID   string `json:"actor_id,omitempty"`
	CommentID string `json:"comment_id"`
	ReplyToID string `json:"reply_to_id,omitempty"`
}

// notifyReply publishes only visible replies to another registered person's
// published comment. The reply's identity deduplicates review/edit reapproval.
func (c *comments) notifyReply(ctx context.Context, tx pgx.Tx, replyID string) error {
	var recipient, actor, parent string
	var ref contentref.ContentRef
	err := tx.QueryRow(ctx, `SELECT reply.id::text,parent.user_id,coalesce(reply.user_id,''),parent.id::text,reply.content_kind,reply.content_id,reply.content_version_id
 FROM `+c.s.t.comments+` reply JOIN `+c.s.t.comments+` parent ON parent.id=reply.reply_to_id AND parent.tenant_id=reply.tenant_id
 WHERE reply.id=$1 AND reply.tenant_id=$2 AND reply.moderation='approved' AND reply.deleted_at IS NULL AND NOT reply.author_hidden
 AND parent.moderation='approved' AND parent.deleted_at IS NULL AND NOT parent.author_hidden AND parent.user_id IS NOT NULL`, replyID, c.s.tenant).
		Scan(&replyID, &recipient, &actor, &parent, &ref.ContentKind, &ref.ContentID, &ref.ContentVersionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("content: resolve reply notification: %w", err)
	}
	if recipient == "" || recipient == actor {
		return nil
	}
	ref.TenantID = c.s.tenant
	return c.publishNotice(ctx, tx, recipient, "comment.reply", "comment.reply:"+replyID, commentNotice{ContentRef: ref, ActorID: actor, CommentID: replyID, ReplyToID: parent})
}

// notifyLike uses one identity per liker and comment across unlike/re-like.
func (c *comments) notifyLike(ctx context.Context, tx pgx.Tx, actor access.Actor, cid string, ref contentref.ContentRef) error {
	if !actor.Member() {
		return nil
	}
	var recipient string
	err := tx.QueryRow(ctx, `SELECT coalesce(user_id,'') FROM `+c.s.t.comments+` WHERE id=$1 AND tenant_id=$2`, cid, c.s.tenant).Scan(&recipient)
	if err != nil {
		return fmt.Errorf("content: resolve like recipient: %w", err)
	}
	if recipient == "" || recipient == actor.AccountID() {
		return nil
	}
	return c.publishNotice(ctx, tx, recipient, "comment.liked", "comment.liked:"+cid+":"+actor.AccountID(), commentNotice{ContentRef: ref, ActorID: actor.AccountID(), CommentID: cid})
}

func (c *comments) publishNotice(ctx context.Context, tx pgx.Tx, recipient, kind, eventID string, payload commentNotice) error {
	if c.rt.notifier == nil {
		return nil
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("content: encode notification: %w", err)
	}
	var revision int64
	var createdAt time.Time
	err = tx.QueryRow(ctx, `INSERT INTO `+c.noticeTable()+` AS n
	(tenant_id,comment_id,recipient,source_id,kind,created_at) VALUES ($1,$2,$3,$4,$5,clock_timestamp())
	ON CONFLICT (tenant_id,recipient,source_id) DO UPDATE
	SET revision=n.revision+CASE WHEN n.withdrawn THEN 1 ELSE 0 END,withdrawn=false
	RETURNING revision,created_at`, c.s.tenant, payload.CommentID, recipient, eventID, kind).Scan(&revision, &createdAt)
	if err != nil {
		return fmt.Errorf("content: record notification identity: %w", err)
	}
	err = c.rt.notifier.Publish(ctx, []notify.Publication{{RecipientID: recipient, ID: eventID, Kind: kind, Data: data, CreatedAt: createdAt, Revision: revision}}, tx)
	// Erased recipients cannot receive new notices; their old published comments
	// can remain under host policy without blocking someone else's interaction.
	if errors.Is(err, notify.ErrRecipientErased) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("content: publish notification: %w", err)
	}
	return nil
}

func (c *comments) noticeTable() string {
	return pgx.Identifier{c.s.schema, "content_comment_notifications"}.Sanitize()
}

// withdrawNotices uses source-owned notice identities, never an inbox scan.
// Replies survive parent deletion, but their reply-to-parent notices do not.
// The caller holds the thread lock and records all withdrawals in its transaction.
func (c *comments) withdrawNotices(ctx context.Context, tx pgx.Tx, cid string) error {
	if c.rt.notifier == nil {
		return nil
	}
	for {
		rows, err := tx.Query(ctx, `WITH selected AS (
	SELECT tenant_id,recipient,source_id FROM `+c.noticeTable()+`
	WHERE tenant_id=$1 AND NOT withdrawn AND (comment_id=$2 OR
	 (kind='comment.reply' AND comment_id IN (SELECT id FROM `+c.s.t.comments+` WHERE tenant_id=$1 AND reply_to_id=$2)))
	ORDER BY recipient,source_id LIMIT $3 FOR UPDATE
)
	UPDATE `+c.noticeTable()+` n SET withdrawn=true,revision=n.revision+1 FROM selected s
	WHERE n.tenant_id=s.tenant_id AND n.recipient=s.recipient AND n.source_id=s.source_id
	RETURNING n.recipient,n.source_id,n.revision`, c.s.tenant, cid, notify.MaxMutations)
		if err != nil {
			return err
		}
		withdrawals, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (notify.Withdrawal, error) {
			var m notify.Withdrawal
			err := row.Scan(&m.RecipientID, &m.ID, &m.Revision)
			return m, err
		})
		if err != nil || len(withdrawals) == 0 {
			return err
		}
		if err := c.rt.notifier.Withdraw(ctx, withdrawals, tx); err != nil {
			return fmt.Errorf("content: withdraw comment notifications: %w", err)
		}
	}
}

// restoreNotices republishes visible replies and active likes with their
// existing identities and newer producer revisions.
func (c *comments) restoreNotices(ctx context.Context, tx pgx.Tx, cid string) error {
	if err := c.notifyReply(ctx, tx, cid); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `SELECT id::text FROM `+c.s.t.comments+` WHERE tenant_id=$1 AND reply_to_id=$2 AND deleted_at IS NULL AND moderation='approved' AND NOT author_hidden ORDER BY id`, c.s.tenant, cid)
	if err != nil {
		return err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := c.notifyReply(ctx, tx, id); err != nil {
			return err
		}
	}
	return c.restoreLikeNotices(ctx, tx, cid, "")
}

func (c *comments) restoreLikeNotices(ctx context.Context, tx pgx.Tx, cid, actorID string) error {
	rows, err := tx.Query(ctx, `SELECT c.user_id,r.user_id,c.content_kind,c.content_id,c.content_version_id
FROM `+c.s.t.comments+` c
JOIN `+c.s.t.reactions+` r ON r.tenant_id=c.tenant_id AND r.content_kind='comment' AND r.content_id=c.id::text
 AND r.content_version_id IS NULL AND r.value=1 AND NOT r.author_hidden AND r.user_id<>c.user_id
JOIN `+c.noticeTable()+` n ON n.tenant_id=c.tenant_id AND n.recipient=c.user_id
 AND n.source_id='comment.liked:' || c.id::text || ':' || r.user_id AND n.withdrawn
WHERE c.tenant_id=$1 AND c.id=$2 AND c.moderation='approved' AND c.deleted_at IS NULL AND NOT c.author_hidden
 AND ($3='' OR r.user_id=$3)`, c.s.tenant, cid, actorID)
	if err != nil {
		return err
	}
	type likeNotice struct {
		recipient string
		notice    commentNotice
	}
	likes, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (likeNotice, error) {
		var out likeNotice
		n := &out.notice
		n.TenantID, n.CommentID = c.s.tenant, cid
		err := row.Scan(&out.recipient, &n.ActorID, &n.ContentKind, &n.ContentID, &n.ContentVersionID)
		return out, err
	})
	if err != nil {
		return err
	}
	for _, like := range likes {
		n := like.notice
		if err := c.publishNotice(ctx, tx, like.recipient, "comment.liked", "comment.liked:"+cid+":"+n.ActorID, n); err != nil {
			return err
		}
	}
	return nil
}

// recordHiddenLike keeps a hidden liker's like as a withdrawn notice identity,
// so unhiding restores it like any withdrawn notice; a like that never had an
// identity (an import) never notifies.
func (c *comments) recordHiddenLike(ctx context.Context, tx pgx.Tx, actor access.Actor, cid string) error {
	if c.rt.notifier == nil || !actor.Member() {
		return nil
	}
	_, err := tx.Exec(ctx, `INSERT INTO `+c.noticeTable()+` (tenant_id,comment_id,recipient,source_id,kind,withdrawn,created_at)
SELECT c.tenant_id,c.id,c.user_id,'comment.liked:'||c.id::text||':'||$3,'comment.liked',true,clock_timestamp()
FROM `+c.s.t.comments+` c JOIN `+c.rt.hiddenAuthorsTable()+` h ON h.tenant_id=c.tenant_id AND h.actor_id=$3
WHERE c.tenant_id=$1 AND c.id=$2 AND c.user_id IS NOT NULL AND c.user_id<>$3
ON CONFLICT (tenant_id,recipient,source_id) DO NOTHING`, c.s.tenant, cid, actor.AccountID())
	return err
}

func (c *comments) withdrawLikeNotice(ctx context.Context, tx pgx.Tx, actorID, cid string) error {
	if c.rt.notifier == nil || actorID == "" {
		return nil
	}
	var m notify.Withdrawal
	err := tx.QueryRow(ctx, `UPDATE `+c.noticeTable()+` SET withdrawn=true,revision=revision+1
WHERE tenant_id=$1 AND source_id=$2 AND NOT withdrawn RETURNING recipient,source_id,revision`,
		c.s.tenant, "comment.liked:"+cid+":"+actorID).Scan(&m.RecipientID, &m.ID, &m.Revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return c.rt.notifier.Withdraw(ctx, []notify.Withdrawal{m}, tx)
}
