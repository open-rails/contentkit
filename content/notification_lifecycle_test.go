package content

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/helpers/notify"
	notifypg "github.com/open-rails/helpers/notify/postgres"
)

func TestCommentDeletionWithdrawsExactNoticesWithoutDeletingReplies(t *testing.T) {
	ctx := t.Context()
	rt := commentsRuntime(t, Options{})
	owner, writer := access.Actor{ID: "owner"}, access.Actor{ID: "writer"}
	parent := mustComment(t, rt, owner, "gallery", cid(1), CommentInput{Body: "parent"})
	b := mustComment(t, rt, writer, "gallery", cid(1), CommentInput{Body: "B", ReplyToID: parent.ID})
	c := mustComment(t, rt, writer, "gallery", cid(1), CommentInput{Body: "C", ReplyToID: parent.ID})
	before := inbox(t, rt, "owner")
	if len(before) != 2 {
		t.Fatalf("reply notices: %d", len(before))
	}
	if err := rt.comments.softDelete(ctx, writer, b.ID); err != nil {
		t.Fatal(err)
	}
	remaining := inbox(t, rt, "owner")
	if len(remaining) != 1 || remaining[0].Origin.ID != "comment.reply:"+c.ID {
		t.Fatalf("deleting B removed wrong notices: %+v", remaining)
	}
	if err := rt.comments.softDelete(ctx, owner, parent.ID); err != nil {
		t.Fatal(err)
	}
	if got := inbox(t, rt, "owner"); len(got) != 0 {
		t.Fatalf("deleted parent's reply notices remain: %+v", got)
	}
	var deleted bool
	if err := rt.store.pool.QueryRow(ctx, `SELECT deleted_at IS NOT NULL FROM `+rt.store.t.comments+` WHERE tenant_id=$1 AND id=$2`, rt.tenant, c.ID).Scan(&deleted); err != nil || deleted {
		t.Fatalf("parent deletion changed C: deleted=%v err=%v", deleted, err)
	}
	// A delayed delivery retains the original revision and cannot resurrect
	// either withdrawn notice, even though neither was read before deletion.
	for _, old := range before {
		if err := rt.notifier.Apply(ctx, []notify.Mutation{{Input: old.Input, Revision: old.ProducerRevision}}); err != nil {
			t.Fatal(err)
		}
	}
	if len(inbox(t, rt, "owner")) != 0 {
		t.Fatal("delayed publication resurrected deleted reply")
	}
	if err := rt.comments.restore(ctx, parent.ID); err != nil {
		t.Fatal(err)
	}
	remaining = inbox(t, rt, "owner")
	if len(remaining) != 1 || remaining[0].Origin.ID != "comment.reply:"+c.ID || remaining[0].ProducerRevision != 3 || remaining[0].ReadAt != nil {
		t.Fatalf("restore did not reopen only live C: %+v", remaining)
	}
}

func TestCommentWithdrawAndDeleteRollBackTogether(t *testing.T) {
	ctx := t.Context()
	rt := commentsRuntime(t, Options{})
	owner, writer := access.Actor{ID: "owner"}, access.Actor{ID: "writer"}
	parent := mustComment(t, rt, owner, "gallery", cid(1), CommentInput{Body: "parent"})
	reply := mustComment(t, rt, writer, "gallery", cid(1), CommentInput{Body: "reply", ReplyToID: parent.ID})
	table := pgx.Identifier{rt.schema, "notify_items"}.Sanitize()
	if _, err := rt.store.pool.Exec(ctx, `ALTER TABLE `+table+` ADD CONSTRAINT reject_withdraw CHECK (NOT withdrawn)`); err != nil {
		t.Fatal(err)
	}
	if err := rt.comments.softDelete(ctx, writer, reply.ID); err == nil {
		t.Fatal("reply deletion committed despite failed withdrawal")
	}
	var deleted bool
	if err := rt.store.pool.QueryRow(ctx, `SELECT deleted_at IS NOT NULL FROM `+rt.store.t.comments+` WHERE tenant_id=$1 AND id=$2`, rt.tenant, reply.ID).Scan(&deleted); err != nil || deleted {
		t.Fatalf("failed withdrawal leaked deletion: %v %v", deleted, err)
	}
	if got := inbox(t, rt, "owner"); len(got) != 1 || got[0].ProducerRevision != 1 {
		t.Fatalf("failed withdrawal changed receipt: %+v", got)
	}
}

func TestCommentPublisherUsesInjectedAccountStore(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Pool(t, nil)
	schema := pgtest.EmptySchema(t, ctx, pool)
	if err := notifypg.Migrate(ctx, pool, schema); err != nil {
		t.Fatal(err)
	}
	account, err := notifypg.New(pool, notifypg.Config{Schema: schema, Scope: "authkit:account-directory"})
	if err != nil {
		t.Fatal(err)
	}
	rt := commentsRuntime(t, Options{NotificationPublisher: account})
	owner, writer := access.Actor{ID: "owner"}, access.Actor{ID: "writer"}
	parent := mustComment(t, rt, owner, "gallery", cid(1), CommentInput{Body: "parent"})
	reply := mustComment(t, rt, writer, "gallery", cid(1), CommentInput{Body: "reply", ReplyToID: parent.ID})
	page, err := account.List(ctx, notify.Recipient{Audience: notify.AudienceUser, ID: "owner"}, notify.ListOptions{})
	if err != nil || len(page.Items) != 1 || page.Items[0].Origin.ID != "comment.reply:"+reply.ID {
		t.Fatalf("injected account inbox: %+v %v", page, err)
	}
	if len(inbox(t, rt, "owner")) != 0 {
		t.Fatal("notice was duplicated into ContentKit's old inbox")
	}
	if err := rt.comments.softDelete(ctx, writer, reply.ID); err != nil {
		t.Fatal(err)
	}
	page, err = account.List(ctx, notify.Recipient{Audience: notify.AudienceUser, ID: "owner"}, notify.ListOptions{})
	if err != nil || len(page.Items) != 0 {
		t.Fatalf("withdrawal did not reach injected inbox: %+v %v", page, err)
	}
}
