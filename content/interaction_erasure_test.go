package content

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5/pgxpool"
	"sync"
	"testing"
	"time"
)

func TestAccountErasureOwnsSourceInteractionsAndExactCounters(t *testing.T) {
	ctx := context.Background()
	rt := newPreferenceRuntime(t)
	rt.perms.PollWrite = pollWritePerm
	u1, u2, anon := Actor{ID: "erase"}, Actor{ID: "keep"}, Actor{ID: "erase", Anonymous: true, IP: "erase"}
	target := ref("gallery", "42")
	for _, a := range []Actor{u1, u2, anon} {
		mustReact(t, rt, a, "gallery", "42:en", 1)
	}
	for _, a := range []Actor{u1, u2} {
		mustFavorite(t, rt, a, "gallery", "42:en", true)
	}
	cm := mustComment(t, rt, u2, "gallery", "42:en", createInput{Body: "keep author's comment"})
	for _, a := range []Actor{u1, u2, anon} {
		if _, err := rt.comments.reactTx(ctx, a, cm.ID, 1); err != nil {
			t.Fatal(err)
		}
	}
	post := insertPost(t, rt)
	if _, err := rt.store.pool.Exec(ctx, `UPDATE `+rt.store.t.posts+` SET is_draft=false WHERE id=$1`, post); err != nil {
		t.Fatal(err)
	}
	for _, a := range []Actor{u1, u2, anon} {
		if err := rt.posts.react(ctx, a, post, 1); err != nil {
			t.Fatal(err)
		}
	}
	poll, err := rt.polls.create(ctx, pollAdmin, createPollInput{Question: "q", Options: []createOptionInput{{Label: "a"}, {Label: "b"}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range []Actor{u1, u2, anon} {
		if _, err := rt.polls.vote(ctx, a, poll.ID, poll.Options[0].ID); err != nil {
			t.Fatal(err)
		}
	}
	other, err := New(ctx, Options{Pool: rt.store.pool, Schema: rt.schema, Tenant: "other", Identity: &fakeIdentity{}, Authz: allowAll{}, Resolver: rt.resolver, ContentKinds: []string{"gallery"}, Canonicalizer: ContentCanonicalizerFunc(galleryWork)})
	if err != nil {
		t.Fatal(err)
	}
	mustReact(t, other, u1, "gallery", "42:en", 1)
	for i := 0; i < 2; i++ {
		if err := rt.EraseSubjects(ctx, []string{u1.ID}); err != nil {
			t.Fatal(err)
		}
		got := countsOf(t, rt, target)
		if got.Likes != 2 || got.Favorites != 1 {
			t.Fatalf("source counters after erase%d: %+v", i, got)
		}
		var likes int
		for _, q := range []string{`SELECT likes FROM ` + rt.store.t.comments + ` WHERE id='` + cm.ID + `'`, `SELECT total_likes FROM ` + rt.store.t.posts + ` WHERE id='` + post + `'`, `SELECT vote_count FROM ` + rt.store.t.pollOptions + ` WHERE id='` + poll.Options[0].ID + `'`} {
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
	if got := countsOf(t, other, other.Ref("gallery", "42")); got.Likes != 1 {
		t.Fatalf("other tenant damaged: %+v", got)
	}
	for name, write := range map[string]func() error{
		"reaction":         func() error { _, _, e := rt.reactions.react(ctx, u1, "gallery", "42:en", 1); return e },
		"favorite":         func() error { _, e := rt.favorites.add(ctx, u1, "gallery", "42:en"); return e },
		"unfavorite":       func() error { _, e := rt.favorites.remove(ctx, u1, "gallery", "42:en"); return e },
		"post reaction":    func() error { return rt.posts.react(ctx, u1, post, 1) },
		"comment reaction": func() error { _, e := rt.comments.reactTx(ctx, u1, cm.ID, 1); return e },
		"poll vote":        func() error { _, e := rt.polls.vote(ctx, u1, poll.ID, poll.Options[0].ID); return e },
	} {
		if err := write(); !errors.Is(err, ErrSubjectErased) {
			t.Fatalf("%s crossed source fence: %v", name, err)
		}
	}
	for _, got := range pending(t, rt) {
		if got.ActorID != u2.ID {
			t.Fatalf("erased snapshot obligation survived: %+v", got)
		}
	}
}

func TestSourceWritesRaceErasureWithoutResurrection(t *testing.T) {
	ctx := context.Background()
	rt := newPreferenceRuntime(t)
	rt.perms.PollWrite = pollWritePerm
	actor := Actor{ID: "racer"}
	poll, err := rt.polls.create(ctx, pollAdmin, createPollInput{Question: "q", Options: []createOptionInput{{Label: "a"}, {Label: "b"}}})
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
				_, _, err = rt.reactions.react(ctx, actor, "gallery", "42:en", 1)
			case 1:
				_, err = rt.favorites.add(ctx, actor, "gallery", "42:en")
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
	if got := countsOf(t, rt, ref("gallery", "42")); got.Likes != 0 || got.Favorites != 0 {
		t.Fatalf("racing source counts retained: %+v", got)
	}
}

func TestConcurrentErasedActorsShareCountersWithoutDeadlock(t *testing.T) {
	ctx := context.Background()
	rt := newPreferenceRuntime(t)
	for _, actor := range []Actor{{ID: "a"}, {ID: "b"}, {ID: "keep"}} {
		for _, id := range []string{"42:en", "7:en"} {
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
	for _, id := range []string{"42", "7"} {
		got := countsOf(t, rt, ref("gallery", id))
		if got.Likes != 1 || got.Favorites != 1 {
			t.Fatalf("shared counters %s=%+v", id, got)
		}
	}
}

func TestRestoreReappliesSourceErasureAndFencedSnapshotsNeverReplay(t *testing.T) {
	ctx := context.Background()
	rt := newPreferenceRuntime(t)
	actor := Actor{ID: "gone"}
	snapshot := mustReact(t, rt, actor, "gallery", "42:en", 1)
	mustReact(t, rt, Actor{ID: "keep"}, "gallery", "42:en", 1)
	if err := rt.EraseSubjects(ctx, []string{actor.ID}); err != nil {
		t.Fatal(err)
	}
	// Privileged restore loads old source/counters but retains the permanent fence.
	if _, err := rt.store.pool.Exec(ctx, `INSERT INTO `+rt.store.t.reactions+` (`+keyCols+`,user_id,value) VALUES ($1,'gallery','42','','gone',1)`, rt.tenant); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.store.pool.Exec(ctx, `UPDATE `+rt.store.t.counts+` SET likes=likes+1 WHERE tenant_id=$1 AND content_kind='gallery' AND content_id='42'`, rt.tenant); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.store.pool.Exec(ctx, `INSERT INTO `+rt.store.t.preferenceSnapshots+` (`+preferenceCols+`) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,0)`, append(snapshot.PreferenceKey.args(), snapshot.Value, snapshot.Revision, snapshot.OccurredAt)...); err != nil {
		t.Fatal(err)
	}
	all, err := rt.ScanPreferences(ctx, PreferenceKey{}, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range all {
		if s.ActorID == actor.ID {
			t.Fatal("restored fenced snapshot became replayable")
		}
	}
	if _, err := rt.MigratePreferences(ctx, PreferenceMigrationOptions{}); !errors.Is(err, ErrSubjectErased) {
		t.Fatalf("restored erased source reseeded: %v", err)
	}
	if err := rt.EraseSubjects(ctx, []string{actor.ID}); err != nil {
		t.Fatal(err)
	}
	if got := countsOf(t, rt, ref("gallery", "42")); got.Likes != 1 {
		t.Fatalf("recovery double-counted erased interaction: %+v", got)
	}
	if _, err := rt.MigratePreferences(ctx, PreferenceMigrationOptions{ExportedKeys: []PreferenceKey{snapshot.PreferenceKey}}); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := rt.store.pool.QueryRow(ctx, `SELECT count(*) FROM `+rt.store.t.preferenceSnapshots+` WHERE actor_id='gone'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("export union recreated erased subject: %d %v", n, err)
	}
}

func TestConcurrentErasureWithCrossAuthoredReactionTargets(t *testing.T) {
	ctx := context.Background()
	rt := moderatedRuntime(t, &fakeModerator{})
	a, b := Actor{ID: "a"}, Actor{ID: "b"}
	ca := mustComment(t, rt, a, "gallery", "1", createInput{Body: "published a"})
	cb := mustComment(t, rt, b, "gallery", "1", createInput{Body: "published b"})
	if _, err := rt.comments.reactTx(ctx, a, cb.ID, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.comments.reactTx(ctx, b, ca.ID, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.comments.edit(ctx, a, ca.ID, "iffy a private"); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.comments.edit(ctx, b, cb.ID, "iffy b private"); err != nil {
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
		t.Fatalf("cross erasure retained private bodies/counters: %d %v", n, err)
	}
}

func TestInteractionErasureDoesNotRecreateRemovedRollups(t *testing.T) {
	ctx := context.Background()
	rt := newPreferenceRuntime(t)
	mustReact(t, rt, Actor{ID: "gone"}, "gallery", "42:en", 1)
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
	rt, err := New(ctx, Options{Pool: pool, Schema: base.schema, Tenant: base.tenant, Identity: &fakeIdentity{}, Authz: allowAll{}, Resolver: base.resolver, ContentKinds: []string{"gallery"}, Canonicalizer: ContentCanonicalizerFunc(galleryWork)})
	if err != nil {
		t.Fatal(err)
	}
	erasure, err := base.store.beginMutation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer erasure.Rollback(context.Background())
	if err := base.lockPrivateSubject(ctx, erasure, "gone"); err != nil {
		t.Fatal(err)
	}
	if _, err := erasure.Exec(ctx, `INSERT INTO `+base.privateFences()+` (tenant_id,actor_id) VALUES ($1,'gone')`, base.tenant); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, _, err := rt.reactions.react(ctx, Actor{ID: "gone"}, "gallery", "42:en", 1); done <- err }()
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

func TestSourceErasureRedactsDraftAndScheduledPosts(t *testing.T) {
	ctx := context.Background()
	rt, _ := newPostRuntime(t, Options{})
	author := Actor{ID: "gone"}
	future := time.Now().Add(time.Hour)
	for _, in := range []postWriteReq{
		{Title: ptr("private draft"), Body: ptr("draft secret"), Excerpt: ptr("draft excerpt"), IsDraft: ptr(true)},
		{Title: ptr("scheduled"), Body: ptr("scheduled secret"), Excerpt: ptr("scheduled excerpt"), IsDraft: ptr(false), LiveAt: &future},
	} {
		rec := doJSON(t, rt.Handler(), author, "POST", "/posts", in)
		if rec.Code != 201 {
			t.Fatalf("create %d %s", rec.Code, rec.Body.String())
		}
	}
	rec := doJSON(t, rt.Handler(), author, "POST", "/posts", postWriteReq{Title: ptr("published"), Body: ptr("retain public"), IsDraft: ptr(false)})
	if rec.Code != 201 {
		t.Fatal(rec.Body.String())
	}
	if err := rt.EraseSubjects(ctx, []string{author.ID}); err != nil {
		t.Fatal(err)
	}
	var private, public int
	if err := rt.store.pool.QueryRow(ctx, `SELECT count(*) FROM `+rt.store.t.posts+` WHERE deleted_at IS NOT NULL AND body='' AND title='' AND excerpt IS NULL AND moderation_verdict IS NULL`).Scan(&private); err != nil {
		t.Fatal(err)
	}
	if err := rt.store.pool.QueryRow(ctx, `SELECT count(*) FROM `+rt.store.t.posts+` WHERE body='retain public' AND deleted_at IS NULL`).Scan(&public); err != nil {
		t.Fatal(err)
	}
	if private != 2 || public != 1 {
		t.Fatalf("private=%d public=%d", private, public)
	}
}

func TestErasureDoesNotRetainScheduledPostAsPublishedBackup(t *testing.T) {
	ctx := context.Background()
	rt := moderatedRuntime(t, &fakeModerator{})
	actor := Actor{ID: "reviewer"}
	future := time.Now().Add(time.Hour)
	rec := doJSON(t, rt.Handler(), actor, "POST", "/posts", postWriteReq{Title: ptr("scheduled"), Body: ptr("private scheduled original"), IsDraft: ptr(false), LiveAt: &future})
	if rec.Code != 201 {
		t.Fatal(rec.Body.String())
	}
	post := decodePost(t, rec)
	rec = doJSON(t, rt.Handler(), actor, "PATCH", "/posts/"+post.ID, postWriteReq{Body: ptr("iffy scheduled replacement")})
	if rec.Code != 202 {
		t.Fatal(rec.Body.String())
	}
	if err := rt.EraseSubjects(ctx, []string{actor.ID}); err != nil {
		t.Fatal(err)
	}
	var body string
	var backup *string
	if err := rt.store.pool.QueryRow(ctx, `SELECT body,published_content::text FROM `+rt.store.t.posts+` WHERE id=$1`, post.ID).Scan(&body, &backup); err != nil {
		t.Fatal(err)
	}
	if body != "" || backup != nil {
		t.Fatalf("neverpublished content retained: %q %v", body, backup)
	}
}
