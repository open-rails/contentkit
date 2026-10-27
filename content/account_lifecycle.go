package content

import (
	"context"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/versionid"
)

func (rt *Runtime) accountLifecycleTable() string {
	return pgx.Identifier{rt.schema, "content_account_lifecycle"}.Sanitize()
}

// SetAccountDeleted changes author presentation during the recovery period.
// It preserves every interaction and never reverses a completed hard deletion.
func (rt *Runtime) SetAccountDeleted(ctx context.Context, actorID string, deleted bool) error {
	if strings.TrimSpace(actorID) == "" {
		return badRequest("subject id is required")
	}
	err := pgx.BeginFunc(ctx, rt.store.pool, func(tx pgx.Tx) error {
		if err := rt.lockErasureSubject(ctx, tx, actorID); err != nil {
			return err
		}
		if deleted {
			_, err := tx.Exec(ctx, `INSERT INTO `+rt.accountLifecycleTable()+` (tenant_id,actor_id,state)
VALUES ($1,$2,'deleted') ON CONFLICT (tenant_id,actor_id) DO NOTHING`, rt.tenant, actorID)
			return err
		}
		_, err := tx.Exec(ctx, `DELETE FROM `+rt.accountLifecycleTable()+` WHERE tenant_id=$1 AND actor_id=$2 AND state='deleted'`, rt.tenant, actorID)
		return err
	})
	return err
}

func (rt *Runtime) hiddenAuthorsTable() string {
	return pgx.Identifier{rt.schema, "content_hidden_authors"}.Sanitize()
}

// SetAccountHidden changes comment visibility without changing moderation or
// account deletion state. Repeating the same state leaves counts unchanged.
func (rt *Runtime) SetAccountHidden(ctx context.Context, actorID string, hidden bool) error {
	if strings.TrimSpace(actorID) == "" {
		return badRequest("subject id is required")
	}
	return pgx.BeginFunc(ctx, rt.store.pool, func(tx pgx.Tx) error {
		if err := rt.lockErasureSubject(ctx, tx, actorID); err != nil {
			return err
		}
		if hidden {
			if _, err := tx.Exec(ctx, `INSERT INTO `+rt.hiddenAuthorsTable()+` (tenant_id,actor_id)
VALUES ($1,$2) ON CONFLICT (tenant_id,actor_id) DO NOTHING`, rt.tenant, actorID); err != nil {
				return err
			}
		} else {
			if _, err := tx.Exec(ctx, `DELETE FROM `+rt.hiddenAuthorsTable()+` WHERE tenant_id=$1 AND actor_id=$2`, rt.tenant, actorID); err != nil {
				return err
			}
		}
		c := rt.comments
		// Lock every affected comment and its parent at once, in id order, the
		// order lockThread takes two of them in. Content rollups follow, once
		// each in key order, as recount and erasure lock them.
		if _, err := tx.Exec(ctx, `SELECT 1 FROM `+c.s.t.comments+` WHERE tenant_id=$1 AND (user_id=$2 OR id IN
(SELECT reply_to_id FROM `+c.s.t.comments+` WHERE tenant_id=$1 AND user_id=$2)) ORDER BY id FOR UPDATE`, rt.tenant, actorID); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT id::text FROM `+c.s.t.comments+` WHERE tenant_id=$1 AND user_id=$2 AND author_hidden<>$3 ORDER BY id`, rt.tenant, actorID, hidden)
		if err != nil {
			return err
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		delta := 1
		if hidden {
			delta = -1
		}
		rollups := map[contentref.ContentKey]int{}
		for _, id := range ids {
			var replyTo *string
			var deletedAt *time.Time
			var state string
			var k contentref.ContentKey
			if err := tx.QueryRow(ctx, `UPDATE `+c.s.t.comments+` SET author_hidden=$3
WHERE tenant_id=$1 AND id=$2 RETURNING reply_to_id::text,deleted_at,moderation,`+keyCols, rt.tenant, id, hidden).
				Scan(&replyTo, &deletedAt, &state, &k.TenantID, &k.ContentKind, &k.ContentID, versionid.Into(&k.ContentVersionID)); err != nil {
				return err
			}
			if state != ModerationApproved || deletedAt != nil {
				continue
			}
			if replyTo == nil {
				rollups[k] += delta
			} else if err := c.count(ctx, tx, replyTo, k, delta); err != nil {
				return err
			}
			if hidden {
				err = c.withdrawNotices(ctx, tx, id)
			} else {
				err = c.restoreNotices(ctx, tx, id)
			}
			if err != nil {
				return err
			}
		}
		keys := slices.Collect(maps.Keys(rollups))
		slices.SortFunc(keys, func(a, b contentref.ContentKey) int {
			return strings.Compare(a.ContentKind+"\x00"+a.ContentID+"\x00"+a.ContentVersionID, b.ContentKind+"\x00"+b.ContentID+"\x00"+b.ContentVersionID)
		})
		for _, k := range keys {
			if err := bumpCounts(ctx, tx, c.s, k, 0, 0, 0, rollups[k]); err != nil {
				return err
			}
		}
		return nil
	})
}

// AnonymizeAccount permanently removes account attribution without removing
// comments or contribution rows. Counters are unchanged.
func (rt *Runtime) AnonymizeAccount(ctx context.Context, actorID, anonymousID string) error {
	if strings.TrimSpace(actorID) == "" {
		return badRequest("subject id is required")
	}
	if _, err := uuid.Parse(anonymousID); err != nil {
		return badRequest("anonymous actor must be a random UUID")
	}
	err := pgx.BeginFunc(ctx, rt.store.pool, func(tx pgx.Tx) error {
		if err := rt.lockErasureSubject(ctx, tx, actorID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO `+rt.erasedSubjectsTable()+` (tenant_id,actor_id,engine_cleanup_complete)
VALUES ($1,$2,false) ON CONFLICT (tenant_id,actor_id) DO NOTHING`, rt.tenant, actorID); err != nil {
			return err
		}
		s := rt.store
		for _, q := range []string{
			`UPDATE ` + s.t.reactions + ` SET user_id=NULL,ip=NULL,anonymous_actor_id=$3 WHERE tenant_id=$1 AND user_id=$2`,
			`UPDATE ` + s.t.favorites + ` SET user_id=NULL,anonymous_actor_id=$3 WHERE tenant_id=$1 AND user_id=$2`,
			`UPDATE ` + s.t.pollVotes + ` SET user_id=NULL,ip=NULL,anonymous_actor_id=$3 WHERE tenant_id=$1 AND user_id=$2`,
		} {
			if _, err := tx.Exec(ctx, q, rt.tenant, actorID, anonymousID); err != nil {
				return err
			}
		}
		for _, q := range []string{
			`DELETE FROM ` + pgx.Identifier{rt.schema, "content_comment_notifications"}.Sanitize() + ` WHERE tenant_id=$1 AND recipient=$2`,
			`UPDATE ` + s.t.pollAnswers + ` SET actor_id=NULL WHERE tenant_id=$1 AND actor_id=$2`,
			`UPDATE ` + s.t.comments + ` SET user_id=NULL,anon_name='<Deleted>' WHERE tenant_id=$1 AND user_id=$2`,
			`DELETE FROM ` + rt.hiddenAuthorsTable() + ` WHERE tenant_id=$1 AND actor_id=$2`,
			`UPDATE ` + s.t.comments + ` SET moderated_by=NULL WHERE tenant_id=$1 AND moderated_by=$2`,
			`DELETE FROM ` + s.t.commentBans + ` WHERE tenant_id=$1 AND (user_id=$2 OR scope='owner:' || $2)`,
			`UPDATE ` + s.t.commentBans + ` SET banned_by='' WHERE tenant_id=$1 AND banned_by=$2`,
			`UPDATE ` + s.t.ipBans + ` SET banned_by='' WHERE tenant_id=$1 AND banned_by=$2`,
			`UPDATE ` + s.t.bannedWords + ` SET created_by='' WHERE tenant_id=$1 AND created_by=$2`,
			`DELETE FROM ` + pgx.Identifier{rt.schema, "content_creators"}.Sanitize() + ` WHERE tenant_id=$1 AND user_id=$2`,
			`UPDATE ` + s.t.items + ` SET created_by=NULL WHERE tenant_id=$1 AND created_by=$2`,
			`UPDATE ` + s.t.entities + ` SET owner_user_id=NULL WHERE tenant_id=$1 AND owner_user_id=$2`,
			`UPDATE ` + pgx.Identifier{rt.schema, "content_channel_reposts"}.Sanitize() + ` SET actor_id=NULL WHERE tenant_id=$1 AND actor_id=$2`,
			`DELETE FROM ` + pgx.Identifier{rt.schema, "content_filter_prefs"}.Sanitize() + ` WHERE tenant_id=$1 AND user_id=$2`,
		} {
			if _, err := tx.Exec(ctx, q, rt.tenant, actorID); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	// Every app consumes the terminal event and fences its own inbox scope.
	// A global fence would block a sibling's startup before its consumer could
	// finish that sibling's still-pending anonymization.
	if rt.eraseAccountReceipts != nil {
		if err := rt.eraseAccountReceipts(ctx, []string{actorID}); err != nil {
			return err
		}
	}
	if rt.providerEraser != nil {
		return rt.providerEraser.EraseSubjects(ctx, rt.tenant, []string{actorID})
	}
	return nil
}
