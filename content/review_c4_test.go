package content

import (
	"context"
	"net/http"
	"testing"

	"github.com/open-rails/contentkit/access"
)

func TestReviewC4StaleReviewCannotPublishEditedText(t *testing.T) {
	rt := moderatedRuntime(t, &fakeModerator{})
	ctx := context.Background()
	author := access.Actor{ID: "author"}
	held := mustComment(t, rt, author, "gallery", "1", createInput{Body: "iffy original"})
	page, err := rt.ListHeld(ctx, KindComment, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rt.comments.edit(ctx, author, held.ID, "iffy replacement never reviewed"); err != nil {
		t.Fatal(err)
	}
	decision := ReviewDecision{Revision: page.Items[0].Revision, Decision: DecisionApprove, Reviewer: "reviewer"}
	_ = page // reviewer saw this immutable page before the edit
	if err := rt.Resolve(ctx, KindComment, held.ID, decision); err == nil {
		t.Fatal("stale reviewer decision published replacement text")
	}
}

func TestReviewC4HeldEditsReturnAccepted(t *testing.T) {
	rt := moderatedRuntime(t, &fakeModerator{})
	author := access.Actor{ID: "author"}
	cm := mustComment(t, rt, author, "gallery", "1", createInput{Body: "fine"})
	res := doJSON(t, rt.Handler(), author, "PATCH", "/comments/"+cm.ID, map[string]string{"body": "iffy edit"})
	if res.Code != http.StatusAccepted {
		t.Fatalf("held edit status=%d body=%s", res.Code, res.Body.String())
	}
}

func TestReviewC4DeletedHeldCommentLeavesReviewQueue(t *testing.T) {
	rt := moderatedRuntime(t, &fakeModerator{})
	actor := access.Actor{ID: "author"}
	ctx := context.Background()
	cm := mustComment(t, rt, actor, "gallery", "1", createInput{Body: "iffy"})
	if err := rt.comments.softDelete(ctx, actor, cm.ID); err != nil {
		t.Fatal(err)
	}
	page, err := rt.ListHeld(ctx, KindComment, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 0 {
		t.Fatal("deleted held comment is still offered for publication review")
	}
}

func TestReviewC4RetryDoesNotReclassifyUnchangedAnswer(t *testing.T) {
	cl := &fakeClassifier{}
	rt, p := newPollTest(t, Options{Classifier: cl})
	ctx := context.Background()
	poll, err := p.create(ctx, pollAdmin, freeTextPoll())
	if err != nil {
		t.Fatal(err)
	}
	first, err := p.answer(ctx, access.Actor{ID: "user"}, poll.ID, "cats")
	if err != nil {
		t.Fatal(err)
	}
	cl.mu.Lock()
	cl.failN = 1
	cl.mu.Unlock()
	second, err := p.answer(ctx, access.Actor{ID: "user"}, poll.ID, "cats")
	if err != nil {
		t.Fatal(err)
	}
	if !second.MyAnswer.Classified || !second.MyAnswer.UpdatedAt.Equal(first.MyAnswer.UpdatedAt) {
		t.Fatal("idempotent answer retry reset classification and mutation time")
	}
	_ = rt
}
