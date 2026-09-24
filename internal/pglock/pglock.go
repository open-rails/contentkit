// Package pglock holds Postgres session advisory locks on dedicated
// connections, so the holder's work may use every connection of its pool.
package pglock

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Acquire locks key on a new connection configured like pool's. Without wait
// it returns ok=false while another session holds key. release closes the
// connection, which ends the lock.
func Acquire(ctx context.Context, pool *pgxpool.Pool, key string, wait bool) (release func(), ok bool, err error) {
	conn, err := pgx.ConnectConfig(ctx, pool.Config().ConnConfig)
	if err != nil {
		return nil, false, err
	}
	release = func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = conn.Close(ctx)
	}
	ok = true
	if wait {
		_, err = conn.Exec(ctx, "SELECT pg_advisory_lock(hashtextextended($1, 0))", key)
	} else {
		err = conn.QueryRow(ctx, "SELECT pg_try_advisory_lock(hashtextextended($1, 0))", key).Scan(&ok)
	}
	if err != nil || !ok {
		release()
		return nil, false, err
	}
	return release, true, nil
}
