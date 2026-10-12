package content

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/helpers/notify"
)

func inbox(t *testing.T, rt *Runtime, user string) []notify.Item {
	t.Helper()
	p, err := rt.Notifications.List(context.Background(), notify.Recipient{Audience: notify.AudienceUser, ID: user}, notify.ListOptions{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	return p.Items
}

func TestCommentNotificationsPublicationAndIdentity(t *testing.T) {
	ctx := context.Background()
	rt := commentsRuntime(t, Options{Moderator: &fakeModerator{}})
	owner := access.Actor{ID: "owner"}
	writer := access.Actor{ID: "writer"}
	root := mustComment(t, rt, owner, "gallery", cid(1), CommentInput{Body: "root"})
	mustComment(t, rt, owner, "gallery", cid(1), CommentInput{Body: "self reply", ReplyToID: root.ID})
	held := mustComment(t, rt, writer, "gallery", cid(1), CommentInput{Body: "iffy reply", ReplyToID: root.ID})
	if got := inbox(t, rt, "owner"); len(got) != 0 {
		t.Fatalf("self/held notified: %+v", got)
	}
	if err := rt.Resolve(ctx, KindComment, held.ID, ReviewDecision{Revision: 1, Decision: DecisionApprove, Reviewer: "staff"}); err != nil {
		t.Fatal(err)
	}
	got := inbox(t, rt, "owner")
	if len(got) != 1 || got[0].Kind != "comment.reply" || !strings.Contains(string(got[0].Data), held.ID) || strings.Contains(string(got[0].Data), "iffy") {
		t.Fatalf("published notice: %+v", got)
	}
	if _, err := rt.Notifications.SetRead(ctx, got[0].Recipient, got[0].ID, true); err != nil {
		t.Fatal(err)
	}
	// A later held edit and reapproval do not re-notify or reset local read state.
	if _, err := rt.comments.edit(ctx, writer, held.ID, "iffy replacement"); err != nil {
		t.Fatal(err)
	}
	if err := rt.Resolve(ctx, KindComment, held.ID, ReviewDecision{Revision: 2, Decision: DecisionApprove, Reviewer: "staff"}); err != nil {
		t.Fatal(err)
	}
	got = inbox(t, rt, "owner")
	if len(got) != 1 || got[0].ReadAt == nil {
		t.Fatalf("reapproval reset notice: %+v", got)
	}
	// First publication by editing a held reply also emits.
	held2 := mustComment(t, rt, writer, "gallery", cid(1), CommentInput{Body: "iffy second", ReplyToID: root.ID})
	if _, err := rt.comments.edit(ctx, writer, held2.ID, "approved replacement"); err != nil {
		t.Fatal(err)
	}
	if len(inbox(t, rt, "owner")) != 2 {
		t.Fatal("edit publication missing notification")
	}
	if len(inbox(t, rt, "writer")) != 0 {
		t.Fatal("sender received own outgoing notification")
	}
}

func TestCommentLikeNotificationsConcurrentRetryAndTransitions(t *testing.T) {
	ctx := context.Background()
	rt := commentsRuntime(t, Options{})
	owner := access.Actor{ID: "owner"}
	reactor := access.Actor{ID: "liker"}
	cm := mustComment(t, rt, owner, "gallery", cid(1), CommentInput{Body: "root"})
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := rt.comments.reactTx(ctx, reactor, cm.ID, 1); errs <- err }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	got := inbox(t, rt, "owner")
	if len(got) != 1 || got[0].Kind != "comment.liked" {
		t.Fatalf("duplicate likes: %+v", got)
	}
	if _, err := rt.comments.reactTx(ctx, owner, cm.ID, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.comments.reactTx(ctx, access.Actor{Anonymous: true, IP: "192.0.2.1"}, cm.ID, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.comments.reactTx(ctx, reactor, cm.ID, -1); err != nil {
		t.Fatal(err)
	}
	if len(inbox(t, rt, "owner")) != 1 {
		t.Fatal("self/anonymous/dislike emitted notification")
	}
	if _, err := rt.comments.reactTx(ctx, reactor, cm.ID, 1); err != nil {
		t.Fatal(err)
	}
	got = inbox(t, rt, "owner")
	if len(got) != 2 || got[0].Origin.ID == got[1].Origin.ID {
		t.Fatalf("new like transition identity: %+v", got)
	}
}

func TestCommentNotificationRollbackAndRecipientErasure(t *testing.T) {
	ctx := context.Background()
	rt := commentsRuntime(t, Options{})
	owner := access.Actor{ID: "owner"}
	writer := access.Actor{ID: "writer"}
	root := mustComment(t, rt, owner, "gallery", cid(1), CommentInput{Body: "root"})
	table := pgx.Identifier{rt.schema, "notify_items"}.Sanitize()
	if _, err := rt.store.pool.Exec(ctx, `ALTER TABLE `+table+` ADD CONSTRAINT reject_reply CHECK(kind <> 'comment.reply')`); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.comments.create(ctx, writer, "gallery", cid(1), CommentInput{Body: "must rollback", ReplyToID: root.ID}); err == nil {
		t.Fatal("reply committed after notification insert failed")
	}
	var replies int
	if err := rt.store.pool.QueryRow(ctx, `SELECT count(*) FROM `+rt.store.t.comments+` WHERE reply_to_id=$1`, root.ID).Scan(&replies); err != nil || replies != 0 {
		t.Fatalf("reply leaked from rollback: %d %v", replies, err)
	}
	if _, err := rt.store.pool.Exec(ctx, `ALTER TABLE `+table+` DROP CONSTRAINT reject_reply`); err != nil {
		t.Fatal(err)
	}
	mustComment(t, rt, writer, "gallery", cid(1), CommentInput{Body: "published", ReplyToID: root.ID})
	previous := inbox(t, rt, "owner")[0]
	if err := rt.EraseSubjects(ctx, []string{"owner"}); err != nil {
		t.Fatal(err)
	}
	mustComment(t, rt, writer, "gallery", cid(1), CommentInput{Body: "after erasure", ReplyToID: root.ID})
	if len(inbox(t, rt, "owner")) != 0 {
		t.Fatal("erased recipient inbox recreated")
	}
	if _, err := rt.Notifications.Publish(ctx, previous.Input); !errors.Is(err, notify.ErrRecipientErased) {
		t.Fatalf("replay after erasure: %v", err)
	}
}
