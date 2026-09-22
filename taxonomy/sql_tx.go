package taxonomy

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

// WithSQLTx borrows an existing database/sql transaction using pgx's stdlib
// driver. The host owns commit, rollback and savepoints. Catalog writes, counts
// and dirty documents participate in that same transaction.
func (s *Store) WithSQLTx(tx *sql.Tx) *Store {
	c := *s
	c.tx = sqlQuerier{tx: tx}
	return &c
}

type rowScanner interface{ Scan(...any) error }
type resultRows interface {
	rowScanner
	Next() bool
	Err() error
	Close()
}
type pgxQuerier interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}
type nativeQuerier struct{ pgxQuerier }

func (q nativeQuerier) Query(ctx context.Context, query string, args ...any) (resultRows, error) {
	return q.pgxQuerier.Query(ctx, query, args...)
}
func (q nativeQuerier) QueryRow(ctx context.Context, query string, args ...any) rowScanner {
	return q.pgxQuerier.QueryRow(ctx, query, args...)
}

func collectRows[T any](rows resultRows, scan func(rowScanner) (T, error)) ([]T, error) {
	defer rows.Close()
	result := make([]T, 0)
	for rows.Next() {
		value, err := scan(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, value)
	}
	return result, rows.Err()
}
func scanString(row rowScanner) (string, error) {
	var value string
	err := row.Scan(&value)
	return value, err
}

type sqlQuerier struct{ tx *sql.Tx }

func (q sqlQuerier) rewrite(ctx context.Context, query string, args []any) (string, []any, error) {
	if q.tx == nil {
		return "", nil, fmt.Errorf("%w: nil SQL transaction", ErrInvalid)
	}
	if len(args) == 1 {
		if named, ok := args[0].(pgx.NamedArgs); ok {
			return named.RewriteQuery(ctx, nil, query, nil)
		}
	}
	return query, args, nil
}
func (q sqlQuerier) Exec(ctx context.Context, query string, args ...any) (pgconn.CommandTag, error) {
	query, args, err := q.rewrite(ctx, query, args)
	if err != nil {
		return pgconn.CommandTag{}, err
	}
	result, err := q.tx.ExecContext(ctx, query, args...)
	if err != nil {
		return pgconn.CommandTag{}, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return pgconn.CommandTag{}, err
	}
	// database/sql exposes the affected count, not the PostgreSQL command verb.
	// Taxonomy consumes only RowsAffected; search.MarkDirty ignores the tag.
	return pgconn.NewCommandTag(strconv.FormatInt(affected, 10)), nil
}
func (q sqlQuerier) Query(ctx context.Context, query string, args ...any) (resultRows, error) {
	query, args, err := q.rewrite(ctx, query, args)
	if err != nil {
		return nil, err
	}
	rows, err := q.tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	return sqlRows{rows}, nil
}
func (q sqlQuerier) QueryRow(ctx context.Context, query string, args ...any) rowScanner {
	query, args, err := q.rewrite(ctx, query, args)
	if err != nil {
		return errorRow{err}
	}
	return sqlRow{q.tx.QueryRowContext(ctx, query, args...)}
}

type errorRow struct{ err error }

func (r errorRow) Scan(...any) error { return r.err }

type sqlRow struct{ row *sql.Row }

func (r sqlRow) Scan(dest ...any) error {
	err := r.row.Scan(sqlScanTargets(dest)...)
	if errors.Is(err, sql.ErrNoRows) {
		return pgx.ErrNoRows
	}
	return err
}

type sqlRows struct{ *sql.Rows }

func (r sqlRows) Close()                 { _ = r.Rows.Close() }
func (r sqlRows) Scan(dest ...any) error { return r.Rows.Scan(sqlScanTargets(dest)...) }
func sqlScanTargets(dest []any) []any {
	out := append([]any(nil), dest...)
	for i, d := range out {
		t := reflect.TypeOf(d)
		if t != nil && t.Kind() == reflect.Pointer && t.Elem().Kind() == reflect.Slice && t.Elem().Elem().Kind() != reflect.Uint8 {
			out[i] = pgtype.NewMap().SQLScanner(d)
		}
	}
	return out
}
