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
