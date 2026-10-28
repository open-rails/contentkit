package versionstore

import (
	"context"
	"slices"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/contentkit/internal/pglock"
)

// LockSources serializes source selection and catalog segment writes before
// either takes row locks. The locks live on the shared lock session, leaving
// the caller's pool available for manifest commits and catalog notifications.
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
	releases := make([]func(), 0, len(ids))
	release := func() {
		for _, r := range slices.Backward(releases) {
			r()
		}
	}
	for _, id := range ids {
		// Keep the original media.PGLocker namespace so existing publishers and
		// upgraded catalog writers contend on exactly the same advisory key.
		r, _, err := pglock.Acquire(ctx, pool, "contentkit:media:contentkit:versions:"+tenant+":"+id, true)
		if err != nil {
			release()
			return nil, err
		}
		releases = append(releases, r)
	}
	return release, nil
}
