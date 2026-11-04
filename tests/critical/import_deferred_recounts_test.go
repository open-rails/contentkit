package critical_test

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/importer"
)

// A bulk import defers its recounts: its chunks leave the counts alone and
// queue each target once, however many batches touch it. Live writes keep
// adding to the stale counts meanwhile. Draining the queue then recounts
// each target once, to counts that hold every imported and live row.
func TestImportDeferredRecounts(t *testing.T) {
	w := newWorld(t, func(cfg *ck.Config) { cfg.Interactions.Limits.Disabled = true })
	a := w.apps[0]
	_, token := w.person(t, 0)
	item := publishedArticle(t, a, "Deferred recounts")
	postComment(t, a.client(token), item.ID, "Live comment before")
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	qs := pgx.Identifier{w.schema}.Sanitize()
	type recount struct{ comments, likes, favorites int }
	counts := func() recount {
		t.Helper()
		var got recount
		must(t, w.pool.QueryRow(ctx, `SELECT comment_count, likes, favorites FROM `+qs+`.content_interaction_counts WHERE content_id=$1 AND content_version_id IS NULL`, item.ID).
			Scan(&got.comments, &got.likes, &got.favorites))
		return got
	}
	before := counts()
	_, err := w.pool.Exec(ctx, fmt.Sprintf(`CREATE TABLE %[1]s.critical_recounts (n bigserial, comments int, likes int, favorites int);
CREATE FUNCTION %[1]s.critical_recount() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN
 INSERT INTO %[1]s.critical_recounts (comments, likes, favorites) VALUES (NEW.comment_count, NEW.likes, NEW.favorites); RETURN NEW; END$$;
CREATE TRIGGER critical_recount BEFORE UPDATE ON %[1]s.content_interaction_counts FOR EACH ROW
 WHEN (NEW.content_id = '%[2]s' AND NEW.content_version_id IS NULL) EXECUTE FUNCTION %[1]s.critical_recount()`, qs, item.ID))
	must(t, err)
	rewrites := func() int {
		t.Helper()
		var n int
		must(t, w.pool.QueryRow(ctx, `SELECT count(*) FROM `+qs+`.critical_recounts`).Scan(&n))
		return n
	}

	on := importer.TargetRef{Content: &importer.Ref{ID: item.ID}}
	parent := &importer.Ref{ExternalID: "parent"}
	comment := func(external, body string, replyTo *importer.Ref) importer.CommentRecord {
		return importer.CommentRecord{ExternalID: external, Target: on, ReplyTo: replyTo, AnonName: "Legacy guest", Body: body}
	}
	like := func(ip string, value int16) importer.ReactionRecord {
		return importer.ReactionRecord{Target: on, IP: ip, Value: value}
	}
	vote := func(option, ip string) importer.VoteRecord {
		return importer.VoteRecord{Poll: importer.Ref{ExternalID: "poll"}, Option: importer.Ref{ExternalID: option}, IP: ip}
	}
	poll := importer.PollRecord{ExternalID: "poll", Question: "Deferred question", Language: "en", Options: []importer.PollOptionRecord{
		{ExternalID: "first", Label: "First"}, {ExternalID: "second", Label: "Second", Position: 1}}}
	queued := func(r importer.Result) int {
		t.Helper()
		if r.RecountsQueued == nil {
			t.Fatalf("result does not report the queue: %+v", r)
		}
		return *r.RecountsQueued
	}
	// One bad comment fails alone; the rest still only queue.
	first := importer.Batch{Source: "deferred-recounts", DeferRecounts: true,
		Comments: []importer.CommentRecord{comment("parent", "Parent", nil), comment("two", "Two", nil), comment("reply-one", "Reply one", parent),
			comment("blank", "   ", nil), comment("three", "Three", nil), comment("reply-two", "Reply two", parent)},
		Reactions: []importer.ReactionRecord{like("203.0.113.1", 1), like("203.0.113.2", 1), {Target: importer.TargetRef{Comment: parent}, IP: "203.0.113.3", Value: 1}},
		Favorites: []importer.FavoriteRecord{{Target: on, UserID: "legacy-fan-1"}},
		Polls:     []importer.PollRecord{poll},
		Votes:     []importer.VoteRecord{vote("first", "203.0.113.1"), vote("first", "203.0.113.2")},
	}
	result, err := a.engine.Client().PostImport(ctx, first)
	must(t, err)
	wantWritten := map[string]int{importer.SectionComments: 5, importer.SectionReactions: 3, importer.SectionFavorites: 1, importer.SectionPolls: 1, importer.SectionVotes: 2}
	if len(result.Failed) != 1 || result.Failed[0].Index != 3 || !reflect.DeepEqual(result.Written, wantWritten) {
		t.Fatalf("bulk import wrote %v and failed %+v; want %v and the blank comment", result.Written, result.Failed, wantWritten)
	}
	// The article's counts, the parent's comment counts and replies, and the poll.
	if n := queued(result); n != 4 {
		t.Fatalf("queued %d targets; want 4", n)
	}
	ids, options := result.IDs[importer.RecordComment], result.IDs[importer.RecordOption]
	pollID := result.IDs[importer.RecordPoll]["poll"]
	if got := counts(); got != before || rewrites() != 0 {
		t.Fatalf("bulk import rewrote the counts %d times to %+v; want them left at %+v", rewrites(), got, before)
	}
	if got := readComment(t, a, item.ID, ids["parent"]); got.ReplyCount != 0 || got.Likes != 0 {
		t.Fatalf("bulk import recounted the parent: replies %d likes %d", got.ReplyCount, got.Likes)
	}

	// Live writes land on the stale counts while the targets wait.
	live := a.client(token)
	postComment(t, live, item.ID, "Live comment during")
	_, err = live.PostContentLike(ctx, item.ID, ck.PostContentLikeQuery{})
	must(t, err)
	_, err = live.PostContentFavorite(ctx, item.ID, ck.PostContentFavoriteQuery{})
	must(t, err)
	_, err = live.PostPollVote(ctx, pollID, content.PollVote{OptionID: options["second"]})
	must(t, err)

	// A later batch touches the same targets, deleting a comment too: they
	// stay queued once and the counts hold only the live writes.
	second := importer.Batch{Source: first.Source, DeferRecounts: true,
		Comments:  []importer.CommentRecord{comment("four", "Four", nil)},
		Reactions: []importer.ReactionRecord{like("203.0.113.4", 1)},
		Favorites: []importer.FavoriteRecord{{Target: on, UserID: "legacy-fan-2"}},
		Votes:     []importer.VoteRecord{vote("second", "203.0.113.4")},
		Deletes:   []importer.DeleteRecord{{Entity: importer.RecordComment, ExternalID: "two"}}}
	result, err = a.engine.Client().PostImport(ctx, second)
	must(t, err)
	if len(result.Failed) != 0 || queued(result) != 4 {
		t.Fatalf("second bulk batch failed %+v or queued %v; want 4 targets", result.Failed, result.RecountsQueued)
	}
	if got, want := counts(), (recount{before.comments + 1, 1, 1}); got != want {
		t.Fatalf("second bulk batch recounted to %+v; want only the live writes %+v", got, want)
	}
	drained := rewrites()

	result, err = a.engine.Client().PostImport(ctx, importer.Batch{Source: first.Source, Recount: 3})
	must(t, err)
	if result.Recounted != 3 || queued(result) != 1 {
		t.Fatalf("recounted %d with %v left; want 3 and 1", result.Recounted, result.RecountsQueued)
	}
	result, err = a.engine.Client().PostImport(ctx, importer.Batch{Source: first.Source, Recount: importer.MaxRecount})
	must(t, err)
	if result.Recounted != 1 || queued(result) != 0 {
		t.Fatalf("recounted %d with %v left; want 1 and 0", result.Recounted, result.RecountsQueued)
	}
	// Imported: four top-level comments, one deleted, three likes, two
	// favorites. Live: one more comment, like and favorite.
	if got, want := counts(), (recount{before.comments + 4, 4, 3}); got != want {
		t.Fatalf("drained counts %+v; want %+v", got, want)
	}
	if n := rewrites() - drained; n != 1 {
		t.Fatalf("draining rewrote the article's counts %d times; want once", n)
	}
	if got := readComment(t, a, item.ID, ids["parent"]); got.ReplyCount != 2 || got.Likes != 1 {
		t.Fatalf("parent reply count %d and likes %d; want 2 and 1", got.ReplyCount, got.Likes)
	}
	votes := map[string]int{}
	for _, external := range []string{"first", "second"} {
		var n int
		must(t, w.pool.QueryRow(ctx, `SELECT vote_count FROM `+qs+`.content_poll_options WHERE id=$1::uuid`, options[external]).Scan(&n))
		votes[external] = n
	}
	if want := map[string]int{"first": 2, "second": 2}; !reflect.DeepEqual(votes, want) {
		t.Fatalf("option vote counts %v; want %v", votes, want)
	}
	if _, err := a.engine.Client().PostImport(ctx, importer.Batch{Source: first.Source, Recount: importer.MaxRecount + 1}); err == nil {
		t.Fatal("a recount above the limit was accepted")
	}
	result, err = a.engine.Client().PostImport(ctx, importer.Batch{Source: first.Source})
	must(t, err)
	if result.RecountsQueued != nil {
		t.Fatalf("an import that neither defers nor recounts reported the queue: %d", *result.RecountsQueued)
	}
}

// A target an import queues again while a recount reads it stays queued:
// that recount counted without the new rows, so a later one recounts it.
func TestDeferredRecountKeepsRequeuedTarget(t *testing.T) {
	w := newWorld(t, func(cfg *ck.Config) { cfg.Interactions.Limits.Disabled = true })
	a := w.apps[0]
	item := publishedArticle(t, a, "Requeued recount")
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	on := importer.TargetRef{Content: &importer.Ref{ID: item.ID}}
	favorite := func(user string) importer.Batch {
		return importer.Batch{Source: "requeued-recount", DeferRecounts: true, Favorites: []importer.FavoriteRecord{{Target: on, UserID: user}}}
	}
	result, err := a.engine.Client().PostImport(ctx, favorite("legacy-fan-1"))
	must(t, err)
	if len(result.Failed) != 0 || result.RecountsQueued == nil || *result.RecountsQueued != 1 {
		t.Fatalf("bulk favorite failed %+v or queued %v", result.Failed, result.RecountsQueued)
	}
	qs := pgx.Identifier{w.schema}.Sanitize()
	favorites := func() int {
		t.Helper()
		var n int
		must(t, w.pool.QueryRow(ctx, `SELECT favorites FROM `+qs+`.content_interaction_counts WHERE content_id=$1 AND content_version_id IS NULL`, item.ID).Scan(&n))
		return n
	}
	mark := func() int64 {
		t.Helper()
		var m int64
		must(t, w.pool.QueryRow(ctx, `SELECT mark FROM `+qs+`.content_import_recounts WHERE content_id=$1`, item.ID).Scan(&m))
		return m
	}
	firstMark := mark()
	gate := w.schema + ":requeued-recount"
	holder, err := w.pool.Acquire(ctx)
	must(t, err)
	defer holder.Release()
	_, err = holder.Exec(ctx, "SELECT pg_advisory_lock(hashtextextended($1,0))", gate)
	must(t, err)
	locked := true
	defer func() {
		if locked {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, _ = holder.Exec(cleanupCtx, "SELECT pg_advisory_unlock(hashtextextended($1,0))", gate)
		}
	}()
	// The recount waits inside its counts update, after its snapshot.
	_, err = w.pool.Exec(ctx, fmt.Sprintf(`CREATE FUNCTION %[1]s.critical_recount_gate() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN PERFORM pg_advisory_xact_lock(hashtextextended(TG_ARGV[0],0)); RETURN NEW; END$$;
CREATE TRIGGER critical_recount_gate BEFORE UPDATE ON %[1]s.content_interaction_counts FOR EACH ROW
 WHEN (NEW.content_id = '%[2]s' AND NEW.content_version_id IS NULL) EXECUTE FUNCTION %[1]s.critical_recount_gate('%[3]s')`, qs, item.ID, gate))
	must(t, err)
	drained := make(chan importer.Result, 1)
	drainErr := make(chan error, 1)
	go func() {
		result, err := a.engine.Client().PostImport(ctx, importer.Batch{Source: "requeued-recount", Recount: 10})
		drained <- result
		drainErr <- err
	}()
	waitForWithin(t, 10*time.Second, func() bool {
		var waiting bool
		must(t, w.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)))`, holder.Conn().PgConn().PID()).Scan(&waiting))
		return waiting
	})
	result, err = a.engine.Client().PostImport(ctx, favorite("legacy-fan-2"))
	must(t, err)
	if len(result.Failed) != 0 {
		t.Fatalf("bulk favorite during the recount failed: %+v", result.Failed)
	}
	_, err = holder.Exec(ctx, "SELECT pg_advisory_unlock(hashtextextended($1,0))", gate)
	must(t, err)
	locked = false
	must(t, <-drainErr)
	// The first pass counted one favorite and left the target queued; the
	// same drain then recounted it again and dequeued it.
	result = <-drained
	left := -1
	if result.RecountsQueued != nil {
		left = *result.RecountsQueued
	}
	if result.Recounted != 1 || left != 0 || favorites() != 2 {
		t.Fatalf("drain dequeued %d at %d favorites with %d queued; want 1, 2 favorites and none queued", result.Recounted, favorites(), left)
	}
	// Queued again after the drain, the target takes a mark it never had, so
	// a drain that read an old mark cannot dequeue it.
	result, err = a.engine.Client().PostImport(ctx, favorite("legacy-fan-3"))
	must(t, err)
	if again := mark(); len(result.Failed) != 0 || again <= firstMark {
		t.Fatalf("queued again with mark %d after %d; marks must not repeat", again, firstMark)
	}
}
