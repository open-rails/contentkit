package content

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/versionid"
	"github.com/open-rails/helpers/notify"
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

// provenance is by, or "" once by is erased: a moderation rule set by an
// erased moderator stands without their id. Lock by's subject fence first.
func (rt *Runtime) provenance(ctx context.Context, tx pgx.Tx, by string) (string, error) {
	err := rt.checkSubjectNotErased(ctx, tx, by)
	if errors.Is(err, ErrSubjectErased) {
		return "", nil
	}
	return by, err
}
func (rt *Runtime) erasedSubjectsTable() string {
	return pgx.Identifier{rt.schema, "content_erased_subjects"}.Sanitize()
}

// EraseSubjects is coordinated tenant-account deletion. With a shared account
// inbox it fences that recipient across every app, so it must not be used for
// an app-only reset or a content-only restore.
//
// It first erases and fences the user inbox, then fences future
// authenticated content writes, removes reactions,
// favorites, poll votes/answers and comment bans of or by them atomically,
// and removes unpublished held/rejected comment payloads and moderation
// provenance. A previously published comment retains its last approved
// payload without publishing it again; its unpublished replacement is erased.
// Never-published comments become tombstones. Published content and reply
// structure stay under host policy.
//
// The source transaction commits BEFORE calling the external eraser. A provider
// failure must keep the HOST's downstream erasure obligation pending for retry.
// AuthKit acknowledgement means durable LOCAL acceptance of that obligation;
// do not delay that acknowledgement until this method or remote cleanup succeeds.
// Nil ProviderDataEraser means configured policy ports retain no external
// personal data. Retaining providers must configure an eraser, including while offline.
func (rt *Runtime) EraseSubjects(ctx context.Context, actorIDs []string) error {
	return rt.eraseSubjects(ctx, actorIDs, true)
}

// EraseAppSubjects permanently fences and erases personal activity in this
// app only. Shared account notifications and avatars are outside this boundary.
func (rt *Runtime) EraseAppSubjects(ctx context.Context, actorIDs []string) error {
	return rt.eraseSubjects(ctx, actorIDs, false)
}

func (rt *Runtime) eraseSubjects(ctx context.Context, actorIDs []string, account bool) error {
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
	// Erase and fence the inbox before content work; retries are idempotent.
	// Keeping the two fences in separate transactions avoids locking recipients
	// in the reverse order of a comment producer.
	for _, id := range ids {
		erase := rt.Notifications.EraseAppRecipient
		if account {
			erase = rt.Notifications.EraseRecipient
		}
		if err := erase(ctx, notify.Recipient{Audience: notify.AudienceUser, ID: id}); err != nil {
			return fmt.Errorf("content: erase notifications: %w", err)
		}
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
		if _, err := tx.Exec(ctx, `INSERT INTO `+rt.erasedSubjectsTable()+` (tenant_id,actor_id,engine_cleanup_complete) VALUES ($1,$2,false) ON CONFLICT (tenant_id,actor_id) DO UPDATE SET engine_cleanup_complete=false`, rt.tenant, id); err != nil {
			return err
		}
	}
	if err := rt.eraseInteractions(ctx, tx, ids); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM `+rt.store.t.pollAnswers+` WHERE tenant_id=$1 AND actor_id=ANY($2)`, rt.tenant, ids); err != nil {
		return err
	}
	// Comment bans of or by the subjects: bans of them and their owner scopes go;
	// global bans they set keep standing without their id.
	scopes := make([]string, len(ids))
	for i, id := range ids {
		scopes[i] = OwnerScope(id)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM `+rt.store.t.commentBans+` WHERE tenant_id=$1 AND (user_id=ANY($2) OR scope=ANY($3))`, rt.tenant, ids, scopes); err != nil {
		return err
	}
	// Moderation rules they set keep standing without their id.
	for _, q := range []string{`UPDATE ` + rt.store.t.commentBans + ` SET banned_by='' WHERE tenant_id=$1 AND banned_by=ANY($2)`,
		`UPDATE ` + rt.store.t.ipBans + ` SET banned_by='' WHERE tenant_id=$1 AND banned_by=ANY($2)`,
		`UPDATE ` + rt.store.t.bannedWords + ` SET created_by='' WHERE tenant_id=$1 AND created_by=ANY($2)`} {
		if _, err := tx.Exec(ctx, q, rt.tenant, ids); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE `+rt.store.t.comments+` SET body=coalesce(published_body,''), user_id=CASE WHEN published_body IS NOT NULL THEN user_id ELSE NULL END, anon_name=CASE WHEN published_body IS NOT NULL THEN anon_name ELSE '[deleted]' END, moderation='rejected', moderation_reason=NULL, moderation_verdict=NULL, moderated_by=NULL, moderated_at=NULL, deleted_at=CASE WHEN published_body IS NOT NULL THEN deleted_at ELSE coalesce(deleted_at,clock_timestamp()) END, moderation_revision=moderation_revision+1, updated_at=clock_timestamp()
 WHERE tenant_id=$1 AND user_id=ANY($2) AND moderation IN ('held','rejected')`, rt.tenant, ids); err != nil {
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
	rows, err := tx.Query(ctx, `SELECT DISTINCT content_id FROM `+s.t.reactions+` WHERE tenant_id=$1 AND user_id=ANY($2) AND content_kind=$3 AND content_version_id IS NULL`, rt.tenant, ids, KindComment)
	if err != nil {
		return err
	}
	reactedIDs, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	// A reaction key is text: only a canonical UUID names a comment. Casting
	// any other key would match a spelling the comment's key never had, or
	// abort the erasure on a malformed one.
	reacted := map[string]bool{}
	for _, k := range reactedIDs {
		if uuidRe.MatchString(k) && k == strings.ToLower(k) {
			reacted[k] = true
		}
	}
	if _, err := tx.Exec(ctx, `SELECT 1 FROM `+s.t.comments+` WHERE tenant_id=$1 AND (
   (user_id=ANY($2) AND moderation IN ('held','rejected')) OR id=ANY($3::uuid[]))
   ORDER BY id FOR UPDATE`, rt.tenant, ids, slices.Collect(maps.Keys(reacted))); err != nil {
		return err
	}
	type delta struct{ likes, dislikes, favorites int }
	changes := map[contentref.ContentKey]delta{}
	rows, err = tx.Query(ctx, `DELETE FROM `+s.t.reactions+` WHERE tenant_id=$1 AND user_id=ANY($2) RETURNING `+keyCols+`,value`, rt.tenant, ids)
	if err != nil {
		return err
	}
	for rows.Next() {
		var k contentref.ContentKey
		var value int16
		if err := rows.Scan(&k.TenantID, &k.ContentKind, &k.ContentID, versionid.Into(&k.ContentVersionID), &value); err != nil {
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
		if err := rows.Scan(&k.TenantID, &k.ContentKind, &k.ContentID, versionid.Into(&k.ContentVersionID), &value); err != nil {
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
	var commentIDs []string
	var commentLikes, commentDislikes []int
	for _, k := range keys {
		d := changes[k]
		// UPDATE only: lifecycle deletion may already have removed a rollup.
		if _, err := tx.Exec(ctx, `UPDATE `+s.t.counts+` SET likes=GREATEST(likes-$5,0),dislikes=GREATEST(dislikes-$6,0),favorites=GREATEST(favorites-$7,0),updated_at=clock_timestamp() WHERE `+keyPred(1), append(keyArgs(k), d.likes, d.dislikes, d.favorites)...); err != nil {
			return err
		}
		if k.ContentKind == KindComment && k.ContentVersionID == "" && reacted[k.ContentID] && (d.likes != 0 || d.dislikes != 0) {
			commentIDs = append(commentIDs, k.ContentID)
			commentLikes = append(commentLikes, d.likes)
			commentDislikes = append(commentDislikes, d.dislikes)
		}
	}
	if len(commentIDs) > 0 {
		if _, err := tx.Exec(ctx, `UPDATE `+s.t.comments+` c SET likes=GREATEST(c.likes-d.likes,0),dislikes=GREATEST(c.dislikes-d.dislikes,0)
 FROM unnest($2::uuid[],$3::int[],$4::int[]) AS d(id,likes,dislikes) WHERE c.tenant_id=$1 AND c.id=d.id`, rt.tenant, commentIDs, commentLikes, commentDislikes); err != nil {
			return err
		}
	}
	rows, err = tx.Query(ctx, `DELETE FROM `+s.t.pollVotes+` WHERE tenant_id=$1 AND user_id=ANY($2) RETURNING option_id::text`, rt.tenant, ids)
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
		if _, err := tx.Exec(ctx, `UPDATE `+s.t.pollOptions+` SET vote_count=GREATEST(vote_count-$2,0) WHERE tenant_id=$3 AND id=$1`, id, votes[id], rt.tenant); err != nil {
			return err
		}
	}
	return nil
}
