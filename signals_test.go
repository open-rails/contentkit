package contentkit

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/open-rails/contentkit/internal/hub"
	"github.com/open-rails/contentkit/signal"
)

// chServer answers while reachable.
type chServer struct{ reachable *atomic.Bool }

func (c chServer) Exec(context.Context, string, ...any) error {
	if !c.reachable.Load() {
		return io.EOF
	}
	return nil
}

func (c chServer) Query(context.Context, string, ...any) (driver.Rows, error) {
	return nil, errors.ErrUnsupported
}

// A plane down at boot is retried until it answers, taken down when it stops
// answering and brought back: signals and the probe follow every step.
func TestSignalPlaneRecovers(t *testing.T) {
	var reachable atomic.Bool
	var on atomic.Bool
	var connects atomic.Int64
	ch := chServer{&reachable}
	p := newSignalPlane(slog.New(slog.DiscardHandler), func(ctx context.Context) (signal.Conn, error) {
		connects.Add(1)
		return ch, ch.Exec(ctx, "SELECT 1")
	}, func(signal.Conn) error {
		on.Store(true)
		return nil
	}, func() { on.Store(false) })
	p.checkEvery, p.retryMin, p.retryMax = 20*time.Millisecond, 10*time.Millisecond, 40*time.Millisecond
	p.start(t.Context())
	t.Cleanup(p.close)
	await := func(want bool) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); on.Load() != want || (p.check(t.Context()) == nil) != want; time.Sleep(5 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("signals never turned %v (probe: %v)", want, p.check(t.Context()))
			}
		}
	}

	await(false)
	for n, deadline := connects.Load(), time.Now().Add(5*time.Second); connects.Load() < n+3; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("no retries")
		}
	}
	reachable.Store(true)
	await(true)
	reachable.Store(false)
	await(false)
	reachable.Store(true)
	await(true)

	p.close()
	n := connects.Load()
	reachable.Store(false)
	time.Sleep(100 * time.Millisecond)
	if connects.Load() != n || !on.Load() {
		t.Fatal("the plane changed after close")
	}
}

// Purged content's signal erasure fails, so River retries, while a configured
// plane is down; with no plane configured there is nothing to erase.
func TestForgetContentsRetriesWhilePlaneDown(t *testing.T) {
	// pgxpool connects lazily; the hub never touches Postgres here.
	pool, err := pgxpool.New(t.Context(), "postgres://unused:unused@127.0.0.1:9/unused")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	job := &river.Job[forgetContentsArgs]{Args: forgetContentsArgs{Contents: []signal.ForgottenContent{{Kind: "gallery", ContentID: "c1"}}}}
	for _, c := range []struct {
		configured bool
		want       error
	}{{true, hub.ErrSignalPlaneDown}, {false, nil}} {
		h, err := hub.New(hub.Config{PG: pool, PGSchema: "contentkit", Tenant: "doujins", SignalPlane: c.configured})
		if err != nil {
			t.Fatal(err)
		}
		if err := (&forgetContentsWorker{e: &Engine{hub: h}}).Work(t.Context(), job); !errors.Is(err, c.want) {
			t.Errorf("plane configured=%v: %v, want %v", c.configured, err, c.want)
		}
	}
}
