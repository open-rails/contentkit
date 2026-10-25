package critical_test

import (
	"context"
	"strings"
	"testing"

	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/content"
)

// Only the external moderation decision is controlled. Comment writes,
// review authorization, source revisions and AuthKit inbox storage remain real.
type notificationReviewControl struct{}

func (notificationReviewControl) StatelessPolicy() {}

func (notificationReviewControl) Screen(_ context.Context, in content.ModerationInput) (content.Verdict, error) {
	decision := content.DecisionApprove
	if strings.HasPrefix(in.Text, "review:") {
		decision = content.DecisionReview
	}
	return content.Verdict{Decision: decision, Reason: "Selected external moderation result", Model: "critical-boundary", PromptVersion: "1"}, nil
}

func TestAccountNotificationScreening(t *testing.T) {
	t.Run("CK-J25-S17_held_creation_edit_and_reapproval_publish_exactly_once", func(t *testing.T) {
		n := newAccountNoticeWorld(t)
		n.a.deps.Moderator = notificationReviewControl{}
		n.reopen(t)
		owner, ownerToken := n.w.person(t, 0)
		writer, writerToken := n.w.person(t, 0)
		item := publishedArticle(t, n.a, "Screened notification replies")
		parent := postComment(t, n.a.client(ownerToken), item.ID, "Parent")
		held := n.reply(t, writerToken, item.ID, parent.ID, "review: original private reply")
		if held.Moderation != "held" || len(n.items(t, owner.ID)) != 0 {
			t.Fatal("initial held reply reached recipient inbox")
		}
		admin := n.a.engine.Client()
		queue, err := admin.GetModerationHeld(t.Context(), ck.GetModerationHeldQuery{Kind: content.KindComment})
		must(t, err)
		if len(queue.Items) != 1 || queue.Items[0].ID != held.ID {
			t.Fatalf("held reply missing from real review queue: %+v", queue)
		}
		firstRevision := queue.Items[0].Revision
		_, err = admin.PostModerationResolve(t.Context(), content.KindComment, held.ID, content.ResolveInput{Decision: content.DecisionApprove, Revision: firstRevision})
		must(t, err)
		notices := n.items(t, owner.ID)
		if len(notices) != 1 || notices[0].Origin.ID != "comment.reply:"+held.ID || strings.Contains(string(notices[0].Data), "original private") {
			t.Fatalf("review approval did not publish safe exact receipt: %+v", notices)
		}
		original := notices[0]
		_, err = n.accounts.SetNotificationRead(t.Context(), owner.ID, original.ID, true)
		must(t, err)
		changed, err := n.a.client(writerToken).PatchComment(t.Context(), held.ID, content.CommentEdit{Body: "review: edited private reply"})
		must(t, err)
		if changed.Moderation != "held" || len(n.items(t, owner.ID)) != 0 {
			t.Fatal("held edit did not withdraw the existing receipt")
		}
		queue, err = admin.GetModerationHeld(t.Context(), ck.GetModerationHeldQuery{Kind: content.KindComment})
		must(t, err)
		if len(queue.Items) != 1 || queue.Items[0].ID != held.ID || queue.Items[0].Revision <= firstRevision {
			t.Fatalf("edited review did not advance source revision: %+v", queue)
		}
		_, err = admin.PostModerationResolve(t.Context(), content.KindComment, held.ID, content.ResolveInput{Decision: content.DecisionApprove, Revision: queue.Items[0].Revision})
		must(t, err)
		notices = n.items(t, owner.ID)
		if len(notices) != 1 || notices[0].ID != original.ID || notices[0].ReadAt != nil || notices[0].ProducerRevision != 3 {
			t.Fatalf("reapproval did not reopen original unread receipt: %+v", notices)
		}
		second := n.reply(t, writerToken, item.ID, parent.ID, "review: second reply")
		if len(n.items(t, owner.ID)) != 1 {
			t.Fatal("second held reply was already published")
		}
		changed, err = n.a.client(writerToken).PatchComment(t.Context(), second.ID, content.CommentEdit{Body: "Approved replacement"})
		must(t, err)
		if changed.Moderation != "" {
			t.Fatalf("approved edit remained held: %+v", changed)
		}
		notices = n.items(t, owner.ID)
		if len(notices) != 2 || len(n.items(t, writer.ID)) != 0 {
			t.Fatalf("edit approval missing or sender notified: %+v", notices)
		}
		seen := map[string]bool{}
		for _, notice := range notices {
			seen[notice.Origin.ID] = true
		}
		if !seen["comment.reply:"+held.ID] || !seen["comment.reply:"+second.ID] {
			t.Fatalf("approval changed source identity: %+v", notices)
		}
	})
}
