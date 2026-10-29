package critical_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	ck "github.com/open-rails/contentkit"
	ckauth "github.com/open-rails/contentkit/adapters/authkit"
	"github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/importer"
)

// A group moderator authorized on a comment's original reference must not
// hold or release it once an import has moved it outside that group.
func TestScopedModeratorImportedReferenceRace(t *testing.T) {
	w := newWorld(t, func(cfg *ck.Config) { cfg.Interactions.Limits.Disabled = true })
	a := w.apps[0]
	for _, action := range []string{"hold", "release"} {
		t.Run("CK-J20-S08_"+action+"_refused_after_an_import_moves_the_comment_out_of_scope", func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			_, _, one := ownedChannel(t, w, a, "Original moderated channel "+action)
			_, otherOwnerToken, two := ownedChannel(t, w, a, "Destination outside scope "+action)
			moderator, token := w.person(t, 0)
			group, err := ckauth.ChannelScopeID(*a.cfg.Scope, one.AuthScope)
			must(t, err)
			authtest.GrantRole(t, w.tenant[0], iam.GroupByID(group), iam.UserSubject(moderator.ID), w.channelModerator)
			client := a.client(token)
			first, second := authoredArticle(t, a, nil, string(one.ID)), authoredArticle(t, a, nil, string(two.ID))
			record := importer.CommentRecord{ExternalID: "moving", Target: importer.TargetRef{Content: &importer.Ref{ID: first.ID}}, AnonName: "Legacy guest", Body: "Original anonymous import"}
			batch := importer.Batch{Source: "scoped-moderation-reference-" + action, Comments: []importer.CommentRecord{record,
				{ExternalID: "control", Target: importer.TargetRef{Content: &importer.Ref{ID: second.ID}}, AnonName: "Other guest", Body: "Static cross-channel denial"},
			}}
			initial, err := a.engine.Client().PostImport(ctx, batch)
			must(t, err)
			if len(initial.Failed) != 0 || initial.Written[importer.SectionComments] != 2 {
				t.Fatalf("scoped import fixture failed: %+v", initial)
			}
			commentID := initial.IDs[importer.RecordComment][record.ExternalID]
			controlID := initial.IDs[importer.RecordComment]["control"]
			requireError(t, client.PostCommentHold(ctx, controlID), 403, "forbidden")
			must(t, a.client(otherOwnerToken).PostCommentHold(ctx, controlID))
			requireError(t, client.PostCommentRelease(ctx, controlID), 403, "forbidden")
			must(t, client.PostCommentHold(ctx, commentID))
			must(t, client.PostCommentRelease(ctx, commentID))
			want := content.ModerationApproved
			if action == "release" {
				must(t, client.PostCommentHold(ctx, commentID))
				want = content.ModerationHeld
			}
			qs := pgx.Identifier{w.schema}.Sanitize()
			var revision int64
			var stamped time.Time
			must(t, w.pool.QueryRow(ctx, `SELECT moderation_revision,moderated_at FROM `+qs+`.content_comments WHERE tenant_id=$1 AND id=$2`, a.cfg.Scope.AppID.String(), commentID).Scan(&revision, &stamped))
			holder, err := w.pool.Acquire(ctx)
			must(t, err)
			defer holder.Release()
			gate := w.schema + ":scoped-import-reference:" + action
			_, err = holder.Exec(ctx, "SELECT pg_advisory_lock(hashtextextended($1,0))", gate)
			must(t, err)
			defer func() {
				cancel()
				cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cleanupCancel()
				_, _ = holder.Exec(cleanupCtx, "SELECT pg_advisory_unlock(hashtextextended($1,0))", gate)
				_, err := holder.Exec(cleanupCtx, `DROP TRIGGER IF EXISTS critical_scoped_reference ON `+qs+`.content_comments; DROP FUNCTION IF EXISTS `+qs+`.critical_scoped_reference()`)
				must(t, err)
			}()
			_, err = w.pool.Exec(ctx, fmt.Sprintf(`CREATE FUNCTION %s.critical_scoped_reference() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN IF NEW.id::text=TG_ARGV[0] AND NEW.content_id=TG_ARGV[1] AND OLD.content_id IS DISTINCT FROM NEW.content_id THEN PERFORM pg_advisory_xact_lock(hashtextextended(TG_ARGV[2],0)); END IF; RETURN NEW; END$$;
CREATE TRIGGER critical_scoped_reference BEFORE UPDATE OF content_id ON %s.content_comments FOR EACH ROW EXECUTE FUNCTION %s.critical_scoped_reference('%s','%s','%s')`, qs, qs, qs, commentID, second.ID, gate))
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
			record.Target = importer.TargetRef{Content: &importer.Ref{ID: second.ID}}
			record.Body = "Imported destination body"
			imported, attempted := make(chan error, 1), make(chan error, 1)
			go func() {
				result, err := a.engine.Client().PostImport(ctx, importer.Batch{Source: batch.Source, Comments: []importer.CommentRecord{record}})
				if err == nil && (len(result.Failed) != 0 || result.Written[importer.SectionComments] != 1) {
					err = fmt.Errorf("comment reference move failed: %+v", result)
				}
				imported <- err
			}()
			importPID := blockedBy(holder.Conn().PgConn().PID())
			go func() {
				if action == "hold" {
					attempted <- client.PostCommentHold(ctx, commentID)
				} else {
					attempted <- client.PostCommentRelease(ctx, commentID)
				}
			}()
			moderatorPID := blockedBy(importPID)
			var query string
			must(t, w.pool.QueryRow(ctx, "SELECT query FROM pg_stat_activity WHERE pid=$1", moderatorPID).Scan(&query))
			if !strings.Contains(query, "ORDER BY id FOR UPDATE") {
				t.Fatalf("moderation did not wait in its thread lock: %s", query)
			}
			_, err = holder.Exec(ctx, "SELECT pg_advisory_unlock(hashtextextended($1,0))", gate)
			must(t, err)
			must(t, <-imported)
			actionErr := <-attempted
			var refused *ck.Error
			if !errors.As(actionErr, &refused) || refused.Status != 403 || refused.Code != "forbidden" {
				t.Errorf("scoped moderator's %s after moving outside its channel was not refused: %v", action, actionErr)
			}
			var state, target, body, by string
			var afterRevision int64
			var afterStamp time.Time
			must(t, w.pool.QueryRow(ctx, `SELECT moderation,content_id,body,moderated_by,moderation_revision,moderated_at FROM `+qs+`.content_comments WHERE tenant_id=$1 AND id=$2`, a.cfg.Scope.AppID.String(), commentID).Scan(&state, &target, &body, &by, &afterRevision, &afterStamp))
			if state != want || target != second.ID || body != record.Body || by != moderator.ID || afterRevision != revision || !afterStamp.Equal(stamped) {
				t.Errorf("scoped %s changed imported destination state: moderation=%s want=%s target=%s body=%q reviewer=%s revision=%d want=%d timestampChanged=%t", action, state, want, target, body, by, afterRevision, revision, !afterStamp.Equal(stamped))
			}
			for _, id := range []string{first.ID, second.ID} {
				var count int
				must(t, w.pool.QueryRow(ctx, `SELECT coalesce((SELECT comment_count FROM `+qs+`.content_interaction_counts WHERE tenant_id=$1 AND content_kind='article' AND content_id=$2 AND content_version_id IS NULL),0)`, a.cfg.Scope.AppID.String(), id).Scan(&count))
				wantCount := 0
				if id == second.ID && want == content.ModerationApproved {
					wantCount = 1
				}
				if count != wantCount {
					t.Errorf("scoped %s changed reference count: content=%s count=%d want=%d", action, id, count, wantCount)
				}
			}
			assertCommentVisibility(t, a.client(""), first.ID, commentID, false)
			assertCommentVisibility(t, a.client(""), second.ID, commentID, want == content.ModerationApproved)
		})
	}
}
