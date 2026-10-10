package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// The server lives only as long as its test run. Every reason to stop
// cancels one context (never a deadline: River's notifier spins on a
// context that ended by deadline), and exit always runs the teardown.
var (
	errParentGone = errors.New("parent process gone")
	errStdinEOF   = errors.New("stdin closed")
	errIdle       = errors.New("idle timeout")
	errLifetime   = errors.New("lifetime reached")
	errPostgres   = errors.New("postgres unreachable")
)

type lifecycle struct {
	ctx    context.Context
	cancel context.CancelCauseFunc
	last   atomic.Int64 // unix nanos of the last request
}

func newLifecycle(parent context.Context) *lifecycle {
	ctx, cancel := context.WithCancelCause(parent)
	l := &lifecycle{ctx: ctx, cancel: cancel}
	l.last.Store(time.Now().UnixNano())
	return l
}

// watch stops on stdin's end (when watchStdin), a new parent, -lifetime, and
// -idle without requests.
func (l *lifecycle) watch(watchStdin bool, lifetime, idle time.Duration) {
	ppid := os.Getppid()
	if watchStdin {
		go func() {
			_, _ = io.Copy(io.Discard, os.Stdin)
			l.cancel(errStdinEOF)
		}()
	}
	timer := time.AfterFunc(lifetime, func() { l.cancel(errLifetime) })
	go func() {
		defer timer.Stop()
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-l.ctx.Done():
				return
			case <-t.C:
			}
			if os.Getppid() != ppid {
				l.cancel(errParentGone)
			}
			if idle > 0 && time.Since(time.Unix(0, l.last.Load())) > idle {
				l.cancel(errIdle)
			}
		}
	}()
}

// watchPostgres stops after Postgres has failed every ping for down.
func (l *lifecycle) watchPostgres(pool *pgxpool.Pool, down time.Duration) {
	go func() {
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		failing := time.Time{}
		for {
			select {
			case <-l.ctx.Done():
				return
			case <-t.C:
			}
			ctx, cancel := context.WithTimeout(l.ctx, 2*time.Second)
			err := pool.Ping(ctx)
			cancel()
			switch {
			case err == nil:
				failing = time.Time{}
			case l.ctx.Err() != nil:
				return
			case failing.IsZero():
				failing = time.Now()
			case time.Since(failing) > down:
				l.cancel(fmt.Errorf("%w: %v", errPostgres, err))
				return
			}
		}
	}()
}

// active counts a request against the idle timeout.
func (l *lifecycle) active(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		l.last.Store(time.Now().UnixNano())
		next.ServeHTTP(w, r)
	})
}

// teardown runs argv once (the compose stack's `down`), bounded in time.
func teardown(argv []string) {
	if len(argv) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdout, cmd.Stderr = io.Discard, os.Stderr
	if err := cmd.Run(); err != nil {
		log.Printf("teardown: %v", err)
	}
}

// cappedWriter passes at most limit bytes, then one notice, then nothing: a
// logging loop cannot fill a disk or a CI log.
type cappedWriter struct {
	mu    sync.Mutex
	w     io.Writer
	limit int64
	n     int64
}

func (c *cappedWriter) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.n >= c.limit {
		return len(p), nil
	}
	c.n += int64(len(p))
	if c.n >= c.limit {
		_, _ = fmt.Fprintf(c.w, "e2e server: log output capped at %d bytes; dropping the rest\n", c.limit)
		return len(p), nil
	}
	return c.w.Write(p)
}
