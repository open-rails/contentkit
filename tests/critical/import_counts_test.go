package critical_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/importer"
)

func TestImportReplyCounts(t *testing.T) {
	w := newWorld(t, func(cfg *ck.Config) { cfg.Interactions.Limits.Disabled = true })
	a := w.apps[0]
	for _, mode := range []string{"update", "delete", "fresh", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			user, token := w.person(t, 0)
			item := publishedArticle(t, a, "Import replies "+mode)
			parent := postComment(t, a.client(token), item.ID, "Parent "+mode)
			created := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
			record := importer.CommentRecord{ExternalID: "old-reply", Target: importer.TargetRef{Content: &importer.Ref{ID: item.ID}},
				ReplyTo: &importer.Ref{ID: parent.ID}, AnonName: "Legacy guest", Body: "Imported reply", CreatedAt: &created}
			batch := importer.Batch{Source: "reply-count-" + mode, Comments: []importer.CommentRecord{record}}
			initial, err := a.engine.Client().PostImport(ctx, batch)
			must(t, err)
			if len(initial.Failed) != 0 || initial.Written[importer.SectionComments] != 1 {
				t.Fatalf("reply import fixture failed: %+v", initial)
			}
			if got := readComment(t, a, item.ID, parent.ID); got.ReplyCount != 1 {
				t.Fatalf("initial reply count=%d", got.ReplyCount)
			}
			gate := w.schema + ":reply:" + parent.ID
			holder, err := w.pool.Acquire(ctx)
			must(t, err)
			defer holder.Release()
			_, err = holder.Exec(ctx, "SELECT pg_advisory_lock(hashtextextended($1,0))", gate)
			must(t, err)
			locked := true
			qs := pgx.Identifier{w.schema}.Sanitize()
			defer func() {
				cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if locked {
					_, _ = holder.Exec(cleanupCtx, "SELECT pg_advisory_unlock(hashtextextended($1,0))", gate)
				}
				_, err := holder.Exec(cleanupCtx, `DROP TRIGGER IF EXISTS critical_live_reply ON `+qs+`.content_comments; DROP FUNCTION IF EXISTS `+qs+`.critical_live_reply()`)
				must(t, err)
			}()
			_, err = w.pool.Exec(ctx, fmt.Sprintf(`CREATE FUNCTION %s.critical_live_reply() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN IF NEW.user_id=TG_ARGV[0] AND NEW.reply_to_id::text=TG_ARGV[1] THEN PERFORM pg_advisory_xact_lock(hashtextextended(TG_ARGV[2],0)); END IF; RETURN NEW; END$$;
CREATE TRIGGER critical_live_reply BEFORE INSERT ON %s.content_comments FOR EACH ROW EXECUTE FUNCTION %s.critical_live_reply('%s','%s','%s')`, qs, qs, qs, user.ID, parent.ID, gate))
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
			live := make(chan error, 1)
			go func() {
				_, err := a.client(token).PostContentComments(ctx, item.ID, content.CommentInput{Body: "Concurrent live reply", ReplyToID: parent.ID}, ck.PostContentCommentsQuery{})
				live <- err
			}()
			livePID := blockedBy(holder.Conn().PgConn().PID())
			deleted := time.Now().UTC()
			section := importer.SectionComments
			switch mode {
			case "update", "cancel":
				batch.Comments[0].DeletedAt = &deleted
			case "delete":
				batch.Comments = nil
				batch.Deletes = []importer.DeleteRecord{{Entity: importer.RecordComment, ExternalID: record.ExternalID}}
				section = importer.SectionDeletes
			case "fresh":
				batch.Comments[0].ExternalID = "fresh-reply"
			}
			importCtx, cancelImport := context.WithCancel(ctx)
			defer cancelImport()
			imported := make(chan importer.Result, 1)
			importErr := make(chan error, 1)
			go func() {
				result, err := a.engine.Client().PostImport(importCtx, batch)
				imported <- result
				importErr <- err
			}()
			waiter := blockedBy(livePID)
			var query string
			must(t, w.pool.QueryRow(ctx, "SELECT query FROM pg_stat_activity WHERE pid=$1", waiter).Scan(&query))
			if !strings.Contains(query, "content_comments") {
				t.Fatalf("import waited outside its comment mutation: %s", query)
			}
			if mode == "cancel" {
				cancelImport()
				if err := <-importErr; err == nil {
					t.Fatal("canceled import unexpectedly succeeded")
				}
				waitForWithin(t, 5*time.Second, func() bool {
					var idle bool
					must(t, w.pool.QueryRow(ctx, "SELECT NOT EXISTS(SELECT 1 FROM pg_stat_activity WHERE pid=$1 AND xact_start IS NOT NULL)", waiter).Scan(&idle))
					return idle
				})
			}
			_, err = holder.Exec(ctx, "SELECT pg_advisory_unlock(hashtextextended($1,0))", gate)
			must(t, err)
			locked = false
			must(t, <-live)
			if mode != "cancel" {
				must(t, <-importErr)
			}
			result := <-imported
			if mode != "cancel" && (len(result.Failed) != 0 || result.Written[section] != 1) {
				t.Fatalf("concurrent import failed: %+v", result)
			}
			replies, err := a.client("").GetCommentReplies(ctx, parent.ID, ck.GetCommentRepliesQuery{})
			must(t, err)
			visible := 0
			for _, reply := range replies {
				if !reply.Deleted {
					visible++
				}
			}
			want := 1
			if mode == "fresh" {
				want = 3
			} else if mode == "cancel" {
				want = 2
			}
			got := readComment(t, a, item.ID, parent.ID)
			if visible != want || got.ReplyCount != want {
				t.Fatalf("import lost a live reply count: public count=%d visible replies=%d want=%d", got.ReplyCount, visible, want)
			}
			if mode != "cancel" {
				replayed, err := a.engine.Client().PostImport(ctx, batch)
				must(t, err)
				if len(replayed.Failed) != 0 || readComment(t, a, item.ID, parent.ID).ReplyCount != want {
					t.Fatalf("replaying import changed the correct count: %+v", replayed)
				}
				if mode != "delete" {
					ext := batch.Comments[0].ExternalID
					if id := result.IDs[importer.RecordComment][ext]; id == "" || replayed.IDs[importer.RecordComment][ext] != id {
						t.Fatal("replay changed imported reply identity")
					}
				}
			}
		})
	}
}

func TestConcurrentImportedReplies(t *testing.T) {
	w := newWorld(t, func(cfg *ck.Config) { cfg.Interactions.Limits.Disabled = true })
	a := w.apps[0]
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	_, token := w.person(t, 0)
	item := publishedArticle(t, a, "Two reply imports")
	parent := postComment(t, a.client(token), item.ID, "Parent")
	holder, err := w.pool.Begin(ctx)
	must(t, err)
	defer holder.Rollback(t.Context())
	_, err = holder.Exec(ctx, `SELECT 1 FROM `+w.schema+`.content_comments WHERE tenant_id=$1 AND id=$2 FOR NO KEY UPDATE`, a.cfg.Scope.AppID.String(), parent.ID)
	must(t, err)
	type outcome struct {
		result importer.Result
		err    error
	}
	results := make(chan outcome, 2)
	for _, external := range []string{"one", "two"} {
		batch := importer.Batch{Source: "simultaneous-replies", Comments: []importer.CommentRecord{{ExternalID: external,
			Target: importer.TargetRef{Content: &importer.Ref{ID: item.ID}}, ReplyTo: &importer.Ref{ID: parent.ID}, AnonName: "Legacy guest", Body: "Imported " + external}}}
		go func() {
			result, err := a.engine.Client().PostImport(ctx, batch)
			results <- outcome{result, err}
		}()
	}
	// Both imports queue behind the same thread owner before writing.
	// Each must finish with a count that includes the other committed reply.
	var first uint32
	waitForWithin(t, 10*time.Second, func() bool {
		must(t, w.pool.QueryRow(ctx, `SELECT coalesce(min(pid),0) FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid))`, holder.Conn().PgConn().PID()).Scan(&first))
		return first != 0
	})
	waitForWithin(t, 10*time.Second, func() bool {
		var second bool
		must(t, w.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE pid<>$2 AND ($1=ANY(pg_blocking_pids(pid)) OR $2=ANY(pg_blocking_pids(pid))))`, holder.Conn().PgConn().PID(), first).Scan(&second))
		return second
	})
	must(t, holder.Commit(ctx))
	for range 2 {
		out := <-results
		must(t, out.err)
		if len(out.result.Failed) != 0 || out.result.Written[importer.SectionComments] != 1 {
			t.Fatalf("concurrent reply import failed: %+v", out.result)
		}
	}
	replies, err := a.client("").GetCommentReplies(ctx, parent.ID, ck.GetCommentRepliesQuery{})
	must(t, err)
	if got := readComment(t, a, item.ID, parent.ID); got.ReplyCount != 2 || len(replies) != 2 {
		t.Fatalf("simultaneous imports lost a reply: count=%d replies=%d", got.ReplyCount, len(replies))
	}
}

func TestImportReplyCountsImportFirst(t *testing.T) {
	w := newWorld(t, func(cfg *ck.Config) { cfg.Interactions.Limits.Disabled = true })
	a := w.apps[0]
	for _, mode := range []string{"update", "delete"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			_, token := w.person(t, 0)
			item := publishedArticle(t, a, "Import first "+mode)
			parent := postComment(t, a.client(token), item.ID, "Parent")
			batch := importer.Batch{Source: "import-first-" + mode, Comments: []importer.CommentRecord{{ExternalID: "old-reply",
				Target: importer.TargetRef{Content: &importer.Ref{ID: item.ID}}, ReplyTo: &importer.Ref{ID: parent.ID}, AnonName: "Guest", Body: "Old reply"}}}
			result, err := a.engine.Client().PostImport(ctx, batch)
			must(t, err)
			if len(result.Failed) != 0 || readComment(t, a, item.ID, parent.ID).ReplyCount != 1 {
				t.Fatalf("reply fixture failed: %+v", result)
			}
			gate := w.schema + ":import-first:" + parent.ID
			holder, err := w.pool.Acquire(ctx)
			must(t, err)
			defer holder.Release()
			_, err = holder.Exec(ctx, "SELECT pg_advisory_lock(hashtextextended($1,0))", gate)
			must(t, err)
			locked := true
			qs := pgx.Identifier{w.schema}.Sanitize()
			defer func() {
				cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if locked {
					_, _ = holder.Exec(cleanupCtx, "SELECT pg_advisory_unlock(hashtextextended($1,0))", gate)
				}
				_, err := holder.Exec(cleanupCtx, `DROP TRIGGER IF EXISTS critical_import_recount ON `+qs+`.content_comments; DROP FUNCTION IF EXISTS `+qs+`.critical_import_recount()`)
				must(t, err)
			}()
			_, err = w.pool.Exec(ctx, fmt.Sprintf(`CREATE FUNCTION %s.critical_import_recount() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN IF NEW.id::text=TG_ARGV[0] THEN PERFORM pg_advisory_xact_lock(hashtextextended(TG_ARGV[1],0)); END IF; RETURN NEW; END$$;
CREATE TRIGGER critical_import_recount BEFORE UPDATE OF reply_count ON %s.content_comments FOR EACH ROW EXECUTE FUNCTION %s.critical_import_recount('%s','%s')`, qs, qs, qs, parent.ID, gate))
			must(t, err)
			if mode == "delete" {
				batch.Comments = nil
				batch.Deletes = []importer.DeleteRecord{{Entity: importer.RecordComment, ExternalID: "old-reply"}}
			} else {
				deleted := time.Now().UTC()
				batch.Comments[0].DeletedAt = &deleted
			}
			imported, live := make(chan error, 1), make(chan error, 1)
			go func() {
				result, err := a.engine.Client().PostImport(ctx, batch)
				if err == nil && len(result.Failed) != 0 {
					err = fmt.Errorf("import rejected: %+v", result.Failed)
				}
				imported <- err
			}()
			blockedBy := func(pid uint32) uint32 {
				t.Helper()
				var waiter uint32
				waitForWithin(t, 10*time.Second, func() bool {
					must(t, w.pool.QueryRow(ctx, `SELECT coalesce(min(pid),0) FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid))`, pid).Scan(&waiter))
					return waiter != 0
				})
				return waiter
			}
			importPID := blockedBy(holder.Conn().PgConn().PID())
			go func() {
				_, err := a.client(token).PostContentComments(ctx, item.ID, content.CommentInput{Body: "Live reply after import", ReplyToID: parent.ID}, ck.PostContentCommentsQuery{})
				live <- err
			}()
			blockedBy(importPID)
			_, err = holder.Exec(ctx, "SELECT pg_advisory_unlock(hashtextextended($1,0))", gate)
			must(t, err)
			locked = false
			must(t, <-imported)
			must(t, <-live)
			if got := readComment(t, a, item.ID, parent.ID); got.ReplyCount != 1 {
				t.Fatalf("live reply after import has count %d; want 1", got.ReplyCount)
			}
		})
	}
}

func TestImportReplyCountsWithModeration(t *testing.T) {
	w := newWorld(t, func(cfg *ck.Config) { cfg.Interactions.Limits.Disabled = true })
	a := w.apps[0]
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	manager, token := w.person(t, 0)
	authtest.GrantRole(t, w.tenant[0], a.group, iam.UserSubject(manager.ID), w.commentManager)
	item := publishedArticle(t, a, "Import and moderation")
	parentID, replyID := "00000000-0000-7000-8000-000000000001", "00000000-0001-7000-8000-000000000001"
	root := importer.CommentRecord{ExternalID: "parent", ID: parentID, Target: importer.TargetRef{Content: &importer.Ref{ID: item.ID}}, AnonName: "Guest", Body: "Parent"}
	reply := importer.CommentRecord{ExternalID: "reply", ID: replyID, Target: root.Target, ReplyTo: &importer.Ref{ID: parentID}, AnonName: "Guest", Body: "Reply"}
	batch := importer.Batch{Source: "import-moderation", Comments: []importer.CommentRecord{root, reply}}
	result, err := a.engine.Client().PostImport(ctx, batch)
	must(t, err)
	if len(result.Failed) != 0 {
		t.Fatalf("thread fixture failed: %+v", result)
	}
	gate := w.schema + ":import-child"
	holder, err := w.pool.Acquire(ctx)
	must(t, err)
	defer holder.Release()
	_, err = holder.Exec(ctx, "SELECT pg_advisory_lock(hashtextextended($1,0))", gate)
	must(t, err)
	locked := true
	qs := pgx.Identifier{w.schema}.Sanitize()
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if locked {
			_, _ = holder.Exec(cleanupCtx, "SELECT pg_advisory_unlock(hashtextextended($1,0))", gate)
		}
		_, err := holder.Exec(cleanupCtx, `DROP TRIGGER IF EXISTS critical_import_child ON `+qs+`.content_comments; DROP FUNCTION IF EXISTS `+qs+`.critical_import_child()`)
		must(t, err)
	}()
	_, err = w.pool.Exec(ctx, fmt.Sprintf(`CREATE FUNCTION %s.critical_import_child() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN IF NEW.id::text=TG_ARGV[0] AND NEW.body='Edited import reply' THEN PERFORM pg_advisory_xact_lock(hashtextextended(TG_ARGV[1],0)); END IF; RETURN NEW; END$$;
CREATE TRIGGER critical_import_child BEFORE UPDATE OF body ON %s.content_comments FOR EACH ROW EXECUTE FUNCTION %s.critical_import_child('%s','%s')`, qs, qs, qs, replyID, gate))
	must(t, err)
	reply.Body = "Edited import reply"
	batch.Comments = []importer.CommentRecord{reply}
	imported, moderated := make(chan error, 1), make(chan error, 1)
	go func() {
		result, err := a.engine.Client().PostImport(ctx, batch)
		if err == nil && len(result.Failed) != 0 {
			err = fmt.Errorf("import failed: %+v", result.Failed)
		}
		imported <- err
	}()
	blockedBy := func(pid uint32) uint32 {
		t.Helper()
		var waiter uint32
		waitForWithin(t, 10*time.Second, func() bool {
			must(t, w.pool.QueryRow(ctx, `SELECT coalesce(min(pid),0) FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid))`, pid).Scan(&waiter))
			return waiter != 0
		})
		return waiter
	}
	importPID := blockedBy(holder.Conn().PgConn().PID())
	go func() { moderated <- a.client(token).PostCommentHold(ctx, replyID) }()
	blockedBy(importPID)
	_, err = holder.Exec(ctx, "SELECT pg_advisory_unlock(hashtextextended($1,0))", gate)
	must(t, err)
	locked = false
	must(t, <-imported)
	must(t, <-moderated)
	if got := readComment(t, a, item.ID, parentID); got.ReplyCount != 0 {
		t.Fatalf("moderated imported reply remains counted: %d", got.ReplyCount)
	}
}

func TestConcurrentImportedReplyMoves(t *testing.T) {
	w := newWorld(t, func(cfg *ck.Config) { cfg.Interactions.Limits.Disabled = true })
	a := w.apps[0]
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	_, token := w.person(t, 0)
	item := publishedArticle(t, a, "Concurrent reply moves")
	var roots []string
	for _, body := range []string{"Original thread", "Intermediate thread", "Final thread"} {
		roots = append(roots, postComment(t, a.client(token), item.ID, body).ID)
	}
	record := importer.CommentRecord{ExternalID: "moving-reply", Target: importer.TargetRef{Content: &importer.Ref{ID: item.ID}},
		ReplyTo: &importer.Ref{ID: roots[0]}, AnonName: "Legacy guest", Body: "Imported reply"}
	batch := importer.Batch{Source: "concurrent-reply-moves", Comments: []importer.CommentRecord{record}}
	initial, err := a.engine.Client().PostImport(ctx, batch)
	must(t, err)
	if len(initial.Failed) != 0 || initial.Written[importer.SectionComments] != 1 {
		t.Fatalf("reply import fixture failed: %+v", initial)
	}
	replyID := initial.IDs[importer.RecordComment][record.ExternalID]
	holder, err := w.pool.Begin(ctx)
	must(t, err)
	defer holder.Rollback(t.Context())
	_, err = holder.Exec(ctx, `SELECT 1 FROM `+w.schema+`.content_comments WHERE tenant_id=$1 AND id=$2 FOR UPDATE`, a.cfg.Scope.AppID.String(), replyID)
	must(t, err)
	type outcome struct {
		result importer.Result
		err    error
	}
	results := make(chan outcome, 2)
	blockedBy := func(pid uint32) uint32 {
		t.Helper()
		var waiter uint32
		waitForWithin(t, 10*time.Second, func() bool {
			must(t, w.pool.QueryRow(ctx, `SELECT coalesce(min(pid),0) FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid))`, pid).Scan(&waiter))
			return waiter != 0
		})
		return waiter
	}
	var first uint32
	for i, root := range roots[1:] {
		move := record
		move.ReplyTo = &importer.Ref{ID: root}
		go func() {
			result, err := a.engine.Client().PostImport(ctx, importer.Batch{Source: batch.Source, Comments: []importer.CommentRecord{move}})
			results <- outcome{result, err}
		}()
		if i == 0 {
			first = blockedBy(holder.Conn().PgConn().PID())
		} else {
			blockedBy(first)
		}
	}
	must(t, holder.Commit(ctx))
	for range 2 {
		out := <-results
		must(t, out.err)
		if len(out.result.Failed) != 0 || out.result.Written[importer.SectionComments] != 1 {
			t.Fatalf("concurrent move import failed: %+v", out.result)
		}
	}
	for _, destination := range []string{roots[2], roots[0]} {
		batch.Comments[0].ReplyTo = &importer.Ref{ID: destination}
		replayed, err := a.engine.Client().PostImport(ctx, batch)
		must(t, err)
		if len(replayed.Failed) != 0 || replayed.IDs[importer.RecordComment][record.ExternalID] != replyID {
			t.Fatalf("reply replay or sequential move lost its identity: %+v", replayed)
		}
		for _, root := range roots {
			replies, err := a.client("").GetCommentReplies(ctx, root, ck.GetCommentRepliesQuery{})
			must(t, err)
			want := 0
			if root == destination {
				want = 1
			}
			if got := readComment(t, a, item.ID, root); got.ReplyCount != want || len(replies) != want {
				t.Fatalf("reply move left stale thread count: count=%d visible=%d want=%d", got.ReplyCount, len(replies), want)
			}
		}
	}
}

func TestMovedImportedReplyDeletionWithModeration(t *testing.T) {
	w := newWorld(t, func(cfg *ck.Config) { cfg.Interactions.Limits.Disabled = true })
	a := w.apps[0]
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	manager, token := w.person(t, 0)
	authtest.GrantRole(t, w.tenant[0], a.group, iam.UserSubject(manager.ID), w.commentManager)
	item := publishedArticle(t, a, "Moved reply deletion and moderation")
	originalID, destinationID, replyID := "00000000-0000-7000-8000-000000000010", "00000000-0000-7000-8000-000000000001", "00000000-0000-7000-8000-000000000020"
	target := importer.TargetRef{Content: &importer.Ref{ID: item.ID}}
	reply := importer.CommentRecord{ExternalID: "reply", ID: replyID, Target: target, ReplyTo: &importer.Ref{ID: originalID}, AnonName: "Legacy guest", Body: "Moving reply"}
	batch := importer.Batch{Source: "moved-reply-deletion", Comments: []importer.CommentRecord{
		{ExternalID: "original", ID: originalID, Target: target, AnonName: "Guest", Body: "Original thread"},
		{ExternalID: "destination", ID: destinationID, Target: target, AnonName: "Guest", Body: "Destination thread"},
		reply,
	}}
	initial, err := a.engine.Client().PostImport(ctx, batch)
	must(t, err)
	if len(initial.Failed) != 0 || initial.Written[importer.SectionComments] != 3 {
		t.Fatalf("moved reply fixture failed: %+v", initial)
	}
	holder, err := w.pool.Acquire(ctx)
	must(t, err)
	defer holder.Release()
	moveGate, deleteGate := w.schema+":move-before-delete", w.schema+":delete-before-hold"
	for _, gate := range []string{moveGate, deleteGate} {
		_, err = holder.Exec(ctx, "SELECT pg_advisory_lock(hashtextextended($1,0))", gate)
		must(t, err)
	}
	qs := pgx.Identifier{w.schema}.Sanitize()
	defer func() {
		cancel()
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		for _, gate := range []string{moveGate, deleteGate} {
			_, _ = holder.Exec(cleanupCtx, "SELECT pg_advisory_unlock(hashtextextended($1,0))", gate)
		}
		_, err := holder.Exec(cleanupCtx, `DROP TRIGGER IF EXISTS critical_moved_reply_delete ON `+qs+`.content_comments; DROP FUNCTION IF EXISTS `+qs+`.critical_moved_reply_delete()`)
		must(t, err)
	}()
	_, err = w.pool.Exec(ctx, fmt.Sprintf(`CREATE FUNCTION %s.critical_moved_reply_delete() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN
 IF NEW.id::text=TG_ARGV[0] THEN
  IF NEW.reply_to_id::text=TG_ARGV[1] AND OLD.reply_to_id IS DISTINCT FROM NEW.reply_to_id THEN
   PERFORM pg_advisory_xact_lock(hashtextextended(TG_ARGV[2],0));
  ELSIF OLD.deleted_at IS NULL AND NEW.deleted_at IS NOT NULL THEN
   PERFORM pg_advisory_xact_lock(hashtextextended(TG_ARGV[3],0));
  END IF;
 END IF;
 RETURN NEW;
END$$;
CREATE TRIGGER critical_moved_reply_delete BEFORE UPDATE ON %s.content_comments FOR EACH ROW EXECUTE FUNCTION %s.critical_moved_reply_delete('%s','%s','%s','%s')`, qs, qs, qs, replyID, destinationID, moveGate, deleteGate))
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
	type outcome struct {
		result importer.Result
		err    error
	}
	moved, deleted := make(chan outcome, 1), make(chan outcome, 1)
	reply.ReplyTo = &importer.Ref{ID: destinationID}
	go func() {
		result, err := a.engine.Client().PostImport(ctx, importer.Batch{Source: batch.Source, Comments: []importer.CommentRecord{reply}})
		moved <- outcome{result, err}
	}()
	movePID := blockedBy(holder.Conn().PgConn().PID())
	go func() {
		result, err := a.engine.Client().PostImport(ctx, importer.Batch{Source: batch.Source, Deletes: []importer.DeleteRecord{{Entity: importer.RecordComment, ExternalID: reply.ExternalID}}})
		deleted <- outcome{result, err}
	}()
	deletePID := blockedBy(movePID)
	var query string
	must(t, w.pool.QueryRow(ctx, "SELECT query FROM pg_stat_activity WHERE pid=$1", deletePID).Scan(&query))
	if !strings.Contains(query, "ORDER BY id FOR UPDATE") {
		t.Fatalf("deletion did not wait in its thread lock: %s", query)
	}
	_, err = holder.Exec(ctx, "SELECT pg_advisory_unlock(hashtextextended($1,0))", moveGate)
	must(t, err)
	move := <-moved
	must(t, move.err)
	if len(move.result.Failed) != 0 || move.result.Written[importer.SectionComments] != 1 {
		t.Fatalf("reply move failed: %+v", move.result)
	}
	if got := blockedBy(holder.Conn().PgConn().PID()); got != deletePID {
		t.Fatalf("delete barrier held another request: got %d want %d", got, deletePID)
	}
	held := make(chan error, 1)
	go func() { held <- a.client(token).PostCommentHold(ctx, replyID) }()
	blockedBy(deletePID)
	_, err = holder.Exec(ctx, "SELECT pg_advisory_unlock(hashtextextended($1,0))", deleteGate)
	must(t, err)
	deletion := <-deleted
	holdErr := <-held
	must(t, deletion.err)
	if len(deletion.result.Failed) != 0 || deletion.result.Written[importer.SectionDeletes] != 1 {
		t.Fatalf("moved reply deletion failed: %+v; hold=%v", deletion.result, holdErr)
	}
	if holdErr != nil {
		requireError(t, holdErr, 409, "conflict")
	}
	for _, root := range []string{originalID, destinationID} {
		replies, err := a.client("").GetCommentReplies(ctx, root, ck.GetCommentRepliesQuery{})
		must(t, err)
		visible := 0
		for _, r := range replies {
			if !r.Deleted {
				visible++
			}
		}
		if got := readComment(t, a, item.ID, root); got.ReplyCount != 0 || visible != 0 {
			t.Fatalf("deleted moved reply remains counted: root=%s count=%d visible=%d", root, got.ReplyCount, visible)
		}
	}
	for _, purged := range []bool{false, true} {
		if purged {
			_, err := w.pool.Exec(ctx, `DELETE FROM `+qs+`.content_comments WHERE tenant_id=$1 AND id=$2`, a.cfg.Scope.AppID.String(), replyID)
			must(t, err)
		}
		replayed, err := a.engine.Client().PostImport(ctx, importer.Batch{Source: batch.Source,
			Deletes: []importer.DeleteRecord{{Entity: importer.RecordComment, ExternalID: reply.ExternalID}}})
		must(t, err)
		if len(replayed.Failed) != 0 || replayed.Written[importer.SectionDeletes] != 1 {
			t.Fatalf("deletion replay failed (purged=%v): %+v", purged, replayed)
		}
	}
}
