package contentstore

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

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
