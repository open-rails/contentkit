package content

import (
	"context"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/contentkit/contentref"
)

// eraseInteractions runs after sorted subject fences are locked. All affected
// authored rows are locked first in table/id order, then rollups in content-key
// order. This matches comment/post reaction writes and avoids opposite lock
// ordering across concurrent erasures of different subjects on shared content.
func (rt *Runtime) eraseInteractions(ctx context.Context, tx pgx.Tx, ids []string) error {
	s := rt.store
	for _, target := range []struct{ table, author, kind string }{{s.t.comments, "user_id", KindComment}, {s.t.posts, "author_id", KindPost}} {
		private := "moderation IN ('held','rejected')"
		if target.kind == KindPost {
			private = "(" + private + " OR is_draft OR live_at > clock_timestamp())"
		}
		rows, err := tx.Query(ctx, `SELECT id::text FROM `+target.table+` WHERE tenant_id=$1 AND (
   (`+target.author+`=ANY($2) AND `+private+`) OR id::text IN (
    SELECT content_id FROM `+s.t.reactions+` WHERE tenant_id=$1 AND user_id=ANY($2) AND content_kind=$3 AND content_version_id=''))
   ORDER BY id FOR UPDATE`, rt.tenant, ids, target.kind)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
	}
	type delta struct{ likes, dislikes, favorites int }
	changes := map[contentref.ContentKey]delta{}
	rows, err := tx.Query(ctx, `DELETE FROM `+s.t.reactions+` WHERE tenant_id=$1 AND user_id=ANY($2) RETURNING `+keyCols+`,value`, rt.tenant, ids)
	if err != nil {
		return err
	}
	for rows.Next() {
		var k contentref.ContentKey
		var value int16
		if err := rows.Scan(&k.TenantID, &k.ContentKind, &k.ContentID, &k.ContentVersionID, &value); err != nil {
			rows.Close()
			return err
		}
		d := changes[k]
		d.likes += b2i(value == 1)
		d.dislikes += b2i(value == -1)
		changes[k] = d
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	rows, err = tx.Query(ctx, `DELETE FROM `+s.t.favorites+` WHERE tenant_id=$1 AND user_id=ANY($2) RETURNING `+keyCols, rt.tenant, ids)
	if err != nil {
		return err
	}
	for rows.Next() {
		var k contentref.ContentKey
		if err := rows.Scan(&k.TenantID, &k.ContentKind, &k.ContentID, &k.ContentVersionID); err != nil {
			rows.Close()
			return err
		}
		d := changes[k]
		d.favorites++
		changes[k] = d
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	keys := make([]contentref.ContentKey, 0, len(changes))
	for k := range changes {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if a.ContentKind != b.ContentKind {
			return a.ContentKind < b.ContentKind
		}
		if a.ContentID != b.ContentID {
			return a.ContentID < b.ContentID
		}
		return a.ContentVersionID < b.ContentVersionID
	})
	for _, k := range keys {
		d := changes[k]
		// UPDATE only: lifecycle deletion may already have removed a rollup.
		if _, err := tx.Exec(ctx, `UPDATE `+s.t.counts+` SET likes=GREATEST(likes-$5,0),dislikes=GREATEST(dislikes-$6,0),favorites=GREATEST(favorites-$7,0),updated_at=clock_timestamp() WHERE `+keyPred(1), append(keyArgs(k), d.likes, d.dislikes, d.favorites)...); err != nil {
			return err
		}
		if k.ContentVersionID != "" || d.likes == 0 && d.dislikes == 0 {
			continue
		}
		switch k.ContentKind {
		case KindComment:
			if _, err := tx.Exec(ctx, `UPDATE `+s.t.comments+` SET likes=GREATEST(likes-$3,0),dislikes=GREATEST(dislikes-$4,0) WHERE tenant_id=$1 AND id::text=$2`, rt.tenant, k.ContentID, d.likes, d.dislikes); err != nil {
				return err
			}
		case KindPost:
			if _, err := tx.Exec(ctx, `UPDATE `+s.t.posts+` SET total_likes=GREATEST(total_likes-$3,0),total_dislikes=GREATEST(total_dislikes-$4,0) WHERE tenant_id=$1 AND id=$2`, rt.tenant, k.ContentID, d.likes, d.dislikes); err != nil {
				return err
			}
		}
	}
	rows, err = tx.Query(ctx, `DELETE FROM `+s.t.pollVotes+` v USING `+s.t.pollQuestions+` q WHERE v.question_id=q.id AND q.tenant_id=$1 AND v.user_id=ANY($2) RETURNING v.option_id::text`, rt.tenant, ids)
	if err != nil {
		return err
	}
	votes := map[string]int{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		votes[id]++
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	options := make([]string, 0, len(votes))
	for id := range votes {
		options = append(options, id)
	}
	sort.Strings(options)
	for _, id := range options {
		if _, err := tx.Exec(ctx, `UPDATE `+s.t.pollOptions+` SET vote_count=GREATEST(vote_count-$2,0) WHERE id=$1`, id, votes[id]); err != nil {
			return err
		}
	}
	for _, table := range []string{s.t.preferenceSnapshots, s.t.preferenceArchive} {
		if _, err := tx.Exec(ctx, `DELETE FROM `+table+` WHERE tenant_id=$1 AND actor_id=ANY($2)`, rt.tenant, ids); err != nil {
			return err
		}
	}
	return nil
}
