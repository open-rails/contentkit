package critical_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/contentkit/internal/pglock"
	"github.com/open-rails/contentkit/internal/versionstore"
)

type panicLockContext struct{ context.Context }

func (panicLockContext) Deadline() (time.Time, bool) { panic("test: waiter context panicked") }

func TestAdvisoryLockSession(t *testing.T) {
	dsn := os.Getenv("CONTENTKIT_CRITICAL_PG")
	if dsn == "" {
		t.Fatal("CONTENTKIT_CRITICAL_PG is required")
	}
	// Each pool stands in for one process; its application name tells its
	// connections apart in pg_stat_activity.
	process := func(t *testing.T) (*pgxpool.Pool, string) {
		cfg, err := pgxpool.ParseConfig(dsn)
		if err != nil {
			t.Fatal(err)
		}
		name := "ck-lock-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
		cfg.ConnConfig.RuntimeParams["application_name"] = name
		pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(pool.Close)
		return pool, name
	}
	prefix := "contentkit:test:" + uuid.NewString() + ":"

	// Sixteen workers each take an outer key and nest three keys of their own
	// while holding it, like a source lock over manifest locks. held, when
	// set, runs once every worker holds all four keys; each then releases.
	nest := func(t *testing.T, pool *pgxpool.Pool, outer func(w int) string, held func()) (overlap int32) {
		ctx := t.Context()
		var inside sync.Map
		var overlaps atomic.Int32
		var holding sync.WaitGroup
		holding.Add(16)
		release := make(chan struct{})
		var wg sync.WaitGroup
		errs := make(chan error, 16)
		for w := range 16 {
			wg.Go(func() {
				defer func() {
					if held != nil {
						<-release
					}
				}()
				key := outer(w)
				unlock, _, err := pglock.Acquire(ctx, pool, key, true)
				if err != nil {
					errs <- err
					holding.Done()
					return
				}
				defer unlock()
				if _, busy := inside.LoadOrStore(key, w); busy {
					overlaps.Add(1)
				}
				defer inside.Delete(key)
				for i := range 3 {
					inner, _, err := pglock.Acquire(ctx, pool, fmt.Sprintf("%sinner:%d:%d", prefix, w, i), true)
					if err != nil {
						errs <- err
						holding.Done()
						return
					}
					defer inner()
				}
				holding.Done()
				if held != nil {
					<-release
					return
				}
				if _, err := pool.Exec(ctx, "SELECT pg_sleep(0.01)"); err != nil {
					errs <- err
				}
			})
		}
		if held != nil {
			holding.Wait()
			held()
			close(release)
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatal(err)
		}
		return overlaps.Load()
	}
	sessions := func(t *testing.T, observer *pgxpool.Pool, name string) (sessions, locks int32) {
		if err := observer.QueryRow(t.Context(), `SELECT count(DISTINCT l.pid), count(*) FROM pg_locks l JOIN pg_stat_activity a ON a.pid=l.pid
 WHERE l.locktype='advisory' AND l.granted AND a.application_name=$1`, name).Scan(&sessions, &locks); err != nil {
			t.Fatal(err)
		}
		return sessions, locks
	}

	t.Run("CK-J11-S10_uncontended_nested_locks_share_one_connection", func(t *testing.T) {
		pool, name := process(t)
		observer, _ := process(t)
		var held, locks int32
		nest(t, pool, func(w int) string { return fmt.Sprintf("%souter:%d", prefix, w) }, func() {
			held, locks = sessions(t, observer, name)
		})
		if held != 1 || locks != 64 {
			t.Fatalf("64 held locks were on %d sessions (%d locks seen), want all on the one shared session", held, locks)
		}
		if _, left := sessions(t, observer, name); left != 0 {
			t.Fatalf("%d advisory locks left held after every release", left)
		}
	})

	t.Run("CK-J11-S10_a_contended_key_is_held_by_one_worker_at_a_time", func(t *testing.T) {
		pool, name := process(t)
		observer, _ := process(t)
		if overlap := nest(t, pool, func(int) string { return prefix + "outer" }, nil); overlap != 0 {
			t.Fatalf("%d workers held the outer key at once", overlap)
		}
		if _, left := sessions(t, observer, name); left != 0 {
			t.Fatalf("%d advisory locks left held after every release", left)
		}
	})

	t.Run("CK-J11-S10_a_lost_session_reconnects_for_the_next_lock", func(t *testing.T) {
		ctx := t.Context()
		pool, name := process(t)
		observer, _ := process(t)
		holder := func() (pid int32) {
			if err := observer.QueryRow(ctx, `SELECT coalesce(max(l.pid), 0) FROM pg_locks l JOIN pg_stat_activity a ON a.pid=l.pid WHERE l.locktype='advisory' AND a.application_name=$1`, name).Scan(&pid); err != nil {
				t.Fatal(err)
			}
			return pid
		}
		release, _, err := pglock.Acquire(ctx, pool, prefix+"lost", true)
		if err != nil {
			t.Fatal(err)
		}
		lost := holder()
		if _, err := observer.Exec(ctx, "SELECT pg_terminate_backend($1)", lost); err != nil {
			t.Fatal(err)
		}
		next, ok, err := pglock.Acquire(ctx, pool, prefix+"next", false)
		if err == nil && ok {
			next()
		}
		// The first query on the terminated session fails; the one after
		// reconnects.
		next, ok, err = pglock.Acquire(ctx, pool, prefix+"next", false)
		if err != nil || !ok {
			t.Fatalf("no lock after the session was lost: ok=%v err=%v", ok, err)
		}
		if pid := holder(); pid == 0 || pid == lost {
			t.Fatalf("lock held by session %d, want a new one (lost %d)", pid, lost)
		}
		next()
		release()
	})

	t.Run("CK-J11-S10_locks_exclude_other_processes_and_survive_a_cancelled_waiter", func(t *testing.T) {
		ctx := t.Context()
		a, _ := process(t)
		b, _ := process(t)
		held, _, err := pglock.Acquire(ctx, a, prefix+"held", true)
		if err != nil {
			t.Fatal(err)
		}
		other, _, err := pglock.Acquire(ctx, b, prefix+"other", true)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok, err := pglock.Acquire(ctx, b, prefix+"held", false); err != nil || ok {
			t.Fatalf("another process took a held key: ok=%v err=%v", ok, err)
		}
		// A waiter in a that gives up must not end a's other locks.
		waitCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
		defer cancel()
		if _, _, err := pglock.Acquire(waitCtx, a, prefix+"other", true); err == nil {
			t.Fatal("waiter acquired a key held by another process")
		}
		if _, ok, err := pglock.Acquire(ctx, b, prefix+"held", false); err != nil || ok {
			t.Fatalf("a cancelled waiter released its process's other lock: ok=%v err=%v", ok, err)
		}
		acquired := make(chan error, 1)
		go func() {
			release, _, err := pglock.Acquire(ctx, b, prefix+"held", true)
			if err == nil {
				release()
			}
			acquired <- err
		}()
		held()
		select {
		case err := <-acquired:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("a waiter in another process never acquired the released key")
		}
		other()
	})
	t.Run("CK279_wait_budget_preserves_other_shared_locks", func(t *testing.T) {
		ctx := t.Context()
		a, name := process(t)
		b, _ := process(t)
		first, _, err := pglock.Acquire(ctx, a, prefix+"budget-first", true)
		if err != nil {
			t.Fatal(err)
		}
		defer first()
		second, _, err := pglock.Acquire(ctx, a, prefix+"budget-second", true)
		if err != nil {
			t.Fatal(err)
		}
		defer second()
		blocker, _, err := pglock.Acquire(ctx, b, prefix+"budget-blocker", true)
		if err != nil {
			t.Fatal(err)
		}
		defer blocker()
		// The caller still has time left when the acquisition's own budget ends.
		waitCtx, cancel := context.WithTimeout(ctx, 19*time.Second)
		defer cancel()
		started := time.Now()
		_, ok, err := pglock.Acquire(waitCtx, a, prefix+"budget-blocker", true)
		if ok || err == nil || !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, pglock.ErrContended) {
			t.Errorf("wait timeout did not return retryable contention: ok=%v err=%v", ok, err)
		}
		if elapsed := time.Since(started); elapsed > 17*time.Second || waitCtx.Err() != nil {
			t.Errorf("acquisition used the caller's long deadline: elapsed=%s caller=%v", elapsed, waitCtx.Err())
		}
		if held, locks := sessions(t, b, name); held != 1 || locks != 2 {
			t.Errorf("wait timeout disturbed shared session: sessions=%d locks=%d", held, locks)
		}
		var waiters int
		if err := b.QueryRow(ctx, `SELECT count(*) FROM pg_locks l JOIN pg_stat_activity a ON a.pid=l.pid WHERE l.locktype='advisory' AND NOT l.granted AND a.application_name=$1`, name).Scan(&waiters); err != nil {
			t.Fatal(err)
		}
		if waiters != 0 {
			t.Errorf("timed-out dedicated waiter still owns %d queued locks", waiters)
		}
		for _, key := range []string{"budget-first", "budget-second"} {
			if release, ok, err := pglock.Acquire(ctx, b, prefix+key, false); err != nil || ok {
				if ok {
					release()
				}
				t.Errorf("timeout dropped another owner's %s: ok=%v err=%v", key, ok, err)
			}
		}
	})

	t.Run("CK279_source_lock_partial_acquisition_releases_on_panic", func(t *testing.T) {
		ctx := t.Context()
		a, name := process(t)
		b, _ := process(t)
		t.Cleanup(func() {
			// This also cleans up the unchanged implementation's leaked key.
			cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, err := b.Exec(cleanup, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE application_name=$1`, name)
			if err != nil {
				t.Error(err)
			}
		})
		tenant := uuid.NewString()
		key := "contentkit:media:contentkit:versions:" + tenant + ":"
		blocker, _, err := pglock.Acquire(ctx, b, key+"b", true)
		if err != nil {
			t.Fatal(err)
		}
		defer blocker()
		func() {
			defer func() {
				if recover() == nil {
					t.Error("second acquisition did not panic")
				}
			}()
			_, _ = versionstore.LockSources(panicLockContext{ctx}, a, tenant, []string{"b", "a"})
		}()
		release, ok, err := pglock.Acquire(ctx, b, key+"a", false)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			t.Fatal("LockSources retained its first key after the second acquisition panicked")
		}
		release()
	})

}
