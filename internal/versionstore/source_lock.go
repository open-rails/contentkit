package versionstore

import (
	"context"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// LockSources serializes source selection and catalog segment writes before
// either takes row locks. A batch uses one dedicated connection, leaving the
// caller's pool available for manifest commits and catalog notifications.
func LockSources(ctx context.Context, pool *pgxpool.Pool, tenant string, vers []string) (func(), error) {
	if len(vers) == 0 {
		return func() {}, nil
	}
	if pool == nil || tenant == "" {
		return nil, ErrInvalid
	}
	ids := slices.Clone(vers)
	slices.Sort(ids)
	ids = slices.Compact(ids)
	for _, id := range ids {
		if id == "" {
			return nil, ErrInvalid
		}
	}
	conn, err := pgx.ConnectConfig(ctx, pool.Config().ConnConfig)
	if err != nil {
		return nil, err
	}
	release := func() {
		clean, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = conn.Close(clean)
	}
	for _, id := range ids {
		// Keep the original media.PGLocker namespace so existing publishers and
		// upgraded catalog writers contend on exactly the same advisory key.
		key := "contentkit:media:contentkit:versions:" + tenant + ":" + id
		if _, err = conn.Exec(ctx, "SELECT pg_advisory_lock(hashtextextended($1, 0))", key); err != nil {
			release()
			return nil, err
		}
	}
	return release, nil
}
