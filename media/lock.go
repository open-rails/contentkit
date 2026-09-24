package media

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/contentkit/internal/pglock"
)

// PGLocker serializes manifest edits with a per-manifest session advisory lock
// in the host database: the fallback for backends without conditional PUT.
// The lock holds its own connection, never one of pool's, so an edit may use
// the pool (quota settlement) without starving it.
func PGLocker(pool *pgxpool.Pool) Locker { return pgLocker{pool} }

type pgLocker struct{ pool *pgxpool.Pool }

func (l pgLocker) Lock(ctx context.Context, key string) (func(), error) {
	release, _, err := pglock.Acquire(ctx, l.pool, "contentkit:media:"+key, true)
	return release, err
}
