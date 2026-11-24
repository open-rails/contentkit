// Package subjectfence serializes every write that stores an account id with
// that account's permanent erasure. A writer locks the subjects before any
// other row lock and refuses an erased one; anonymization holds the same lock.
package subjectfence

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ErrErased is a write by or naming an erased account.
var ErrErased = errors.New("content: subject erased")

// Tx is the writer's transaction (pgx.Tx).
type Tx interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Key is the advisory lock key of tenant's subject id in schema.
func Key(schema, tenant, id string) string { return schema + "\x1ferasure\x1f" + tenant + "\x1f" + id }

// Lock takes the subjects' erasure locks in id order and returns the
// distinct non-empty ids. Empty ids are anonymous.
func Lock(ctx context.Context, tx Tx, schema, tenant string, ids ...string) ([]string, error) {
	ids = slices.Compact(slices.Sorted(slices.Values(slices.DeleteFunc(slices.Clone(ids), func(id string) bool { return id == "" }))))
	if len(ids) == 0 {
		return nil, nil
	}
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1 || u, 0)) FROM unnest($2::text[]) AS u ORDER BY u`,
		Key(schema, tenant, ""), ids)
	return ids, err
}

// Guard locks the subjects and refuses an erased one with ErrErased.
func Guard(ctx context.Context, tx Tx, schema, tenant string, ids ...string) error {
	ids, err := Lock(ctx, tx, schema, tenant, ids...)
	if err != nil || len(ids) == 0 {
		return err
	}
	var erased string
	if err := tx.QueryRow(ctx, `SELECT coalesce(min(actor_id), '') FROM `+pgx.Identifier{schema, "content_erased_subjects"}.Sanitize()+`
 WHERE tenant_id = $1 AND actor_id = ANY($2)`, tenant, ids).Scan(&erased); err != nil {
		return err
	}
	if erased != "" {
		return fmt.Errorf("%w: %s", ErrErased, erased)
	}
	return nil
}

// Erased reports whether id is erased, without locking: for read-time checks
// that store nothing.
func Erased(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, schema, tenant, id string) (bool, error) {
	if id == "" {
		return false, nil
	}
	var erased bool
	err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM `+pgx.Identifier{schema, "content_erased_subjects"}.Sanitize()+` WHERE tenant_id=$1 AND actor_id=$2)`, tenant, id).Scan(&erased)
	return erased, err
}
