package contentkit

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// adoptCommentNoticeIdentities runs in the explicit migration step. The old
// local inbox is read once to retain its exact reply/like transition identities;
// normal publication and withdrawal only read ContentKit's own source state.
// No payload or user read state is copied into the source ledger.
func (e *Engine) adoptCommentNoticeIdentities(ctx context.Context) error {
	schema, namespace := e.accountStorage()
	_, err := e.pool.Exec(ctx, `INSERT INTO `+pgx.Identifier{e.schema, "content_comment_notifications"}.Sanitize()+`
	(tenant_id,comment_id,recipient,source_id,kind,revision,withdrawn,created_at)
	SELECT $1,c.id,n.recipient,n.source_id,n.kind,greatest(n.producer_revision,1),n.withdrawn,n.created_at
	FROM `+pgx.Identifier{schema, "notify_items"}.Sanitize()+` n
	JOIN `+pgx.Identifier{e.schema, "content_comments"}.Sanitize()+` c ON c.tenant_id=$1 AND c.id::text=n.data->>'comment_id'
	WHERE n.scope=$2 AND n.audience='user' AND n.source='contentkit' AND n.source_scope=$1
	 AND n.kind IN ('comment.reply','comment.liked')
	ON CONFLICT (tenant_id,recipient,source_id) DO NOTHING`, e.cfg.Tenant, "contentkit:"+namespace)
	if err != nil {
		return fmt.Errorf("contentkit: adopt comment notification identities: %w", err)
	}
	return nil
}
