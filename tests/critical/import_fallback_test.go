package critical_test

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/importer"
)

// One bad record sends its chunk to the one-by-one fallback. The good records
// land, the bad one fails alone, and the article's counts are recounted once
// per section, after its records, to counts that keep the live rows. A failed
// final recount keeps none of the chunk.
func TestImportFallbackRecountsOnce(t *testing.T) {
	w := newWorld(t, func(cfg *ck.Config) { cfg.Interactions.Limits.Disabled = true })
	a := w.apps[0]
	_, token := w.person(t, 0)
	item := publishedArticle(t, a, "Import fallback recount")
	postComment(t, a.client(token), item.ID, "Live comment")
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
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
	live := counts().comments
	_, err := w.pool.Exec(ctx, fmt.Sprintf(`CREATE TABLE %[1]s.critical_recounts (n bigserial, comments int, likes int, favorites int);
CREATE FUNCTION %[1]s.critical_recount() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN
 INSERT INTO %[1]s.critical_recounts (comments, likes, favorites) VALUES (NEW.comment_count, NEW.likes, NEW.favorites); RETURN NEW; END$$;
CREATE TRIGGER critical_recount BEFORE UPDATE ON %[1]s.content_interaction_counts FOR EACH ROW
 WHEN (NEW.content_id = '%[2]s' AND NEW.content_version_id IS NULL) EXECUTE FUNCTION %[1]s.critical_recount()`, qs, item.ID))
	must(t, err)

	on := importer.TargetRef{Content: &importer.Ref{ID: item.ID}}
	parent := &importer.Ref{ExternalID: "parent"}
	comment := func(external, body string, replyTo *importer.Ref) importer.CommentRecord {
		return importer.CommentRecord{ExternalID: external, Target: on, ReplyTo: replyTo, AnonName: "Legacy guest", Body: body}
	}
	like := func(ip string, value int16) importer.ReactionRecord {
		return importer.ReactionRecord{Target: on, IP: ip, Value: value}
	}
	vote := func(poll, option, ip string) importer.VoteRecord {
		return importer.VoteRecord{Poll: importer.Ref{ExternalID: poll}, Option: importer.Ref{ExternalID: option}, IP: ip}
	}
	poll := func(external string) importer.PollRecord {
		return importer.PollRecord{ExternalID: external, Question: "Question " + external, Language: "en", Options: []importer.PollOptionRecord{
			{ExternalID: external + "-1", Label: "First"}, {ExternalID: external + "-2", Label: "Second", Position: 1}}}
	}
	batch := importer.Batch{Source: "fallback-recount",
		Comments: []importer.CommentRecord{comment("parent", "Parent", nil), comment("two", "Two", nil), comment("reply-one", "Reply one", parent),
			comment("blank", "   ", nil), comment("three", "Three", nil), comment("reply-two", "Reply two", parent), comment("four", "Four", nil)},
		Reactions: []importer.ReactionRecord{like("203.0.113.1", 1), like("203.0.113.2", 1), like("203.0.113.3", 5), like("203.0.113.4", 1),
			{Target: importer.TargetRef{Comment: parent}, IP: "203.0.113.5", Value: 1}},
		Favorites: []importer.FavoriteRecord{{Target: on, UserID: "legacy-fan-1"}, {Target: on}, {Target: on, UserID: "legacy-fan-2"}},
		Polls:     []importer.PollRecord{poll("a"), poll("b")},
		Votes: []importer.VoteRecord{vote("a", "a-1", "203.0.113.1"), vote("a", "a-1", "203.0.113.2"), vote("a", "b-1", "203.0.113.3"),
			vote("a", "a-2", "203.0.113.4"), vote("b", "b-1", "203.0.113.5")},
	}
	result, err := a.engine.Client().PostImport(ctx, batch)
	must(t, err)

	type failure struct {
		section string
		index   int
		code    string
	}
	failures := func(r importer.Result) []failure {
		var out []failure
		for _, f := range r.Failed {
			out = append(out, failure{f.Section, f.Index, f.Code})
		}
		return out
	}
	wantFailed := []failure{{importer.SectionComments, 3, importer.CodeInvalid}, {importer.SectionReactions, 2, importer.CodeInvalid},
		{importer.SectionFavorites, 1, importer.CodeInvalid}, {importer.SectionVotes, 2, importer.CodeNotFound}}
	wantWritten := map[string]int{importer.SectionComments: 6, importer.SectionReactions: 4, importer.SectionFavorites: 2, importer.SectionPolls: 2, importer.SectionVotes: 4}
	if got := failures(result); !reflect.DeepEqual(got, wantFailed) || !reflect.DeepEqual(result.Written, wantWritten) {
		t.Fatalf("fallback wrote %v and failed %+v; want %v and %+v", result.Written, result.Failed, wantWritten, wantFailed)
	}
	ids := result.IDs[importer.RecordComment]
	if len(ids) != 6 || ids["blank"] != "" {
		t.Fatalf("comment identities %v; want the six good ones only", ids)
	}

	comments := live + 4
	if got, want := counts(), (recount{comments, 3, 2}); got != want {
		t.Fatalf("article counts %+v; want %+v", got, want)
	}
	rows, err := w.pool.Query(ctx, `SELECT comments, likes, favorites FROM `+qs+`.critical_recounts ORDER BY n`)
	must(t, err)
	var log []recount
	for rows.Next() {
		var r recount
		must(t, rows.Scan(&r.comments, &r.likes, &r.favorites))
		log = append(log, r)
	}
	must(t, rows.Err())
	// Recounting per record would update the row once for each good record.
	if want := []recount{{comments, 0, 0}, {comments, 3, 0}, {comments, 3, 2}}; !reflect.DeepEqual(log, want) {
		t.Fatalf("article counts were rewritten %d times %+v; want once per section %+v", len(log), log, want)
	}
	if got := readComment(t, a, item.ID, ids["parent"]); got.ReplyCount != 2 || got.Likes != 1 {
		t.Fatalf("parent reply count %d and likes %d; want 2 and 1", got.ReplyCount, got.Likes)
	}
	options := result.IDs[importer.RecordOption]
	votes := map[string]int{}
	for _, external := range []string{"a-1", "a-2", "b-1", "b-2"} {
		var n int
		must(t, w.pool.QueryRow(ctx, `SELECT vote_count FROM `+qs+`.content_poll_options WHERE id=$1::uuid`, options[external]).Scan(&n))
		votes[external] = n
	}
	if want := map[string]int{"a-1": 2, "a-2": 1, "b-1": 1, "b-2": 0}; !reflect.DeepEqual(votes, want) {
		t.Fatalf("option vote counts %v; want %v", votes, want)
	}

	_, err = w.pool.Exec(ctx, fmt.Sprintf(`CREATE FUNCTION %[1]s.critical_recount_fault() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN RAISE EXCEPTION 'recount fault'; END$$;
CREATE TRIGGER critical_recount_fault BEFORE UPDATE ON %[1]s.content_interaction_counts FOR EACH ROW
 WHEN (NEW.content_id = '%[2]s' AND NEW.content_version_id IS NULL) EXECUTE FUNCTION %[1]s.critical_recount_fault()`, qs, item.ID))
	must(t, err)
	more := importer.Batch{Source: batch.Source, Comments: []importer.CommentRecord{comment("five", "Five", nil), comment("blank-two", "   ", nil), comment("six", "Six", nil)}}
	result, err = a.engine.Client().PostImport(ctx, more)
	must(t, err)
	wantFailed = []failure{{importer.SectionComments, 1, importer.CodeInvalid}, {importer.SectionComments, 0, importer.CodeInternal}, {importer.SectionComments, 2, importer.CodeInternal}}
	if got := failures(result); !reflect.DeepEqual(got, wantFailed) || len(result.Written) != 0 || len(result.IDs) != 0 {
		t.Fatalf("failed recount wrote %v %v and failed %+v; want nothing written and %+v", result.Written, result.IDs, result.Failed, wantFailed)
	}
	var kept int
	must(t, w.pool.QueryRow(ctx, `SELECT count(*) FROM `+qs+`.content_comments WHERE content_id=$1 AND body IN ('Five', 'Six')`, item.ID).Scan(&kept))
	if kept != 0 || counts().comments != comments {
		t.Fatalf("failed recount kept %d comments and counts %+v", kept, counts())
	}
	_, err = w.pool.Exec(ctx, `DROP TRIGGER critical_recount_fault ON `+qs+`.content_interaction_counts`)
	must(t, err)
	result, err = a.engine.Client().PostImport(ctx, more)
	must(t, err)
	if len(result.Failed) != 1 || result.Written[importer.SectionComments] != 2 || counts().comments != comments+2 {
		t.Fatalf("retried import wrote %v and failed %+v; counts %+v", result.Written, result.Failed, counts())
	}
}

// An import locks a comment before writing a like on it, as a live like
// does. Paused right after writing an anonymous like, it holds the comment,
// so a live like from the same address waits for it instead of deadlocking.
func TestImportedCommentLikeLockOrder(t *testing.T) {
	w := newWorld(t, func(cfg *ck.Config) {
		cfg.Interactions.Limits.Disabled = true
		cfg.Interactions.Anonymous.Reactions = true
	})
	a := w.apps[0]
	_, token := w.person(t, 0)
	item := publishedArticle(t, a, "Imported comment like lock order")
	liked := postComment(t, a.client(token), item.ID, "Liked comment")
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	gate := w.schema + ":imported-like"
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
	qs := pgx.Identifier{w.schema}.Sanitize()
	_, err = w.pool.Exec(ctx, fmt.Sprintf(`CREATE FUNCTION %[1]s.critical_imported_like() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN PERFORM pg_advisory_xact_lock(hashtextextended(TG_ARGV[0],0)); RETURN NEW; END$$;
CREATE TRIGGER critical_imported_like AFTER INSERT ON %[1]s.content_reactions FOR EACH ROW
 WHEN (NEW.content_id = '%[2]s' AND NEW.user_id IS NULL) EXECUTE FUNCTION %[1]s.critical_imported_like('%[3]s')`, qs, liked.ID, gate))
	must(t, err)
	blockedBy := func(pid uint32) uint32 {
		t.Helper()
		var waiter uint32
		waitForWithin(t, 10*time.Second, func() bool {
			must(t, w.pool.QueryRow(ctx, `SELECT coalesce(min(pid),0) FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid))`, pid).Scan(&waiter))
			return waiter != 0
		})
		return waiter
	}
	// The bad like sends the chunk to the one-by-one fallback.
	batch := importer.Batch{Source: "imported-like-lock-order", Reactions: []importer.ReactionRecord{
		{Target: importer.TargetRef{Comment: &importer.Ref{ID: liked.ID}}, IP: "127.0.0.1", Value: 1},
		{Target: importer.TargetRef{Content: &importer.Ref{ID: item.ID}}, IP: "127.0.0.1", Value: 5}}}
	imported := make(chan importer.Result, 1)
	importErr, live := make(chan error, 1), make(chan error, 1)
	go func() {
		result, err := a.engine.Client().PostImport(ctx, batch)
		imported <- result
		importErr <- err
	}()
	importPID := blockedBy(holder.Conn().PgConn().PID())
	go func() {
		_, err := a.client("").PostCommentLike(ctx, liked.ID)
		live <- err
	}()
	blockedBy(importPID)
	_, err = holder.Exec(ctx, "SELECT pg_advisory_unlock(hashtextextended($1,0))", gate)
	must(t, err)
	locked = false
	must(t, <-live)
	must(t, <-importErr)
	result := <-imported
	if len(result.Failed) != 1 || result.Failed[0].Index != 1 || result.Written[importer.SectionReactions] != 1 {
		t.Fatalf("import wrote %v and failed %+v; want the like written and the bad value failed", result.Written, result.Failed)
	}
	if got := readComment(t, a, item.ID, liked.ID); got.Likes != 1 {
		t.Fatalf("one address liked twice; comment likes %d, want 1", got.Likes)
	}
}
