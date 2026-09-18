package content

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeClassifier groups an answer by its first word and owns the assignments
// per (tenant, question), like a real classifier that may re-cluster. failN
// makes the next N Classify calls fail.
type fakeClassifier struct {
	mu         sync.Mutex
	failN      int
	assigned   map[string]map[string]string // tenant|question -> answer id -> group id
	groupCalls []string                     // "tenant|question" per Groups call
}

func (c *fakeClassifier) Classify(_ context.Context, a Answer) (GroupAssignment, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failN > 0 {
		c.failN--
		return GroupAssignment{}, errors.New("classifier down")
	}
	word := strings.ToLower(strings.Fields(a.Text)[0])
	if c.assigned == nil {
		c.assigned = map[string]map[string]string{}
	}
	k := a.Tenant + "|" + a.QuestionID
	if c.assigned[k] == nil {
		c.assigned[k] = map[string]string{}
	}
	c.assigned[k][a.AnswerID] = word
	return GroupAssignment{GroupID: word, Label: strings.ToUpper(word[:1]) + word[1:]}, nil
}

func (c *fakeClassifier) Groups(_ context.Context, tenant, questionID string) ([]Group, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	k := tenant + "|" + questionID
	c.groupCalls = append(c.groupCalls, k)
	counts := map[string]int{}
	for _, g := range c.assigned[k] {
		counts[g]++
	}
	var out []Group
	for id, n := range counts { // map order: ContentKit must sort
		out = append(out, Group{ID: id, Label: strings.ToUpper(id[:1]) + id[1:], Count: n})
	}
	return out, nil
}

func freeTextPoll() createPollInput {
	return createPollInput{Kind: PollFreeText, Question: "Favorite animal?", Language: "en"}
}

func TestPolls_FreeTextRefusedWithoutClassifier(t *testing.T) {
	rt, p := newPollTest(t, Options{})
	ctx := context.Background()
	if _, err := p.create(ctx, pollAdmin, freeTextPoll()); !errors.Is(err, ErrNoClassifier) {
		t.Fatalf("free-text poll without a classifier: %v, want ErrNoClassifier", err)
	}
	if rec := doPollReq(t, rt.Handler(), "POST", "/polls", freeTextPoll(), pollAdmin); rec.Code != http.StatusNotImplemented {
		t.Fatalf("HTTP = %d %s, want 501", rec.Code, rec.Body.String())
	}
	if _, err := p.create(ctx, pollAdmin, twoOptionPoll("en")); err != nil {
		t.Fatalf("multiple-choice still creates: %v", err)
	}
	if lst, _ := p.list(ctx, pollAdmin, listFilter{limit: 10}); len(lst) != 1 || lst[0].Kind != PollMultipleChoice {
		t.Fatalf("list = %+v", lst)
	}
}

func TestPolls_FreeTextAnswersOnePerActorEditableUntilClose(t *testing.T) {
	cl := &fakeClassifier{}
	rt, p := newPollTest(t, Options{Classifier: cl})
	ctx := context.Background()
	u1, u2, anon := Actor{ID: "u1"}, Actor{ID: "u2"}, Actor{Anonymous: true, IP: "1.1.1.1"}

	bad := freeTextPoll()
	bad.Options = []createOptionInput{{Label: "x"}, {Label: "y"}}
	if _, err := p.create(ctx, pollAdmin, bad); err == nil {
		t.Fatal("free-text poll with options accepted")
	}
	if _, err := p.create(ctx, pollAdmin, createPollInput{Kind: "essay", Question: "?"}); err == nil {
		t.Fatal("unknown kind accepted")
	}
	poll, err := p.create(ctx, pollAdmin, freeTextPoll())
	if err != nil || poll.Kind != PollFreeText || len(poll.Options) != 0 || poll.Closed {
		t.Fatalf("create = %+v err=%v", poll, err)
	}
	mc, _ := p.create(ctx, pollAdmin, twoOptionPoll("en"))

	if _, err := p.answer(ctx, anon, poll.ID, "cats"); !errors.Is(err, errUnauthorized) {
		t.Fatalf("anonymous answer: %v, want 401", err)
	}
	if _, err := p.answer(ctx, u1, poll.ID, "   "); err == nil {
		t.Fatal("blank answer accepted")
	}
	if _, err := p.answer(ctx, u1, poll.ID, strings.Repeat("x", maxAnswerLen+1)); err == nil {
		t.Fatal("oversized answer accepted")
	}
	v, err := p.answer(ctx, u1, poll.ID, "cats, obviously")
	if err != nil || v.AnswerCount != 1 || v.MyAnswer == nil || v.MyAnswer.Text != "cats, obviously" || !v.MyAnswer.Classified {
		t.Fatalf("first answer = %+v err=%v", v, err)
	}
	first := v.MyAnswer.ID
	v, err = p.answer(ctx, u1, poll.ID, "dogs, changed my mind")
	if err != nil || v.AnswerCount != 1 || v.MyAnswer.ID != first || v.MyAnswer.Text != "dogs, changed my mind" || !v.MyAnswer.Classified {
		t.Fatalf("edited answer = %+v err=%v", v, err)
	}
	if v, _ = p.answer(ctx, u2, poll.ID, "birds"); v.AnswerCount != 2 || v.MyAnswer.Text != "birds" {
		t.Fatalf("second actor = %+v", v)
	}
	if got, _ := p.get(ctx, u1, poll.ID); got.MyAnswer == nil || got.MyAnswer.Text != "dogs, changed my mind" {
		t.Fatalf("u1 view = %+v", got)
	}
	if got, _ := p.get(ctx, anon, poll.ID); got.MyAnswer != nil || got.AnswerCount != 2 {
		t.Fatalf("anonymous view = %+v", got)
	}
	// kind mismatches
	if _, err := p.vote(ctx, u1, poll.ID, mc.Options[0].ID); err == nil {
		t.Fatal("vote on a free-text poll accepted")
	}
	if _, err := p.answer(ctx, u1, mc.ID, "text"); err == nil {
		t.Fatal("answer on a multiple-choice poll accepted")
	}
	// close by closes_at: no more answers or edits, results still readable
	past := time.Now().Add(-time.Minute)
	if _, err := p.update(ctx, pollAdmin, poll.ID, updatePollInput{ClosesAt: &past}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.answer(ctx, u1, poll.ID, "too late"); err == nil {
		t.Fatal("answer after close accepted")
	}
	if _, err := p.answer(ctx, Actor{ID: "u3"}, poll.ID, "too late"); err == nil {
		t.Fatal("new answer after close accepted")
	}
	got, _ := p.get(ctx, u1, poll.ID)
	if !got.Closed || got.ClosesAt == nil || got.AnswerCount != 2 || got.MyAnswer.Text != "dogs, changed my mind" {
		t.Fatalf("closed view = %+v", got)
	}
	// close by deactivation, for votes too
	off := false
	if _, err := p.update(ctx, pollAdmin, mc.ID, updatePollInput{IsActive: &off}); err != nil {
		t.Fatal(err)
	}
	var he httpError
	if _, err := p.vote(ctx, u1, mc.ID, mc.Options[0].ID); !errors.As(err, &he) || he.status != http.StatusBadRequest {
		t.Fatalf("vote on a deactivated poll: %v, want 400", err)
	}
	if got, _ := p.get(ctx, u1, mc.ID); !got.Closed {
		t.Fatal("deactivated poll not reported closed")
	}
	// a scheduled close in the future keeps it open
	future := time.Now().Add(time.Hour)
	in := freeTextPoll()
	in.ClosesAt = &future
	open, err := p.create(ctx, pollAdmin, in)
	if err != nil || open.Closed || open.ClosesAt == nil {
		t.Fatalf("scheduled close = %+v err=%v", open, err)
	}
	if _, err := p.answer(ctx, u1, open.ID, "still open"); err != nil {
		t.Fatal(err)
	}
	in.LiveAt, in.ClosesAt = &future, &past
	if _, err := p.create(ctx, pollAdmin, in); err == nil {
		t.Fatal("closes_at before live_at accepted")
	}
	// options cannot be added to a free-text poll
	if rec := doPollReq(t, rt.Handler(), "POST", "/polls/"+open.ID+"/options", map[string]string{"label": "x"}, pollAdmin); rec.Code != http.StatusBadRequest {
		t.Fatalf("add option to a free-text poll = %d, want 400", rec.Code)
	}
}

func TestPolls_FreeTextResultsDeterministic(t *testing.T) {
	cl := &fakeClassifier{}
	rt, p := newPollTest(t, Options{Classifier: cl})
	ctx := context.Background()
	poll, err := p.create(ctx, pollAdmin, freeTextPoll())
	if err != nil {
		t.Fatal(err)
	}
	for i, text := range []string{"cats rule", "Cats forever", "dogs", "birds", "dogs are great"} {
		if _, err := p.answer(ctx, Actor{ID: fmt.Sprintf("u%d", i)}, poll.ID, text); err != nil {
			t.Fatal(err)
		}
	}
	want := `[{"id":"cats","label":"Cats","count":2},{"id":"dogs","label":"Dogs","count":2},{"id":"birds","label":"Birds","count":1}]`
	for i := 0; i < 5; i++ { // classifier map order never leaks
		v, err := p.get(ctx, Actor{ID: "u0"}, poll.ID)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := json.Marshal(v.Groups)
		if string(got) != want || v.AnswerCount != 5 || v.MyAnswer == nil || v.MyAnswer.Text != "cats rule" || v.GroupsUnavailable {
			t.Fatalf("results #%d = %s (count %d, mine %+v), want %s", i, got, v.AnswerCount, v.MyAnswer, want)
		}
	}
	// over HTTP, for a reader who did not answer
	rec := doPollReq(t, rt.Handler(), "GET", "/polls/"+poll.ID, nil, Actor{ID: "lurker"})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), want) || strings.Contains(rec.Body.String(), `"my_answer"`) || !strings.Contains(rec.Body.String(), `"answer_count":5`) {
		t.Fatalf("GET = %d %s", rec.Code, rec.Body.String())
	}
	rec = doPollReq(t, rt.Handler(), "POST", "/polls/"+poll.ID+"/answer", map[string]string{"text": "cats via http"}, Actor{ID: "lurker"})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"text":"cats via http"`) || !strings.Contains(rec.Body.String(), `{"id":"cats","label":"Cats","count":3}`) {
		t.Fatalf("POST answer = %d %s", rec.Code, rec.Body.String())
	}
	if rec = doPollReq(t, rt.Handler(), "POST", "/polls/"+poll.ID+"/answer", map[string]string{"text": "x"}, Actor{Anonymous: true, IP: "2.2.2.2"}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous POST answer = %d, want 401", rec.Code)
	}
	// the list view carries the same results
	lst, _ := p.list(ctx, Actor{ID: "u0"}, listFilter{limit: 10})
	if len(lst) != 1 || len(lst[0].Groups) != 3 || lst[0].Groups[0].Count != 3 || lst[0].MyAnswer == nil {
		t.Fatalf("list = %+v", lst)
	}
}

func TestPolls_FreeTextClassifierFailureRetry(t *testing.T) {
	cl := &fakeClassifier{failN: 1}
	rt, p := newPollTest(t, Options{Classifier: cl})
	ctx := context.Background()
	poll, _ := p.create(ctx, pollAdmin, freeTextPoll())
	u := Actor{ID: "u1"}
	v, err := p.answer(ctx, u, poll.ID, "cats")
	if err != nil || v.AnswerCount != 1 || v.MyAnswer == nil || v.MyAnswer.Classified || len(v.Groups) != 0 {
		t.Fatalf("answer under a failing classifier = %+v err=%v, want kept and unclassified", v, err)
	}
	n, err := rt.ReclassifyPending(ctx, 10)
	if err != nil || n != 1 {
		t.Fatalf("ReclassifyPending = %d, %v", n, err)
	}
	if v, _ = p.get(ctx, u, poll.ID); !v.MyAnswer.Classified || len(v.Groups) != 1 || v.Groups[0].ID != "cats" {
		t.Fatalf("after retry = %+v", v)
	}
	if n, err = rt.ReclassifyPending(ctx, 10); err != nil || n != 0 {
		t.Fatalf("second ReclassifyPending = %d, %v", n, err)
	}
	// an edit reclassifies; a failing retry reports the error and keeps waiting
	cl.failN = 2
	if v, _ = p.answer(ctx, u, poll.ID, "dogs"); v.MyAnswer.Classified {
		t.Fatal("edited answer kept a stale classification")
	}
	if n, err = rt.ReclassifyPending(ctx, 10); err == nil || n != 0 {
		t.Fatalf("failing retry = %d, %v, want the error", n, err)
	}
	if n, err = rt.ReclassifyPending(ctx, 10); err != nil || n != 1 {
		t.Fatalf("recovered retry = %d, %v", n, err)
	}
	if v, _ = p.get(ctx, u, poll.ID); len(v.Groups) != 1 || v.Groups[0].ID != "dogs" {
		t.Fatalf("after edit and retry = %+v", v.Groups)
	}
}

func TestPolls_FreeTextTenantIsolation(t *testing.T) {
	ctx := context.Background()
	cl := &fakeClassifier{}
	a, pool := newTestRuntime(t, Options{Tenant: "site_a", Classifier: cl, Perms: Perms{PollWrite: pollWritePerm}})
	b, err := New(ctx, Options{Pool: pool, Schema: a.schema, Tenant: "site_b", Identity: &fakeIdentity{}, Authz: allowAll{}, Resolver: &fakeResolver{}, Classifier: cl, Perms: Perms{PollWrite: pollWritePerm}})
	if err != nil {
		t.Fatal(err)
	}
	u := Actor{ID: "shared-account"}
	poll, err := a.polls.create(ctx, pollAdmin, freeTextPoll())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.polls.answer(ctx, u, poll.ID, "cats"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.polls.answer(ctx, u, poll.ID, "dogs"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("tenant b answered a's poll: %v", err)
	}
	if _, err := b.polls.get(ctx, u, poll.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("tenant b read a's poll: %v", err)
	}
	if n, _ := b.ReclassifyPending(ctx, 10); n != 0 {
		t.Fatalf("tenant b reclassified a's answers: %d", n)
	}
	v, _ := a.polls.get(ctx, u, poll.ID)
	if v.AnswerCount != 1 || v.MyAnswer == nil || len(v.Groups) != 1 {
		t.Fatalf("tenant a view = %+v", v)
	}
	for _, call := range cl.groupCalls {
		if !strings.HasPrefix(call, "site_a|") {
			t.Fatalf("classifier asked for groups of %q", call)
		}
	}
}
