package content

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/helpers/notify"

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
	err := tx.QueryRow(ctx, `SELECT parent.user_id,coalesce(reply.user_id,''),parent.id::text,reply.content_kind,reply.content_id,reply.content_version_id
 FROM `+c.s.t.comments+` reply JOIN `+c.s.t.comments+` parent ON parent.id=reply.reply_to_id AND parent.tenant_id=reply.tenant_id
 WHERE reply.id=$1 AND reply.tenant_id=$2 AND reply.moderation='approved' AND reply.deleted_at IS NULL
 AND parent.moderation='approved' AND parent.deleted_at IS NULL AND parent.user_id IS NOT NULL`, replyID, c.s.tenant).
		Scan(&recipient, &actor, &parent, &ref.ContentKind, &ref.ContentID, &ref.ContentVersionID)
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

// notifyLike runs only after a real transition to +1. The durable reaction
// revision identifies that transition; concurrent duplicate likes emit once.
func (c *comments) notifyLike(ctx context.Context, tx pgx.Tx, actor access.Actor, cid string, ref contentref.ContentRef) error {
	if actor.Anonymous || actor.ID == "" {
		return nil
	}
	var recipient string
	err := tx.QueryRow(ctx, `SELECT coalesce(user_id,'') FROM `+c.s.t.comments+` WHERE id=$1 AND tenant_id=$2`, cid, c.s.tenant).Scan(&recipient)
	if err != nil {
		return fmt.Errorf("content: resolve like recipient: %w", err)
	}
	if recipient == "" || recipient == actor.ID {
		return nil
	}
	var revision int64
	if err := tx.QueryRow(ctx, `SELECT revision FROM `+c.s.t.reactions+` WHERE tenant_id=$1 AND content_kind='comment' AND content_id=$2 AND content_version_id IS NULL AND user_id=$3`, c.s.tenant, cid, actor.ID).Scan(&revision); err != nil {
		return fmt.Errorf("content: resolve like revision: %w", err)
	}
	return c.publishNotice(ctx, tx, recipient, "comment.liked", fmt.Sprintf("comment.liked:%d", revision), commentNotice{ContentRef: ref, ActorID: actor.ID, CommentID: cid})
}

func (c *comments) publishNotice(ctx context.Context, tx pgx.Tx, recipient, kind, eventID string, payload commentNotice) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("content: encode notification: %w", err)
	}
	_, err = c.rt.Notifications.PublishTx(ctx, tx, notify.Input{Recipient: notify.Recipient{Audience: notify.AudienceUser, ID: recipient}, Origin: notify.Origin{System: "contentkit", Scope: c.s.tenant, ID: eventID}, Kind: kind, Data: data, CreatedAt: time.Now().UTC()})
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
