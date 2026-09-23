package content

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/contentkit/contentref"
)

// ProviderDataEraser deletes personal data retained by optional moderator and
// classifier implementations. Success means a durable tenant/subject fence is
// installed: Classify/Screen calls begun before erasure cannot recreate data
// when they complete later. Merely queuing a delete is not success. Calls are
// idempotent; implementations must preserve fences across restore/restart.
// A host using multiple retaining providers must implement a composite eraser.
type ProviderDataEraser interface {
	EraseSubjects(ctx context.Context, tenant string, actorIDs []string) error
}

// ErrSubjectErased is returned when an erased account tries to write again.
var ErrSubjectErased = fmt.Errorf("content: subject erased")

// lockErasureSubject serializes source writes with permanent erasure. Empty
// subjects are anonymous and cannot be erased.
// Call before any content row locks, consistently across all write paths.
func (rt *Runtime) lockErasureSubject(ctx context.Context, tx pgx.Tx, actorID string) error {
	if actorID == "" {
		return nil
	}
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, rt.schema+"\x1ferasure\x1f"+rt.tenant+"\x1f"+actorID)
	return err
}
func (rt *Runtime) checkSubjectNotErased(ctx context.Context, q querier, actorID string) error {
	if actorID == "" {
		return nil
	}
	var erased bool
	err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM `+rt.erasedSubjectsTable()+` WHERE tenant_id=$1 AND actor_id=$2)`, rt.tenant, actorID).Scan(&erased)
	if err != nil {
		return err
	}
	if erased {
		return ErrSubjectErased
	}
	return nil
}
func (rt *Runtime) guardErasedSubject(ctx context.Context, tx pgx.Tx, actorID string) error {
	if err := rt.lockErasureSubject(ctx, tx, actorID); err != nil {
		return err
	}
	return rt.checkSubjectNotErased(ctx, tx, actorID)
}
func (rt *Runtime) erasedSubjectsTable() string {
	return pgx.Identifier{rt.schema, "content_erased_subjects"}.Sanitize()
}

// EraseSubjects fences future authenticated content writes, removes reactions,
// favorites and poll votes/answers atomically,
// and removes unpublished held/rejected/draft/scheduled payloads and moderation
// provenance. A previously published item retains its last approved payload
// without publishing it again; its unpublished replacement is erased. Never-published items become
// tombstones. Published content and reply structure stay under host policy.
//
// The source transaction commits BEFORE calling the external eraser. A provider
// failure must keep the HOST's downstream erasure obligation pending for retry.
// AuthKit acknowledgement means durable LOCAL acceptance of that obligation;
// do not delay that acknowledgement until this method or remote cleanup succeeds.
// Nil ProviderDataEraser means configured policy ports retain no external
// personal data. Retaining providers must configure an eraser, including while offline.
func (rt *Runtime) EraseSubjects(ctx context.Context, actorIDs []string) error {
	ids := append([]string(nil), actorIDs...)
	sort.Strings(ids)
	compact := ids[:0]
	for _, id := range ids {
		if strings.TrimSpace(id) == "" {
			return badRequest("subject id is required")
		}
		if len(compact) == 0 || compact[len(compact)-1] != id {
			compact = append(compact, id)
		}
	}
	ids = compact
	if len(ids) == 0 {
		return nil
	}
	tx, err := rt.store.beginMutation(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	for _, id := range ids {
		if err := rt.lockErasureSubject(ctx, tx, id); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO `+rt.erasedSubjectsTable()+` (tenant_id,actor_id) VALUES ($1,$2) ON CONFLICT DO NOTHING`, rt.tenant, id); err != nil {
			return err
		}
	}
	if err := rt.eraseInteractions(ctx, tx, ids); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM `+rt.store.t.pollAnswers+` WHERE tenant_id=$1 AND actor_id=ANY($2)`, rt.tenant, ids); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE `+rt.store.t.comments+` SET body=coalesce(published_body,''), user_id=CASE WHEN published_body IS NOT NULL THEN user_id ELSE NULL END, anon_name=CASE WHEN published_body IS NOT NULL THEN anon_name ELSE '[deleted]' END, moderation='rejected', moderation_reason=NULL, moderation_verdict=NULL, moderated_by=NULL, moderated_at=NULL, deleted_at=CASE WHEN published_body IS NOT NULL THEN deleted_at ELSE coalesce(deleted_at,clock_timestamp()) END, moderation_revision=moderation_revision+1, updated_at=clock_timestamp()
 WHERE tenant_id=$1 AND user_id=ANY($2) AND moderation IN ('held','rejected')`, rt.tenant, ids); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE `+rt.store.t.posts+` SET title=coalesce(published_content->>'title',''), body=coalesce(published_content->>'body',''), excerpt=published_content->>'excerpt', author_id=CASE WHEN published_content IS NOT NULL THEN author_id ELSE '' END, moderation='rejected', moderation_reason=NULL, moderation_verdict=NULL, moderated_by=NULL, moderated_at=NULL, deleted_at=CASE WHEN published_content IS NOT NULL THEN deleted_at ELSE coalesce(deleted_at,clock_timestamp()) END, moderation_revision=moderation_revision+1, updated_at=clock_timestamp()
 WHERE tenant_id=$1 AND author_id=ANY($2) AND (moderation IN ('held','rejected') OR is_draft OR live_at > clock_timestamp())`, rt.tenant, ids); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	eraseLocalPolicy(ctx, rt.moderator, rt.tenant, ids)
	if rt.providerEraser != nil {
		return rt.providerEraser.EraseSubjects(ctx, rt.tenant, ids)
	}
	return nil
}

// eraseInteractions runs after sorted subject fences are locked. All affected
// authored rows are locked first in table/id order, then rollups in content-key
// order. This matches comment/post reaction writes and avoids opposite lock
// ordering across concurrent erasures of different subjects on shared content.
func (rt *Runtime) eraseInteractions(ctx context.Context, tx pgx.Tx, ids []string) error {
	s := rt.store
	for _, target := range []struct{ table, author, kind string }{{s.t.comments, "user_id", KindComment}, {s.t.posts, "author_id", KindPost}} {
		unpublished := "moderation IN ('held','rejected')"
		if target.kind == KindPost {
			unpublished = "(" + unpublished + " OR is_draft OR live_at > clock_timestamp())"
		}
		rows, err := tx.Query(ctx, `SELECT id::text FROM `+target.table+` WHERE tenant_id=$1 AND (
   (`+target.author+`=ANY($2) AND `+unpublished+`) OR id::text IN (
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
	rows, err = tx.Query(ctx, `DELETE FROM `+s.t.favorites+` WHERE tenant_id=$1 AND user_id=ANY($2) RETURNING `+keyCols+`,value`, rt.tenant, ids)
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
		d.favorites += int(value)
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
	return nil
}
