package content

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
)

// PrivateDataEraser deletes personal data retained by optional moderator and
// classifier implementations. Success means a durable tenant/subject fence is
// installed: Classify/Screen calls begun before erasure cannot recreate data
// when they complete later. Merely queuing a delete is not success. Calls are
// idempotent; implementations must preserve fences across restore/restart.
// A host using multiple retaining providers must implement a composite eraser.
type PrivateDataEraser interface {
	EraseSubjects(ctx context.Context, tenant string, actorIDs []string) error
}

// ErrSubjectErased is the permanent private-content write fence.
var ErrSubjectErased = fmt.Errorf("content: subject erased")

// lockPrivateSubject serializes source writes with permanent erasure. Empty
// subjects are anonymous and have no authenticated private-data identity.
// Call before any content row locks, consistently across all write paths.
func (rt *Runtime) lockPrivateSubject(ctx context.Context, tx pgx.Tx, actorID string) error {
	if actorID == "" {
		return nil
	}
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, rt.schema+"\x1fprivate\x1f"+rt.tenant+"\x1f"+actorID)
	return err
}
func (rt *Runtime) privateSubjectAllowed(ctx context.Context, q querier, actorID string) error {
	if actorID == "" {
		return nil
	}
	var erased bool
	err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM `+rt.privateFences()+` WHERE tenant_id=$1 AND actor_id=$2)`, rt.tenant, actorID).Scan(&erased)
	if err != nil {
		return err
	}
	if erased {
		return ErrSubjectErased
	}
	return nil
}
func (rt *Runtime) guardPrivateSubject(ctx context.Context, tx pgx.Tx, actorID string) error {
	if err := rt.lockPrivateSubject(ctx, tx, actorID); err != nil {
		return err
	}
	return rt.privateSubjectAllowed(ctx, tx, actorID)
}
func (rt *Runtime) privateFences() string {
	return pgx.Identifier{rt.schema, "content_private_subject_erasures"}.Sanitize()
}

// ErasePrivateSubjects fences future private submissions, removes poll answers,
// and removes private held/rejected payloads and moderation provenance. A
// previously published item retains its last approved payload without publishing
// it again; its private replacement is erased. Never-published items become
// tombstones. Published content and reply structure stay under host policy.
//
// The source transaction commits BEFORE calling the external eraser. A provider
// failure must keep the HOST's downstream erasure obligation pending for retry.
// AuthKit acknowledgement means durable LOCAL acceptance of that obligation;
// do not delay that acknowledgement until this method or remote cleanup succeeds.
// Nil PrivateDataEraser means configured policy ports retain no external personal
// data. Retaining providers must configure an eraser, including while offline.
func (rt *Runtime) ErasePrivateSubjects(ctx context.Context, actorIDs []string) error {
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
	tx, err := rt.store.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	for _, id := range ids {
		if err := rt.lockPrivateSubject(ctx, tx, id); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO `+rt.privateFences()+` (tenant_id,actor_id) VALUES ($1,$2) ON CONFLICT DO NOTHING`, rt.tenant, id); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM `+rt.store.t.pollAnswers+` WHERE tenant_id=$1 AND actor_id=ANY($2)`, rt.tenant, ids); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE `+rt.store.t.comments+` SET body=coalesce(published_body,''), user_id=CASE WHEN published_body IS NOT NULL THEN user_id ELSE NULL END, anon_name=CASE WHEN published_body IS NOT NULL THEN anon_name ELSE '[deleted]' END, moderation='rejected', moderation_reason=NULL, moderation_verdict=NULL, moderated_by=NULL, moderated_at=NULL, deleted_at=CASE WHEN published_body IS NOT NULL THEN deleted_at ELSE coalesce(deleted_at,clock_timestamp()) END, moderation_revision=moderation_revision+1, updated_at=clock_timestamp()
 WHERE tenant_id=$1 AND user_id=ANY($2) AND moderation IN ('held','rejected')`, rt.tenant, ids); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE `+rt.store.t.posts+` SET title=coalesce(published_content->>'title',''), body=coalesce(published_content->>'body',''), excerpt=published_content->>'excerpt', author_id=CASE WHEN published_content IS NOT NULL THEN author_id ELSE '' END, moderation='rejected', moderation_reason=NULL, moderation_verdict=NULL, moderated_by=NULL, moderated_at=NULL, deleted_at=CASE WHEN published_content IS NOT NULL THEN deleted_at ELSE coalesce(deleted_at,clock_timestamp()) END, moderation_revision=moderation_revision+1, updated_at=clock_timestamp()
 WHERE tenant_id=$1 AND author_id=ANY($2) AND moderation IN ('held','rejected')`, rt.tenant, ids); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	eraseLocalPolicy(ctx, rt.moderator, rt.tenant, ids)
	if rt.privateEraser != nil {
		return rt.privateEraser.EraseSubjects(ctx, rt.tenant, ids)
	}
	return nil
}
