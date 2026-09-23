package media

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PGLocker serializes manifest edits with a per-manifest session advisory lock
// in the host database: the fallback for backends without conditional PUT.
func PGLocker(pool *pgxpool.Pool) Locker { return pgLocker{pool} }

type pgLocker struct{ pool *pgxpool.Pool }

func (l pgLocker) Lock(ctx context.Context, key string) (func(), error) {
	conn, err := l.pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	lockKey := "contentkit:media:" + key
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock(hashtextextended($1, 0))", lockKey); err != nil {
		conn.Release()
		return nil, err
	}
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := conn.Exec(ctx, "SELECT pg_advisory_unlock(hashtextextended($1, 0))", lockKey); err != nil {
			// A connection that may still hold the lock must not return to the pool.
			_ = conn.Conn().Close(ctx)
		}
		conn.Release()
	}, nil
}
