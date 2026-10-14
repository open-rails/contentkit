package content

import (
	"context"
	"testing"

	"github.com/open-rails/contentkit/access"
)

type delayedReviewClassifier struct {
	fakeClassifier
	started chan struct{}
	release chan struct{}
}

func (c *delayedReviewClassifier) Classify(ctx context.Context, a Answer) (GroupAssignment, error) {
	if a.Text == "old" {
		close(c.started)
		<-c.release
	}
	return c.fakeClassifier.Classify(ctx, a)
}
func TestReviewC4DelayedClassifierCannotReplaceCurrentGroups(t *testing.T) {
	cl := &delayedReviewClassifier{started: make(chan struct{}), release: make(chan struct{})}
	_, p := newPollTest(t, Options{Classifier: cl})
	ctx := context.Background()
	poll, err := p.create(ctx, pollAdmin, freeTextPoll())
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := p.answer(ctx, access.Actor{ID: "user"}, poll.ID, "old"); done <- err }()
	<-cl.started
	newer, err := p.answer(ctx, access.Actor{ID: "user"}, poll.ID, "new")
	if err != nil {
		t.Fatal(err)
	}
	if newer.Groups[0].ID != "new" {
		t.Fatal("new answer not classified")
	}
	close(cl.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	got, err := p.get(ctx, access.Actor{ID: "user"}, poll.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.MyAnswer.Text != "new" || len(got.Groups) != 1 || got.Groups[0].ID != "new" {
		t.Fatalf("late old provider side effect won: source=%+v groups=%+v", got.MyAnswer, got.Groups)
	}
}

func TestReviewC4DeletedPollIsNotSentForClassification(t *testing.T) {
	cl := &fakeClassifier{failN: 1}
	rt, p := newPollTest(t, Options{Classifier: cl})
	ctx := context.Background()
	poll, err := p.create(ctx, pollAdmin, freeTextPoll())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.answer(ctx, access.Actor{ID: "user"}, poll.ID, "free-text answer"); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.store.pool.Exec(ctx, `UPDATE `+rt.store.t.pollQuestions+` SET deleted_at=now() WHERE id=$1`, poll.ID); err != nil {
		t.Fatal(err)
	}
	n, err := rt.ReclassifyPending(ctx, "", 10)
	if err != nil || n.Classified != 0 {
		t.Fatalf("deleted poll answer sent for classification: n=%+v err=%v", n, err)
	}
}
