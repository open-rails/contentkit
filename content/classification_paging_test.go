package content

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/open-rails/contentkit/access"
)

type failedAnswerClassifier struct{ fail string }

func (*failedAnswerClassifier) StatelessPolicy() {}
func (c *failedAnswerClassifier) Classify(_ context.Context, a Answer) (GroupAssignment, error) {
	if a.AnswerID == c.fail {
		return GroupAssignment{}, errors.New("this answer unavailable")
	}
	return GroupAssignment{GroupID: a.Text, Label: a.Text}, nil
}
func TestClassificationPageContinuesPastFailureAndRevisitsEdits(t *testing.T) {
	ctx := context.Background()
	rt, p := newPollTest(t, Options{Classifier: &fakeClassifier{failN: 10}})
	poll, err := p.create(ctx, pollAdmin, freeTextPoll())
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b"} {
		if _, err := p.answer(ctx, access.Actor{ID: id}, poll.ID, id); err != nil {
			t.Fatal(err)
		}
	}
	var first, actor string
	if err := rt.store.pool.QueryRow(ctx, `SELECT id::text,actor_id FROM `+rt.store.t.pollAnswers+` ORDER BY id LIMIT 1`).Scan(&first, &actor); err != nil {
		t.Fatal(err)
	}
	cl := &failedAnswerClassifier{fail: first}
	rt.classifier = cl
	page, err := rt.ReclassifyPending(ctx, "", 1)
	if err == nil || page.Next != first || page.Classified != 0 {
		t.Fatalf("first page=%+v %v", page, err)
	}
	next, err := rt.ReclassifyPending(ctx, page.Next, 1)
	if err != nil || next.Next != "" || next.Classified != 1 {
		t.Fatalf("healthy answer starved: %+v %v", next, err)
	}
	// A changed answer behind the cursor is reconsidered at the next sweep.
	if _, err := p.answer(ctx, access.Actor{ID: actor}, poll.ID, "changed"); err != nil {
		t.Fatal(err)
	}
	cl.fail = ""
	again, err := rt.ReclassifyPending(ctx, "", 1)
	if err != nil || again.Classified != 1 || again.Next != "" {
		t.Fatalf("new sweep missed change: %+v %v", again, err)
	}
}

type cursorProbe struct{}

func (cursorProbe) StatelessPolicy() {}
func (cursorProbe) Classify(_ context.Context, a Answer) (GroupAssignment, error) {
	return GroupAssignment{}, errors.New(a.AnswerID)
}
func TestClassificationCursorUsesUUIDValue(t *testing.T) {
	ctx := context.Background()
	rt, p := newPollTest(t, Options{Classifier: cursorProbe{}})
	poll, err := p.create(ctx, pollAdmin, freeTextPoll())
	if err != nil {
		t.Fatal(err)
	}
	first := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	second := "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	for i, id := range []string{first, second} {
		if _, err := rt.store.pool.Exec(ctx, `INSERT INTO `+rt.store.t.pollAnswers+` (id,tenant_id,question_id,actor_id,text) VALUES ($1,$2,$3,$4,'text')`, id, rt.tenant, poll.ID, fmt.Sprint(i)); err != nil {
			t.Fatal(err)
		}
	}
	for _, cursor := range []string{first, strings.ToUpper(first)} {
		page, err := rt.ReclassifyPending(ctx, cursor, 1)
		if err == nil || err.Error() != second || page.Next != "" {
			t.Fatalf("cursor %s has different UUID semantics: %+v %v", cursor, page, err)
		}
	}
}
