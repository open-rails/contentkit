package content

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// retainingPolicy models external durable storage and terminal fencing, with
// an explicitly paused completion and an outage before erasure acceptance.
type retainingPolicy struct {
	mu       sync.Mutex
	fenced   map[string]bool
	retained map[string]string
	down     bool
	entered  chan struct{}
	release  chan struct{}
}

func (p *retainingPolicy) store(tenant, subject, text string) error {
	if p.entered != nil && text == "paused" {
		close(p.entered)
		<-p.release
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	key := tenant + "/" + subject
	if p.fenced[key] {
		return ErrSubjectErased
	}
	if p.retained == nil {
		p.retained = map[string]string{}
	}
	p.retained[key] = text
	return nil
}
func (p *retainingPolicy) Screen(_ context.Context, in ModerationInput) (Verdict, error) {
	if err := p.store(in.Tenant, in.SubjectID, in.Text); err != nil {
		return Verdict{}, err
	}
	return Verdict{Decision: DecisionReview}, nil
}
func (p *retainingPolicy) Classify(_ context.Context, a Answer) (GroupAssignment, error) {
	if err := p.store(a.Tenant, a.SubjectID, a.Text); err != nil {
		return GroupAssignment{}, err
	}
	return GroupAssignment{GroupID: a.Text, Label: a.Text}, nil
}
func (p *retainingPolicy) EraseSubjects(_ context.Context, tenant string, ids []string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.down {
		return errors.New("provider unavailable")
	}
	if p.fenced == nil {
		p.fenced = map[string]bool{}
	}
	for _, id := range ids {
		key := tenant + "/" + id
		p.fenced[key] = true
		delete(p.retained, key)
	}
	return nil
}

func TestPrivateErasureRejectsUnconfiguredRetainingPolicy(t *testing.T) {
	rt, pool := newTestRuntime(t, Options{})
	_, err := New(context.Background(), Options{Pool: pool, Schema: rt.schema, Tenant: testTenant, Identity: &fakeIdentity{}, Authz: allowAll{}, Resolver: &fakeResolver{}, Classifier: &retainingPolicy{}})
	if err == nil {
		t.Fatal("retaining classifier accepted without lifecycle eraser")
	}
}

func TestPrivateErasureRetriesProviderAfterDurableSourceFence(t *testing.T) {
	ctx := context.Background()
	provider := &retainingPolicy{}
	rt, p := newPollTest(t, Options{Classifier: provider, PrivateDataEraser: provider})
	poll, err := p.create(ctx, pollAdmin, freeTextPoll())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.answer(ctx, Actor{ID: "u1"}, poll.ID, "personal"); err != nil {
		t.Fatal(err)
	}
	provider.down = true
	if err := rt.EraseSubjects(ctx, []string{"u1"}); err == nil {
		t.Fatal("provider outage incorrectly completed deletion")
	}
	var n int
	if err := rt.store.pool.QueryRow(ctx, `SELECT count(*) FROM `+rt.store.t.pollAnswers).Scan(&n); err != nil || n != 0 {
		t.Fatalf("source not durably removed: %d %v", n, err)
	}
	if _, err := p.answer(ctx, Actor{ID: "u1"}, poll.ID, "new"); !errors.Is(err, ErrSubjectErased) {
		t.Fatalf("source fence lost: %v", err)
	}
	provider.down = false
	if err := rt.EraseSubjects(ctx, []string{"u1"}); err != nil {
		t.Fatal(err)
	}
	if len(provider.retained) != 0 || !provider.fenced[testTenant+"/u1"] {
		t.Fatal("provider retry did not complete fenced erasure")
	}
}

func TestPrivateErasureFencesPausedClassifierCompletion(t *testing.T) {
	ctx := context.Background()
	provider := &retainingPolicy{entered: make(chan struct{}), release: make(chan struct{})}
	rt, p := newPollTest(t, Options{Classifier: provider, PrivateDataEraser: provider})
	poll, err := p.create(ctx, pollAdmin, freeTextPoll())
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := p.answer(ctx, Actor{ID: "u1"}, poll.ID, "paused"); done <- err }()
	<-provider.entered
	if err := rt.EraseSubjects(ctx, []string{"u1"}); err != nil {
		t.Fatal(err)
	}
	close(provider.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	got, err := p.get(ctx, Actor{ID: "u1"}, poll.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.AnswerCount != 0 || len(got.Groups) != 0 || len(provider.retained) != 0 {
		t.Fatalf("paused classifier resurrected erased data: %+v", got)
	}
}

func TestPrivateErasureFencesPausedModerationBeforeSourceCommit(t *testing.T) {
	ctx := context.Background()
	provider := &retainingPolicy{entered: make(chan struct{}), release: make(chan struct{})}
	res := &fakeResolver{}
	res.set("gallery", "1", true, true)
	rt, _ := newTestRuntime(t, Options{Moderator: provider, PrivateDataEraser: provider, Resolver: res, ContentKinds: []string{"gallery"}})
	done := make(chan error, 1)
	go func() {
		_, err := rt.comments.create(ctx, Actor{ID: "u1"}, "gallery", "1", createInput{Body: "paused"})
		done <- err
	}()
	<-provider.entered
	if err := rt.EraseSubjects(ctx, []string{"u1"}); err != nil {
		t.Fatal(err)
	}
	close(provider.release)
	if err := <-done; !errors.Is(err, ErrSubjectErased) {
		t.Fatalf("source write crossed erase fence: %v", err)
	}
	var n int
	if err := rt.store.pool.QueryRow(ctx, `SELECT count(*) FROM `+rt.store.t.comments).Scan(&n); err != nil || n != 0 {
		t.Fatalf("late moderation stored source: %d %v", n, err)
	}
	if len(provider.retained) != 0 {
		t.Fatal("late moderator retained erased personal data")
	}
}

func TestPrivateErasurePreservesPublicationRepliesAndOtherTenant(t *testing.T) {
	ctx := context.Background()
	rt := moderatedRuntime(t, &fakeModerator{})
	author := Actor{ID: "u1"}
	sibling := Actor{ID: "u2"}
	published := mustComment(t, rt, author, "gallery", "1", createInput{Body: "published original"})
	reply := mustComment(t, rt, sibling, "gallery", "1", createInput{ReplyToID: published.ID, Body: "sibling reply"})
	private := mustComment(t, rt, author, "gallery", "1", createInput{Body: "iffy never published"})
	if _, err := rt.comments.edit(ctx, author, published.ID, "iffy private replacement"); err != nil {
		t.Fatal(err)
	}
	_, err := rt.store.pool.Exec(ctx, `INSERT INTO `+rt.store.t.comments+` (tenant_id,content_kind,content_id,user_id,body,moderation) VALUES ('other','gallery','1','u1','other tenant private','held')`)
	if err != nil {
		t.Fatal(err)
	}
	if err := rt.EraseSubjects(ctx, []string{"u1"}); err != nil {
		t.Fatal(err)
	}
	var body string
	var deleted bool
	if err := rt.store.pool.QueryRow(ctx, `SELECT body,deleted_at IS NOT NULL FROM `+rt.store.t.comments+` WHERE id=$1`, published.ID).Scan(&body, &deleted); err != nil || body != "published original" || deleted {
		t.Fatalf("lost prior publication: %q deleted=%v %v", body, deleted, err)
	}
	if err := rt.store.pool.QueryRow(ctx, `SELECT body FROM `+rt.store.t.comments+` WHERE id=$1`, private.ID).Scan(&body); err != nil || body != "" {
		t.Fatalf("private content retained: %q %v", body, err)
	}
	if err := rt.store.pool.QueryRow(ctx, `SELECT body FROM `+rt.store.t.comments+` WHERE id=$1`, reply.ID).Scan(&body); err != nil || body != "sibling reply" {
		t.Fatalf("sibling reply damaged: %q %v", body, err)
	}
	if err := rt.store.pool.QueryRow(ctx, `SELECT body FROM `+rt.store.t.comments+` WHERE tenant_id='other'`).Scan(&body); err != nil || body != "other tenant private" {
		t.Fatalf("foreign tenant erased: %q %v", body, err)
	}
	if _, err := rt.comments.edit(ctx, author, published.ID, "try again"); !errors.Is(err, ErrSubjectErased) {
		t.Fatalf("erased author edited again: %v", err)
	}
}

func TestPrivateErasurePreservesPublishedPostSnapshot(t *testing.T) {
	ctx := context.Background()
	rt := moderatedRuntime(t, &fakeModerator{})
	author := Actor{ID: "reviewer"}
	rec := doJSON(t, rt.Handler(), author, "POST", "/posts", postWriteReq{Title: ptr("published"), Body: ptr("public body"), IsDraft: ptr(false)})
	if rec.Code != 201 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	post := decodePost(t, rec)
	rec = doJSON(t, rt.Handler(), author, "PATCH", "/posts/"+post.ID, postWriteReq{Body: ptr("iffy private edit")})
	if rec.Code != 202 {
		t.Fatalf("hold edit: %d %s", rec.Code, rec.Body.String())
	}
	if err := rt.EraseSubjects(ctx, []string{author.ID}); err != nil {
		t.Fatal(err)
	}
	var body, title, state string
	var deleted bool
	if err := rt.store.pool.QueryRow(ctx, `SELECT title,body,moderation,deleted_at IS NOT NULL FROM `+rt.store.t.posts+` WHERE id=$1`, post.ID).Scan(&title, &body, &state, &deleted); err != nil {
		t.Fatal(err)
	}
	if title != "published" || body != "public body" || state != ModerationRejected || deleted {
		t.Fatalf("post cleanup lost publication or republished private edit: %q %q %s %v", title, body, state, deleted)
	}
}

func TestBasicModeratorPrivateCacheUsesTypedIdentityAndErasure(t *testing.T) {
	m := &BasicModerator{}
	ctx := context.Background()
	for _, in := range []ModerationInput{{Tenant: "a", Actor: Actor{ID: "b|c"}, Text: "same"}, {Tenant: "a|b", Actor: Actor{ID: "c"}, Text: "same"}} {
		v, err := m.Screen(ctx, in)
		if err != nil || v.Decision != DecisionApprove {
			t.Fatalf("opaque identity collision: %+v %v", v, err)
		}
	}
	if err := m.EraseSubjects(ctx, "a", []string{"b|c"}); err != nil {
		t.Fatal(err)
	}
	if len(m.recent) != 1 {
		t.Fatalf("private cache cleanup crossed identities: %d", len(m.recent))
	}
	if _, err := m.Screen(ctx, ModerationInput{Tenant: "a", Actor: Actor{ID: "b|c"}, Text: "late"}); !errors.Is(err, ErrSubjectErased) {
		t.Fatalf("late completion recreated erased cache: %v", err)
	}
}

func TestPrivateFenceSurvivesRuntimeRestartAndReview(t *testing.T) {
	ctx := context.Background()
	rt := moderatedRuntime(t, &fakeModerator{})
	author := Actor{ID: "u1"}
	cm := mustComment(t, rt, author, "gallery", "1", createInput{Body: "iffy held"})
	page, err := rt.ListHeld(ctx, KindComment, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if err := rt.EraseSubjects(ctx, []string{author.ID}); err != nil {
		t.Fatal(err)
	}
	opts := Options{Pool: rt.store.pool, Schema: rt.schema, Tenant: rt.tenant, Identity: &fakeIdentity{}, Authz: allowAll{}, Resolver: rt.resolver, ContentKinds: []string{"gallery"}, Moderator: &fakeModerator{}}
	restarted, err := New(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.comments.create(ctx, author, "gallery", "1", createInput{Body: "new private body"}); !errors.Is(err, ErrSubjectErased) {
		t.Fatalf("new runtime forgot source fence: %v", err)
	}
	if err := restarted.Resolve(ctx, KindComment, cm.ID, ReviewDecision{Revision: page.Items[0].Revision, Decision: DecisionApprove, Reviewer: "reviewer"}); err == nil {
		t.Fatal("review captured before erasure republished cleaned source")
	}
	opts.Tenant = "other"
	other, err := New(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.comments.create(ctx, author, "gallery", "1", createInput{Body: "other tenant content"}); err != nil {
		t.Fatalf("source fence crossed tenant runtime: %v", err)
	}
}
