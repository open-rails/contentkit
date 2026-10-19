package content

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/internal/testauth"
	"github.com/open-rails/contentkit/migrations"
)

func TestAccountErasureOwnsSourceInteractionsAndExactCounters(t *testing.T) {
	ctx := context.Background()
	rt := newPreferenceRuntime(t)
	u1, u2, anon := access.Actor{ID: "erase"}, access.Actor{ID: "keep"}, access.Actor{ID: "erase", Anonymous: true, IP: "erase"}
	target := ref("gallery", cid(42))
	for _, a := range []access.Actor{u1, u2, anon} {
		mustReact(t, rt, a, "gallery", localeID(42, "en"), 1)
	}
	for _, a := range []access.Actor{u1, u2} {
		mustFavorite(t, rt, a, "gallery", localeID(42, "en"), true)
	}
	cm := mustComment(t, rt, u2, "gallery", localeID(42, "en"), CommentInput{Body: "keep author's comment"})
	for _, a := range []access.Actor{u1, u2, anon} {
		if _, err := rt.comments.reactTx(ctx, a, cm.ID, 1); err != nil {
			t.Fatal(err)
		}
	}
	poll, err := rt.polls.create(ctx, pollAdmin, PollInput{Question: "q", Options: []PollOptionInput{{Label: "a"}, {Label: "b"}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range []access.Actor{u1, u2, anon} {
		if _, err := rt.polls.vote(ctx, a, poll.ID, poll.Options[0].ID); err != nil {
			t.Fatal(err)
		}
	}
	other, err := New(ctx, Options{Pool: rt.store.pool, Schema: rt.schema, Tenant: "other", Auth: testauth.Auth{Authz: allowAll{}}, Resolver: rt.resolver, Kinds: testKinds("gallery")})
	if err != nil {
		t.Fatal(err)
	}
	mustReact(t, other, u1, "gallery", localeID(42, "en"), 1)
	for i := 0; i < 2; i++ {
		if err := rt.EraseSubjects(ctx, []string{u1.ID}); err != nil {
			t.Fatal(err)
		}
		got := countsOf(t, rt, target)
		if got.Likes != 2 || got.Favorites != 1 {
			t.Fatalf("source counters after erase%d: %+v", i, got)
		}
		var likes int
		for _, q := range []string{`SELECT likes FROM ` + rt.store.t.comments + ` WHERE id='` + cm.ID + `'`, `SELECT vote_count FROM ` + rt.store.t.pollOptions + ` WHERE id='` + poll.Options[0].ID + `'`} {
			if err := rt.store.pool.QueryRow(ctx, q).Scan(&likes); err != nil || likes != 2 {
				t.Fatalf("own counter=%d err=%v", likes, err)
			}
		}
		for _, table := range []string{rt.store.t.reactions, rt.store.t.favorites} {
			var n int
			if err := rt.store.pool.QueryRow(ctx, `SELECT count(*) FROM `+table+` WHERE tenant_id=$1 AND user_id=$2`, rt.tenant, u1.ID).Scan(&n); err != nil || n != 0 {
				t.Fatalf("authenticated source retained: %d %v", n, err)
			}
		}
	}
	if got := countsOf(t, other, other.Ref("gallery", cid(42))); got.Likes != 1 {
		t.Fatalf("other tenant damaged: %+v", got)
	}
	for name, write := range map[string]func() error{
		"reaction":         func() error { _, e := rt.reactions.react(ctx, u1, rt.Ref("gallery", localeID(42, "en")), 1); return e },
		"favorite":         func() error { return favErr(rt.favorites.add(ctx, u1, rt.Ref("gallery", localeID(42, "en")))) },
		"unfavorite":       func() error { return favErr(rt.favorites.remove(ctx, u1, rt.Ref("gallery", localeID(42, "en")))) },
		"comment reaction": func() error { _, e := rt.comments.reactTx(ctx, u1, cm.ID, 1); return e },
		"poll vote":        func() error { _, e := rt.polls.vote(ctx, u1, poll.ID, poll.Options[0].ID); return e },
	} {
		if err := write(); !errors.Is(err, ErrSubjectErased) {
			t.Fatalf("%s crossed source fence: %v", name, err)
		}
	}
	for _, got := range exported(t, rt) {
		if got.ActorID != u2.ID {
			t.Fatalf("erased preference still exports: %+v", got)
		}
	}
}

func TestSourceWritesRaceErasureWithoutResurrection(t *testing.T) {
	ctx := context.Background()
	rt := newPreferenceRuntime(t)
	actor := access.Actor{ID: "racer"}
	poll, err := rt.polls.create(ctx, pollAdmin, PollInput{Question: "q", Options: []PollOptionInput{{Label: "a"}, {Label: "b"}}})
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	errs := make(chan error, 25)
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			var err error
			switch i % 3 {
			case 0:
				_, err = rt.reactions.react(ctx, actor, rt.Ref("gallery", localeID(42, "en")), 1)
			case 1:
				err = favErr(rt.favorites.add(ctx, actor, rt.Ref("gallery", localeID(42, "en"))))
			case 2:
				_, err = rt.polls.vote(ctx, actor, poll.ID, poll.Options[0].ID)
			}
			errs <- err
		}(i)
	}
	wg.Add(1)
	go func() { defer wg.Done(); <-start; errs <- rt.EraseSubjects(ctx, []string{actor.ID}) }()
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil && !errors.Is(err, ErrSubjectErased) {
			t.Fatal(err)
		}
	}
	for _, table := range []string{rt.store.t.reactions, rt.store.t.favorites, rt.store.t.pollVotes} {
		var n int
		if err := rt.store.pool.QueryRow(ctx, `SELECT count(*) FROM `+table+` WHERE user_id=$1`, actor.ID).Scan(&n); err != nil || n != 0 {
			t.Fatalf("race recreated %s: %d %v", table, n, err)
		}
	}
	if got := countsOf(t, rt, ref("gallery", cid(42))); got.Likes != 0 || got.Favorites != 0 {
		t.Fatalf("racing source counts retained: %+v", got)
	}
}

func TestConcurrentErasedActorsShareCountersWithoutDeadlock(t *testing.T) {
	ctx := context.Background()
	rt := newPreferenceRuntime(t)
	for _, actor := range []access.Actor{{ID: "a"}, {ID: "b"}, {ID: "keep"}} {
		for _, id := range []string{localeID(42, "en"), localeID(7, "en")} {
			mustReact(t, rt, actor, "gallery", id, 1)
			mustFavorite(t, rt, actor, "gallery", id, true)
		}
	}
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, id := range []string{"a", "b"} {
		wg.Add(1)
		go func(id string) { defer wg.Done(); errs <- rt.EraseSubjects(ctx, []string{id}) }(id)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{cid(42), cid(7)} {
		got := countsOf(t, rt, ref("gallery", id))
		if got.Likes != 1 || got.Favorites != 1 {
			t.Fatalf("shared counters %s=%+v", id, got)
		}
	}
}

func TestRestoreReappliesSourceErasureAndFencedRowsNeverExport(t *testing.T) {
	ctx := context.Background()
	rt := newPreferenceRuntime(t)
	actor := access.Actor{ID: "gone"}
	mustReact(t, rt, actor, "gallery", localeID(42, "en"), 1)
	mustReact(t, rt, access.Actor{ID: "keep"}, "gallery", localeID(42, "en"), 1)
	mustFavorite(t, rt, access.Actor{ID: "keep"}, "gallery", localeID(42, "en"), true)
	if err := rt.EraseSubjects(ctx, []string{actor.ID}); err != nil {
		t.Fatal(err)
	}
	// Privileged restore loads old source/counters but retains the permanent fence.
	if _, err := rt.store.pool.Exec(ctx, `INSERT INTO `+rt.store.t.reactions+` (`+keyCols+`,user_id,value,revision) VALUES ($1,'gallery','`+cid(42)+`',NULL,'gone',1,`+rt.store.nextRevision()+`)`, rt.tenant); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.store.pool.Exec(ctx, `INSERT INTO `+rt.store.t.favorites+` (`+keyCols+`,user_id,value,revision) VALUES ($1,'gallery','`+cid(42)+`',NULL,'gone',0,`+rt.store.nextRevision()+`)`, rt.tenant); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.store.pool.Exec(ctx, `UPDATE `+rt.store.t.counts+` SET likes=likes+1 WHERE tenant_id=$1 AND content_kind='gallery' AND content_id='`+cid(42)+`'`, rt.tenant); err != nil {
		t.Fatal(err)
	}
	for _, p := range exported(t, rt) {
		if p.ActorID == actor.ID {
			t.Fatal("restored fenced row became exportable")
		}
	}
	if err := rt.EraseSubjects(ctx, []string{actor.ID}); err != nil {
		t.Fatal(err)
	}
	if got := countsOf(t, rt, ref("gallery", cid(42))); got.Likes != 1 || got.Favorites != 1 {
		t.Fatalf("recovery miscounted erased interactions: %+v", got)
	}
}

func TestConcurrentErasureWithCrossAuthoredReactionTargets(t *testing.T) {
	ctx := context.Background()
	rt := moderatedRuntime(t, &fakeModerator{})
	a, b := access.Actor{ID: "a"}, access.Actor{ID: "b"}
	ca := mustComment(t, rt, a, "gallery", cid(1), CommentInput{Body: "published a"})
	cb := mustComment(t, rt, b, "gallery", cid(1), CommentInput{Body: "published b"})
	if _, err := rt.comments.reactTx(ctx, a, cb.ID, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.comments.reactTx(ctx, b, ca.ID, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.comments.edit(ctx, a, ca.ID, "iffy a unpublished"); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.comments.edit(ctx, b, cb.ID, "iffy b unpublished"); err != nil {
		t.Fatal(err)
	}
	errs := make(chan error, 2)
	go func() { errs <- rt.EraseSubjects(ctx, []string{a.ID}) }()
	go func() { errs <- rt.EraseSubjects(ctx, []string{b.ID}) }()
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	var n int
	if err := rt.store.pool.QueryRow(ctx, `SELECT count(*) FROM `+rt.store.t.comments+` WHERE likes<>0 OR body LIKE 'iffy%'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("cross erasure retained unpublished bodies/counters: %d %v", n, err)
	}
}

func TestInteractionErasureDoesNotRecreateRemovedRollups(t *testing.T) {
	ctx := context.Background()
	rt := newPreferenceRuntime(t)
	mustReact(t, rt, access.Actor{ID: "gone"}, "gallery", localeID(42, "en"), 1)
	if _, err := rt.store.pool.Exec(ctx, `DELETE FROM `+rt.store.t.counts); err != nil {
		t.Fatal(err)
	}
	if err := rt.EraseSubjects(ctx, []string{"gone"}); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := rt.store.pool.QueryRow(ctx, `SELECT count(*) FROM `+rt.store.t.counts).Scan(&n); err != nil || n != 0 {
		t.Fatalf("erasure created ghost rollup: %d %v", n, err)
	}
}

func TestSourceFenceSeesCommittedErasureWithRepeatableReadHostDefault(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	base := newPreferenceRuntime(t)
	cfg := base.store.pool.Config()
	cfg.ConnConfig.RuntimeParams["default_transaction_isolation"] = "repeatable read"
	cfg.ConnConfig.RuntimeParams["application_name"] = "fence_" + base.schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	rt, err := New(ctx, Options{Pool: pool, Schema: base.schema, Tenant: base.tenant, Auth: testauth.Auth{Authz: allowAll{}}, Resolver: base.resolver, Kinds: testKinds("gallery")})
	if err != nil {
		t.Fatal(err)
	}
	erasure, err := base.store.beginMutation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer erasure.Rollback(context.Background())
	if err := base.lockErasureSubject(ctx, erasure, "gone"); err != nil {
		t.Fatal(err)
	}
	if _, err := erasure.Exec(ctx, `INSERT INTO `+base.erasedSubjectsTable()+` (tenant_id,actor_id) VALUES ($1,'gone')`, base.tenant); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := rt.reactions.react(ctx, access.Actor{ID: "gone"}, rt.Ref("gallery", localeID(42, "en")), 1)
		done <- err
	}()
	// Observe the writer's advisory-lock wait before committing the erasure.
	for {
		var waiting bool
		if err := base.store.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE application_name=$1 AND wait_event='advisory')`, cfg.ConnConfig.RuntimeParams["application_name"]).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(5 * time.Millisecond):
		}
	}
	if err := erasure.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, ErrSubjectErased) {
		t.Fatalf("repeatable-read session hid committed fence: %v", err)
	}
	var n int
	if err := base.store.pool.QueryRow(ctx, `SELECT count(*) FROM `+base.store.t.reactions+` WHERE user_id='gone'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("source resurrected after lock wait: %d %v", n, err)
	}
}

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

func TestErasureRejectsUnconfiguredRetainingPolicy(t *testing.T) {
	rt, pool := newTestRuntime(t, Options{})
	_, err := New(context.Background(), Options{Kinds: testKinds("gallery"), Pool: pool, Schema: rt.schema, Tenant: testTenant, Auth: testauth.Auth{Authz: allowAll{}}, Resolver: &fakeResolver{}, Classifier: &retainingPolicy{}})
	if err == nil {
		t.Fatal("retaining classifier accepted without lifecycle eraser")
	}
}

func TestErasureRetriesProviderAfterDurableSourceFence(t *testing.T) {
	ctx := context.Background()
	provider := &retainingPolicy{}
	rt, p := newPollTest(t, Options{Classifier: provider, ProviderDataEraser: provider})
	poll, err := p.create(ctx, pollAdmin, freeTextPoll())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.answer(ctx, access.Actor{ID: "u1"}, poll.ID, "personal"); err != nil {
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
	if _, err := p.answer(ctx, access.Actor{ID: "u1"}, poll.ID, "new"); !errors.Is(err, ErrSubjectErased) {
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

func TestErasureFencesPausedClassifierCompletion(t *testing.T) {
	ctx := context.Background()
	provider := &retainingPolicy{entered: make(chan struct{}), release: make(chan struct{})}
	rt, p := newPollTest(t, Options{Classifier: provider, ProviderDataEraser: provider})
	poll, err := p.create(ctx, pollAdmin, freeTextPoll())
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := p.answer(ctx, access.Actor{ID: "u1"}, poll.ID, "paused"); done <- err }()
	<-provider.entered
	if err := rt.EraseSubjects(ctx, []string{"u1"}); err != nil {
		t.Fatal(err)
	}
	close(provider.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	got, err := p.get(ctx, access.Actor{ID: "u1"}, poll.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.AnswerCount != 0 || len(got.Groups) != 0 || len(provider.retained) != 0 {
		t.Fatalf("paused classifier resurrected erased data: %+v", got)
	}
}

func TestErasureFencesPausedModerationBeforeSourceCommit(t *testing.T) {
	ctx := context.Background()
	provider := &retainingPolicy{entered: make(chan struct{}), release: make(chan struct{})}
	res := &fakeResolver{}
	res.set("gallery", cid(1), true, true)
	rt, _ := newTestRuntime(t, Options{Moderator: provider, ProviderDataEraser: provider, Resolver: res, Kinds: testKinds("gallery")})
	done := make(chan error, 1)
	go func() {
		_, err := rt.comments.create(ctx, access.Actor{ID: "u1"}, rt.Ref("gallery", cid(1)), CommentInput{Body: "paused"})
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

func TestErasurePreservesPublicationRepliesAndOtherTenant(t *testing.T) {
	ctx := context.Background()
	rt := moderatedRuntime(t, &fakeModerator{})
	author := access.Actor{ID: "u1"}
	sibling := access.Actor{ID: "u2"}
	published := mustComment(t, rt, author, "gallery", cid(1), CommentInput{Body: "published original"})
	reply := mustComment(t, rt, sibling, "gallery", cid(1), CommentInput{ReplyToID: published.ID, Body: "sibling reply"})
	unpublished := mustComment(t, rt, author, "gallery", cid(1), CommentInput{Body: "iffy never published"})
	if _, err := rt.comments.edit(ctx, author, published.ID, "iffy unpublished replacement"); err != nil {
		t.Fatal(err)
	}
	_, err := rt.store.pool.Exec(ctx, `INSERT INTO `+rt.store.t.comments+` (tenant_id,content_kind,content_id,user_id,body,moderation) VALUES ('other','gallery','`+cid(1)+`','u1','other tenant unpublished','held')`)
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
	if err := rt.store.pool.QueryRow(ctx, `SELECT body FROM `+rt.store.t.comments+` WHERE id=$1`, unpublished.ID).Scan(&body); err != nil || body != "" {
		t.Fatalf("unpublished content retained: %q %v", body, err)
	}
	if err := rt.store.pool.QueryRow(ctx, `SELECT body FROM `+rt.store.t.comments+` WHERE id=$1`, reply.ID).Scan(&body); err != nil || body != "sibling reply" {
		t.Fatalf("sibling reply damaged: %q %v", body, err)
	}
	if err := rt.store.pool.QueryRow(ctx, `SELECT body FROM `+rt.store.t.comments+` WHERE tenant_id='other'`).Scan(&body); err != nil || body != "other tenant unpublished" {
		t.Fatalf("foreign tenant erased: %q %v", body, err)
	}
	if _, err := rt.comments.edit(ctx, author, published.ID, "try again"); !errors.Is(err, ErrSubjectErased) {
		t.Fatalf("erased author edited again: %v", err)
	}
}

func TestBasicModeratorCacheUsesTypedIdentityAndErasure(t *testing.T) {
	m := &BasicModerator{}
	ctx := context.Background()
	for _, in := range []ModerationInput{{Tenant: "a", Actor: access.Actor{ID: "b|c"}, Text: "same"}, {Tenant: "a|b", Actor: access.Actor{ID: "c"}, Text: "same"}} {
		v, err := m.Screen(ctx, in)
		if err != nil || v.Decision != DecisionApprove {
			t.Fatalf("opaque identity collision: %+v %v", v, err)
		}
	}
	if err := m.EraseSubjects(ctx, "a", []string{"b|c"}); err != nil {
		t.Fatal(err)
	}
	if len(m.recent) != 1 {
		t.Fatalf("moderator cache cleanup crossed identities: %d", len(m.recent))
	}
	if _, err := m.Screen(ctx, ModerationInput{Tenant: "a", Actor: access.Actor{ID: "b|c"}, Text: "late"}); !errors.Is(err, ErrSubjectErased) {
		t.Fatalf("late completion recreated erased cache: %v", err)
	}
}

func TestErasureFenceSurvivesRuntimeRestartAndReview(t *testing.T) {
	ctx := context.Background()
	rt := moderatedRuntime(t, &fakeModerator{})
	author := access.Actor{ID: "u1"}
	cm := mustComment(t, rt, author, "gallery", cid(1), CommentInput{Body: "iffy held"})
	page, err := rt.ListHeld(ctx, KindComment, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if err := rt.EraseSubjects(ctx, []string{author.ID}); err != nil {
		t.Fatal(err)
	}
	opts := Options{Pool: rt.store.pool, Schema: rt.schema, Tenant: rt.tenant, Auth: testauth.Auth{Authz: allowAll{}}, Resolver: rt.resolver, Kinds: testKinds("gallery"), Moderator: &fakeModerator{}}
	restarted, err := New(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.comments.create(ctx, author, restarted.Ref("gallery", cid(1)), CommentInput{Body: "new unpublished body"}); !errors.Is(err, ErrSubjectErased) {
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
	if _, err := other.comments.create(ctx, author, other.Ref("gallery", cid(1)), CommentInput{Body: "other tenant content"}); err != nil {
		t.Fatalf("source fence crossed tenant runtime: %v", err)
	}
}

func TestBaselinePublishedContentSurvivesErasure(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Pool(t, nil)
	schema := pgtest.EmptySchema(t, ctx, pool)
	db := stdlib.OpenDBFromPool(pool)
	defer db.Close()
	pgtest.EnsureExtensions(t, ctx, pool)
	if err := migrations.ApplyPostgres(ctx, db, schema); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO `+schema+`.content_comments (tenant_id,content_kind,content_id,user_id,body) VALUES ('hostapp','gallery','`+cid(1)+`','u1','legacy publication')`); err != nil {
		t.Fatal(err)
	}
	if err := migrations.ApplyPostgres(ctx, db, schema); err != nil {
		t.Fatal(err)
	}
	rt, err := New(ctx, Options{Kinds: testKinds("gallery"), Pool: pool, Schema: schema, Tenant: testTenant, Auth: testauth.Auth{Authz: allowAll{}}, Resolver: &fakeResolver{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := rt.EraseSubjects(ctx, []string{"u1"}); err != nil {
		t.Fatal(err)
	}
	var body, user string
	var published *string
	if err := pool.QueryRow(ctx, `SELECT body,published_body,user_id FROM `+schema+`.content_comments`).Scan(&body, &published, &user); err != nil {
		t.Fatal(err)
	}
	if body != "legacy publication" || published != nil || user != "u1" {
		t.Fatalf("legacy published data altered: %q %v %q", body, published, user)
	}
}

// commentStatements records the statements a pool sends against one table.
type commentStatements struct {
	table string
	mu    sync.Mutex
	got   []pgx.TraceQueryStartData
}

func (l *commentStatements) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	if strings.Contains(d.SQL, l.table) {
		l.mu.Lock()
		l.got = append(l.got, d)
		l.mu.Unlock()
	}
	return ctx
}

func (*commentStatements) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestCommentReactionErasureLooksUpCommentsByKey(t *testing.T) {
	ctx := context.Background()
	rt := moderatedRuntime(t, &fakeModerator{})
	author, fan, keep := access.Actor{ID: "author"}, access.Actor{ID: "fan"}, access.Actor{ID: "keep"}
	var ids []string
	for i := range 3 {
		cm := mustComment(t, rt, author, "gallery", cid(1), CommentInput{Body: fmt.Sprintf("comment %d", i)})
		if _, err := rt.comments.reactTx(ctx, fan, cm.ID, 1); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, cm.ID)
	}
	if _, err := rt.comments.reactTx(ctx, keep, ids[0], 1); err != nil {
		t.Fatal(err)
	}
	// Historical keys no comment id equals as text: erasure removes them
	// without casting, which would abort it on the malformed one.
	for _, key := range []string{"not-a-uuid", strings.ReplaceAll(ids[1], "-", "")} {
		if _, err := rt.store.pool.Exec(ctx, `INSERT INTO `+rt.store.t.reactions+` (`+keyCols+`,user_id,value,revision) VALUES ($1,$2,$3,NULL,$4,1,`+rt.store.nextRevision()+`)`, rt.tenant, KindComment, key, fan.ID); err != nil {
			t.Fatal(err)
		}
	}
	// Enough other comments that the planner's choice reflects a real thread
	// population rather than a three-row table.
	if _, err := rt.store.pool.Exec(ctx, `INSERT INTO `+rt.store.t.comments+` (tenant_id,content_kind,content_id,user_id,body) SELECT $1,'gallery',$2,'other','filler' FROM generate_series(1,5000)`, rt.tenant, cid(1)); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.store.pool.Exec(ctx, `ANALYZE `+rt.store.t.comments); err != nil {
		t.Fatal(err)
	}
	log := &commentStatements{table: rt.store.t.comments}
	pool := pgtest.Pool(t, func(c *pgxpool.Config) { c.ConnConfig.Tracer = log })
	traced, err := New(ctx, Options{Pool: pool, Schema: rt.schema, Tenant: rt.tenant, Auth: testauth.Auth{Authz: allowAll{}}, Resolver: rt.resolver, Kinds: testKinds("gallery")})
	if err != nil {
		t.Fatal(err)
	}
	log.got = nil
	if err := traced.EraseSubjects(ctx, []string{fan.ID}); err != nil {
		t.Fatal(err)
	}
	for i, id := range ids {
		var likes int
		if err := rt.store.pool.QueryRow(ctx, `SELECT likes FROM `+rt.store.t.comments+` WHERE id=$1`, id).Scan(&likes); err != nil || likes != b2i(i == 0) {
			t.Fatalf("comment %d likes=%d err=%v", i, likes, err)
		}
	}
	var left int
	if err := rt.store.pool.QueryRow(ctx, `SELECT count(*) FROM `+rt.store.t.reactions+` WHERE user_id=$1`, fan.ID).Scan(&left); err != nil || left != 0 {
		t.Fatalf("erased reactions left=%d err=%v", left, err)
	}
	// With sequential scans priced out, a comment access bounded only by
	// tenant is a scan of every comment, not a lookup by key.
	tenantOnly := "Index Cond: (tenant_id = '" + rt.tenant + "'::text)"
	tx, err := rt.store.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SET LOCAL enable_seqscan = off`); err != nil {
		t.Fatal(err)
	}
	if len(log.got) == 0 {
		t.Fatal("erasure sent no comment statements")
	}
	for _, q := range log.got {
		rows, err := tx.Query(ctx, `EXPLAIN (COSTS OFF) `+q.SQL, q.Args...)
		if err != nil {
			t.Fatal(err)
		}
		plan, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range plan {
			if line = strings.TrimSpace(line); line == tenantOnly || strings.Contains(line, "Seq Scan on content_comments") {
				t.Fatalf("erasure scans comments:\n%s\n%s", q.SQL, strings.Join(plan, "\n"))
			}
		}
	}
}

func TestReplyCountWritesAndErasureLockCommentsInIDOrder(t *testing.T) {
	author, fan, reviewer := access.Actor{ID: "author"}, access.Actor{ID: "fan"}, access.Actor{ID: "reviewer"}
	revision := func(ctx context.Context, rt *Runtime, id string) int64 {
		var n int64
		if err := rt.store.pool.QueryRow(ctx, `SELECT moderation_revision FROM `+rt.store.t.comments+` WHERE id=$1`, id).Scan(&n); err != nil {
			t.Error(err)
		}
		return n
	}
	held := func(ctx context.Context, rt *Runtime, reply string) error {
		_, err := rt.comments.edit(ctx, author, reply, "iffy reply")
		return err
	}
	for _, tc := range []struct {
		name    string
		prep    func(ctx context.Context, rt *Runtime, reply string) error
		write   func(ctx context.Context, rt *Runtime, reply string) error
		replies int
	}{
		{name: "delete", write: func(ctx context.Context, rt *Runtime, reply string) error {
			return rt.comments.softDelete(ctx, author, reply)
		}},
		{name: "restore", replies: 1, prep: func(ctx context.Context, rt *Runtime, reply string) error {
			return rt.comments.softDelete(ctx, author, reply)
		}, write: func(ctx context.Context, rt *Runtime, reply string) error {
			return rt.comments.restore(ctx, reply)
		}},
		{name: "edit", write: held},
		{name: "hold", write: func(ctx context.Context, rt *Runtime, reply string) error {
			return rt.comments.hold(ctx, rt.as(reviewer), reply)
		}},
		{name: "approve", replies: 1, prep: held, write: func(ctx context.Context, rt *Runtime, reply string) error {
			return rt.Resolve(ctx, KindComment, reply, ReviewDecision{Revision: revision(ctx, rt, reply), Decision: DecisionApprove, Reviewer: reviewer.ID})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			rt := moderatedRuntime(t, &fakeModerator{})
			parent := mustComment(t, rt, author, "gallery", cid(1), CommentInput{Body: "parent"})
			// Comment ids are UUIDv7: a later millisecond orders the reply
			// after its parent, so erasure locks the parent first.
			time.Sleep(time.Millisecond)
			reply := mustComment(t, rt, author, "gallery", cid(1), CommentInput{Body: "reply", ReplyToID: parent.ID})
			if parent.ID >= reply.ID {
				t.Fatalf("reply %s does not sort after parent %s", reply.ID, parent.ID)
			}
			for _, id := range []string{parent.ID, reply.ID} {
				if _, err := rt.comments.reactTx(ctx, fan, id, 1); err != nil {
					t.Fatal(err)
				}
			}
			if tc.prep != nil {
				if err := tc.prep(ctx, rt, reply.ID); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(ctx, time.Minute)
			defer cancel()
			// Erasure queues on the held parent first and the reply write
			// behind it; releasing the parent lets both proceed.
			holder, err := rt.store.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer holder.Rollback(context.Background())
			if _, err := holder.Exec(ctx, `SELECT 1 FROM `+rt.store.t.comments+` WHERE id=$1 FOR UPDATE`, parent.ID); err != nil {
				t.Fatal(err)
			}
			errs := make(chan error, 2)
			go func() { errs <- rt.EraseSubjects(ctx, []string{fan.ID}) }()
			waitLockWaiters(t, ctx, rt, 1)
			go func() { errs <- tc.write(ctx, rt, reply.ID) }()
			waitLockWaiters(t, ctx, rt, 2)
			if err := holder.Rollback(ctx); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if err := <-errs; err != nil {
					t.Fatal(err)
				}
			}
			var replies, likes, replyLikes int
			if err := rt.store.pool.QueryRow(ctx, `SELECT p.reply_count, p.likes, r.likes FROM `+rt.store.t.comments+` p, `+rt.store.t.comments+` r WHERE p.id=$1 AND r.id=$2`, parent.ID, reply.ID).Scan(&replies, &likes, &replyLikes); err != nil {
				t.Fatal(err)
			}
			if replies != tc.replies || likes != 0 || replyLikes != 0 {
				t.Fatalf("reply_count=%d likes=%d reply likes=%d, want %d 0 0", replies, likes, replyLikes, tc.replies)
			}
		})
	}
}

// An admitted moderation write racing its moderator's erasure keeps its rule
// but never stores the erased id, whichever side takes the subject fence
// first; another moderator's write records them as before.
func TestModerationWritesRaceModeratorErasure(t *testing.T) {
	type write struct {
		name   string
		write  func(context.Context, *Runtime, string, string) error
		insert string // $1 tenant, $2 key, $3 provenance
		row    string // provenance column by $1 tenant, $2 key
	}
	writes := []write{
		{"comment ban", func(ctx context.Context, rt *Runtime, by, key string) error {
			_, err := rt.banCommenter(ctx, by, ScopeGlobal, key, BanInput{})
			return err
		}, `INSERT INTO %s (tenant_id, user_id, scope, banned_by) VALUES ($1, $2, 'global', $3)`,
			`SELECT banned_by FROM %s WHERE tenant_id=$1 AND user_id=$2 AND scope='global'`},
		{"banned word", func(ctx context.Context, rt *Runtime, by, key string) error {
			return rt.addBannedWords(ctx, by, []BannedWordInput{{Word: key}})
		}, `INSERT INTO %s (tenant_id, word, created_by) VALUES ($1, $2, $3)`,
			`SELECT created_by FROM %s WHERE tenant_id=$1 AND word=$2`},
		{"ip ban", func(ctx context.Context, rt *Runtime, by, key string) error {
			return rt.banIPs(ctx, by, []IPBanInput{{Network: key}})
		}, `INSERT INTO %s (tenant_id, network, banned_by) VALUES ($1, $2::cidr, $3)`,
			`SELECT banned_by FROM %s WHERE tenant_id=$1 AND network=$2::cidr`},
	}
	keys := map[string][3]string{ // seed, race, normal
		"comment ban": {"seeded", "target", "other-target"},
		"banned word": {"seeded", "racer", "other"},
		"ip ban":      {"192.0.2.0/24", "198.51.100.0/24", "203.0.113.0/24"},
	}
	const gone = "departing-moderator"
	for i, w := range writes {
		for _, erasureFirst := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/erasure first %v", w.name, erasureFirst), func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				rt, pool := newTestRuntime(t, Options{})
				table := []string{rt.store.t.commentBans, rt.store.t.bannedWords, rt.store.t.ipBans}[i]
				insert, row := fmt.Sprintf(w.insert, table), fmt.Sprintf(w.row, table)
				seed, race, normal := keys[w.name][0], keys[w.name][1], keys[w.name][2]
				if _, err := pool.Exec(ctx, insert, rt.tenant, seed, gone); err != nil {
					t.Fatal(err)
				}
				run := func(f func() error) <-chan error {
					c := make(chan error, 1)
					go func() { c <- f() }()
					return c
				}
				// waitFence returns once a session waits on a subject fence, or
				// once the other side finished without taking one.
				waitFence := func(c <-chan error) <-chan error {
					for {
						var waiting bool
						if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_locks WHERE locktype='advisory' AND NOT granted)`).Scan(&waiting); err != nil {
							t.Fatal(err)
						}
						if waiting {
							return c
						}
						select {
						case err := <-c:
							done := make(chan error, 1)
							done <- err
							return done
						case <-ctx.Done():
							t.Fatal(ctx.Err())
						case <-time.After(5 * time.Millisecond):
						}
					}
				}
				// The blocker pauses the first side after its fence: erasure on
				// the seeded rule it clears, the write on its own uncommitted key.
				blocker, err := pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer blocker.Rollback(context.Background())
				erase := func() error { return rt.EraseAppSubjects(ctx, []string{gone}) }
				moderate := func() error { return w.write(ctx, rt, gone, race) }
				var erased, wrote <-chan error
				if erasureFirst {
					if _, err := blocker.Exec(ctx, row+` FOR UPDATE`, rt.tenant, seed); err != nil {
						t.Fatal(err)
					}
					erased = run(erase)
					waitLockWaiters(t, ctx, rt, 1)
					wrote = waitFence(run(moderate))
				} else {
					if _, err := blocker.Exec(ctx, insert, rt.tenant, race, "blocker"); err != nil {
						t.Fatal(err)
					}
					wrote = run(moderate)
					waitLockWaiters(t, ctx, rt, 1)
					erased = waitFence(run(erase))
				}
				if err := blocker.Rollback(ctx); err != nil {
					t.Fatal(err)
				}
				if err := <-erased; err != nil {
					t.Fatalf("erasure: %v", err)
				}
				if err := <-wrote; err != nil {
					t.Fatalf("write: %v", err)
				}
				var by string
				if err := pool.QueryRow(ctx, row, rt.tenant, race).Scan(&by); err != nil || by != "" {
					t.Fatalf("raced rule provenance %q (%v)", by, err)
				}
				var n int
				if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM `+rt.store.t.commentBans+` WHERE banned_by=$1)
					+ (SELECT count(*) FROM `+rt.store.t.bannedWords+` WHERE created_by=$1)
					+ (SELECT count(*) FROM `+rt.store.t.ipBans+` WHERE banned_by=$1)`, gone).Scan(&n); err != nil || n != 0 {
					t.Fatalf("%d rows keep the erased moderator (%v)", n, err)
				}
				if err := w.write(ctx, rt, "staff", normal); err != nil {
					t.Fatal(err)
				}
				if err := pool.QueryRow(ctx, row, rt.tenant, normal).Scan(&by); err != nil || by != "staff" {
					t.Fatalf("another moderator's provenance %q (%v)", by, err)
				}
			})
		}
	}
}
