// Package pglock holds Postgres session advisory locks on direct sessions of
// their own, outside the holder's pool: the work may use every connection of
// that pool, which may be a transaction pooler. A free key is taken on one
// session shared by every lock a Locks holds; only a key someone already holds
// is waited for on a connection of its own, so extra connections follow
// contention, not the number of locks held.
package pglock

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// A lock's session ends itself once idle this long, so a holder whose
	// client vanished without closing the connection (a dead node or a dropped
	// tunnel) frees the lock instead of blocking every waiter until TCP gives
	// up. A live holder pings well within it.
	idleLimit = 2 * time.Minute
	pingEvery = idleLimit / 4
	// A query that times out closes its connection, and with it every lock on
	// the session, so a lock query may run until the server would end the
	// session anyway.
	queryTimeout   = idleLimit - pingEvery
	connectTimeout = 15 * time.Second
)

// ErrContended asks callers to retry through their existing request or job path.
var ErrContended = errors.New("pglock: acquisition budget expired; retry")

// Locks takes one process's advisory locks on sessions dialed from a direct
// (unpooled) configuration.
type Locks struct {
	cfg *pgxpool.Config

	mu   sync.Mutex
	conn *pgx.Conn
	// held are the keys taken on conn. Advisory locks are re-entrant within a
	// session, so another goroutine wanting one waits on its own connection.
	held map[string]bool
}

// New dials lock sessions from cfg, which must reach Postgres directly: a
// transaction pooler would end a lock's session with its first statement.
func New(cfg *pgxpool.Config) *Locks {
	cfg = cfg.Copy()
	if cfg.ConnConfig.RuntimeParams == nil {
		cfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	cfg.ConnConfig.RuntimeParams["idle_session_timeout"] = strconv.FormatInt(idleLimit.Milliseconds(), 10)
	return &Locks{cfg: cfg, held: map[string]bool{}}
}

func (s *Locks) dial(ctx context.Context) (*pgx.Conn, error) {
	cc := s.cfg.ConnConfig.Copy()
	if s.cfg.BeforeConnect != nil {
		if err := s.cfg.BeforeConnect(ctx, cc); err != nil {
			return nil, err
		}
	}
	conn, err := pgx.ConnectConfig(ctx, cc)
	if err != nil {
		return nil, err
	}
	if s.cfg.AfterConnect != nil {
		if err := s.cfg.AfterConnect(ctx, conn); err != nil {
			_ = conn.Close(context.WithoutCancel(ctx))
			return nil, err
		}
	}
	return conn, nil
}

// Acquire locks key against every other holder, in this process or another.
// Without wait it returns ok=false while anyone else holds key; with it, it
// waits in Postgres's queue for key, where Postgres detects deadlocks.
func (s *Locks) Acquire(ctx context.Context, key string, wait bool) (release func(), ok bool, err error) {
	conn, ok, err := s.try(key)
	if err != nil {
		return nil, false, err
	}
	if ok {
		var once sync.Once
		return func() { once.Do(func() { s.release(key, conn) }) }, true, nil
	}
	if !wait {
		return nil, false, nil
	}
	return s.waitFor(ctx, key)
}

// try takes key on the shared session unless this process already holds it
// there, connecting first when there is no session. It returns the connection
// that took the lock. Its queries ignore the caller's context: pgx closes a
// connection whose query is cancelled, which would end every other holder's
// lock.
func (s *Locks) try(key string) (*pgx.Conn, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), queryTimeout)
	defer cancel()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.held[key] {
		return nil, false, nil
	}
	if s.conn == nil {
		connectCtx, cancel := context.WithTimeout(ctx, connectTimeout)
		conn, err := s.dial(connectCtx)
		cancel()
		if err != nil {
			return nil, false, connectError(err)
		}
		s.conn = conn
		go s.keepAlive(conn)
	}
	var ok bool
	if err := s.conn.QueryRow(ctx, "SELECT pg_try_advisory_lock(hashtextextended($1, 0))", key).Scan(&ok); err != nil {
		s.forgetClosed()
		return nil, false, err
	}
	if ok {
		s.held[key] = true
	}
	return s.conn, ok, nil
}

func (s *Locks) release(key string, conn *pgx.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// A lock taken on a session that has since closed already ended with it.
	if s.conn != conn {
		return
	}
	delete(s.held, key)
	ctx, cancel := context.WithTimeout(context.Background(), queryTimeout)
	defer cancel()
	// Closing ends every lock on the session; that is the only way to be sure
	// this one is not left held.
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_unlock(hashtextextended($1, 0))", key); err != nil {
		s.closeLocked()
	}
}

func (s *Locks) keepAlive(conn *pgx.Conn) {
	t := time.NewTicker(pingEvery)
	defer t.Stop()
	for range t.C {
		s.mu.Lock()
		if s.conn != conn {
			s.mu.Unlock()
			return
		}
		if len(s.held) == 0 {
			s.closeLocked()
			s.mu.Unlock()
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), queryTimeout)
		_, err := conn.Exec(ctx, "SELECT 1")
		cancel()
		if err != nil && s.forgetClosed() {
			s.mu.Unlock()
			return
		}
		s.mu.Unlock()
	}
}

// forgetClosed drops the session's connection if an error closed it, with
// every lock it held. A failed query on a live connection leaves its locks
// held, and closing it would end every one of them.
func (s *Locks) forgetClosed() bool {
	if !s.conn.IsClosed() {
		return false
	}
	s.conn = nil
	clear(s.held)
	return true
}

func (s *Locks) closeLocked() {
	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	defer cancel()
	_ = s.conn.Close(ctx)
	s.conn = nil
	clear(s.held)
}

// waitFor waits for key on a connection of its own, which then holds it;
// release closes the connection, which ends the lock.
func (s *Locks) waitFor(ctx context.Context, key string) (release func(), ok bool, err error) {
	// Reuse the 15-second connect budget for connect plus queueing, well below
	// media's one-minute settlement and 30-minute publication deadlines.
	waitCtx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	conn, err := s.dial(waitCtx)
	if err != nil {
		if errors.Is(waitCtx.Err(), context.DeadlineExceeded) {
			err = errors.Join(ErrContended, err)
		}
		return nil, false, connectError(err)
	}
	closeConn := func() {
		ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
		defer cancel()
		_ = conn.Close(ctx)
	}
	if _, err := conn.Exec(waitCtx, "SELECT pg_advisory_lock(hashtextextended($1, 0))", key); err != nil {
		closeConn()
		if errors.Is(waitCtx.Err(), context.DeadlineExceeded) {
			err = errors.Join(ErrContended, err)
		}
		return nil, false, err
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(pingEvery)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				ctx, cancel := context.WithTimeout(context.Background(), queryTimeout)
				_, _ = conn.Exec(ctx, "SELECT 1")
				cancel()
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

// Resource refusals leave acquisition to the caller's existing retry path.
func connectError(err error) error {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		switch pe.Code {
		case "53300", "53000", "53100", "53200", "53400":
			return errors.Join(ErrContended, err)
		}
	}
	return err
}
