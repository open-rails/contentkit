// Package pglock holds Postgres session advisory locks on dedicated
// connections, so the holder's work may use every connection of its pool.
package pglock

import (
	"context"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// A lock's session ends itself once idle this long, so a holder whose client
// vanished without closing the connection (a dead node or a dropped tunnel)
// frees the lock instead of blocking every waiter until TCP gives up. A live
// holder pings well within it.
const idleLimit = 2 * time.Minute

// Acquire locks key on a new connection configured like pool's. Without wait
// it returns ok=false while another session holds key. release closes the
// connection, which ends the lock.
func Acquire(ctx context.Context, pool *pgxpool.Pool, key string, wait bool) (release func(), ok bool, err error) {
	return acquire(ctx, pool, key, wait, idleLimit, idleLimit/4)
}

func acquire(ctx context.Context, pool *pgxpool.Pool, key string, wait bool, idle, ping time.Duration) (release func(), ok bool, err error) {
	cfg := pool.Config().ConnConfig.Copy()
	if cfg.RuntimeParams == nil {
		cfg.RuntimeParams = map[string]string{}
	}
	cfg.RuntimeParams["idle_session_timeout"] = strconv.FormatInt(idle.Milliseconds(), 10)
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, false, err
	}
	var mu sync.Mutex
	closeConn := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		mu.Lock()
		defer mu.Unlock()
		_ = conn.Close(ctx)
	}
	ok = true
	if wait {
		_, err = conn.Exec(ctx, "SELECT pg_advisory_lock(hashtextextended($1, 0))", key)
	} else {
		err = conn.QueryRow(ctx, "SELECT pg_try_advisory_lock(hashtextextended($1, 0))", key).Scan(&ok)
	}
	if err != nil || !ok {
		closeConn()
		return nil, false, err
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		if ping <= 0 {
			return
		}
		t := time.NewTicker(ping)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				mu.Lock()
				ctx, cancel := context.WithTimeout(context.Background(), ping)
				_, _ = conn.Exec(ctx, "SELECT 1")
				cancel()
				mu.Unlock()
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			close(stop)
			<-done
			closeConn()
		})
	}, true, nil
}
