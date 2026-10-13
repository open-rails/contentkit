package contentkit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/open-rails/migratekit"
	"github.com/open-rails/migratekit/chmigrate"
	"github.com/riverqueue/river"

	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/migrations"
	"github.com/open-rails/contentkit/signal"
)

// How the engine keeps a configured signal plane: New tries once, then a
// background loop retries with backoff while it is down and checks it while
// it is up.
const (
	signalBootWait   = 10 * time.Second
	signalCheckEvery = 10 * time.Second
	signalCheckWait  = 5 * time.Second
	signalRetryMin   = time.Second
	signalRetryMax   = 30 * time.Second
)

// signalPlane keeps a configured ClickHouse signal plane: signals are on
// while it answers and off (the content model and the contentkit_clickhouse
// probe say so) while it doesn't. Only start and its loop change the state.
type signalPlane struct {
	connect func(context.Context) (signal.Conn, error) // migrate, open, check the schema
	up      func(signal.Conn) error                    // signals on
	down    func()                                     // signals off
	log     *slog.Logger

	bootWait, checkEvery, checkWait, retryMin, retryMax time.Duration

	mu   sync.Mutex
	conn signal.Conn // nil while down
	err  error       // why it is down
	stop context.CancelFunc
	done chan struct{}
}

func newSignalPlane(log *slog.Logger, connect func(context.Context) (signal.Conn, error), up func(signal.Conn) error, down func()) *signalPlane {
	return &signalPlane{connect: connect, up: up, down: down, log: log, err: errors.New("contentkit: the signal plane is not open yet"),
		bootWait: signalBootWait, checkEvery: signalCheckEvery, checkWait: signalCheckWait, retryMin: signalRetryMin, retryMax: signalRetryMax}
}

// start tries once within ctx and bootWait, then keeps the plane in the
// background until close.
func (p *signalPlane) start(ctx context.Context) {
	first, cancel := context.WithTimeout(ctx, p.bootWait)
	p.attempt(first)
	cancel()
	run, stop := context.WithCancel(context.WithoutCancel(ctx))
	p.stop, p.done = stop, make(chan struct{})
	go p.run(run)
}

func (p *signalPlane) close() {
	if p.stop != nil {
		p.stop()
		<-p.done
	}
}

func (p *signalPlane) run(ctx context.Context) {
	defer close(p.done)
	wait := p.retryMin
	for {
		conn, _ := p.state()
		delay := wait
		if conn != nil {
			delay = p.checkEvery
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		switch {
		case conn != nil:
			if err := p.check(ctx); err != nil && ctx.Err() == nil {
				p.fail(err)
				wait = p.retryMin
			}
		case p.attempt(ctx):
			wait = p.retryMin
		default:
			wait = min(2*wait, p.retryMax)
		}
	}
}

// attempt opens the plane; true once it is up.
func (p *signalPlane) attempt(ctx context.Context) bool {
	conn, err := p.connect(ctx)
	if err == nil {
		err = p.up(conn)
	}
	if err != nil && errors.Is(ctx.Err(), context.Canceled) {
		return false // closing
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err != nil {
		if p.err != nil && p.err.Error() == err.Error() {
			p.log.Debug("contentkit: the signal plane is still down", "err", err)
		} else {
			p.log.Warn("contentkit: the signal plane is down; signals are off while it is retried", "err", err)
		}
		p.err = err
		return false
	}
	if p.conn == nil && p.done != nil {
		p.log.Info("contentkit: the signal plane is back; signals are on")
	}
	p.conn, p.err = conn, nil
	return true
}

func (p *signalPlane) fail(err error) {
	p.mu.Lock()
	p.conn, p.err = nil, err
	p.mu.Unlock()
	p.down()
	p.log.Warn("contentkit: the signal plane stopped answering; signals are off while it is retried", "err", err)
}

func (p *signalPlane) state() (signal.Conn, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.conn, p.err
}

// check is the contentkit_clickhouse probe: nil while the plane answers.
func (p *signalPlane) check(ctx context.Context) error {
	conn, err := p.state()
	if conn == nil {
		return fmt.Errorf("contentkit: signals are off while the signal plane is down: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, p.checkWait)
	defer cancel()
	return conn.Exec(ctx, "SELECT 1")
}

// newSignals keeps the signal plane Deps.ClickHouse or Config.ClickHouse
// configures; nil without one.
func (e *Engine) newSignals(deps Deps) *signalPlane {
	c := e.cfg.ClickHouse
	if deps.ClickHouse == nil && c.Addr == "" {
		return nil
	}
	return newSignalPlane(e.log, func(ctx context.Context) (signal.Conn, error) { return e.connectSignals(ctx, deps.ClickHouse) },
		func(conn signal.Conn) error {
			store, err := signal.NewStore(conn, c.Database)
			if err != nil {
				return err
			}
			e.hub.SetSignals(store)
			e.signalsBack()
			return nil
		},
		func() { e.hub.SetSignals(nil) })
}

// connectSignals applies the signal migrations (with Addr), takes host's
// connection or opens one from Config (once; it reconnects by itself) and
// checks its schema.
func (e *Engine) connectSignals(ctx context.Context, host signal.Conn) (signal.Conn, error) {
	c := e.cfg.ClickHouse
	if c.Addr != "" {
		steps, err := migratekit.Load(migrations.ClickHouse, ".")
		if err != nil {
			return nil, fmt.Errorf("contentkit: load signal migrations: %w", err)
		}
		db := stdlib.OpenDBFromPool(e.pool)
		defer db.Close()
		m := chmigrate.New(&chmigrate.Config{ClientAddr: c.Addr, Database: c.Database, Username: c.Username, Password: c.Password,
			Cluster: c.Cluster, App: "contentkit_signal", PostgresDB: db})
		defer m.Close()
		if err := m.ApplyMigrations(ctx, steps); err != nil {
			return nil, fmt.Errorf("contentkit: apply signal migrations: %w", err)
		}
	}
	conn := host
	if conn == nil {
		e.chMu.Lock()
		if e.ownCH == nil {
			own, err := clickhouse.Open(&clickhouse.Options{Addr: []string{c.Addr},
				Auth: clickhouse.Auth{Database: c.Database, Username: c.Username, Password: c.Password}})
			if err != nil {
				e.chMu.Unlock()
				return nil, fmt.Errorf("contentkit: open ClickHouse: %w", err)
			}
			e.ownCH = own
		}
		conn = e.ownCH
		e.chMu.Unlock()
	}
	if err := signal.CheckSchema(ctx, conn, c.Database); err != nil {
		return nil, err
	}
	return conn, nil
}

// signalsBack queues what the signal plane missed while it was down:
// preferences, projection repair and popularity. Before Start the periodic
// jobs' first runs do it.
func (e *Engine) signalsBack() {
	c := e.jobs.bound()
	if c == nil {
		return
	}
	opts := &river.InsertOpts{Queue: QueueContentKit, MaxAttempts: 3, UniqueOpts: media.PendingOnce}
	ctx, cancel := context.WithTimeout(context.Background(), signalCheckWait)
	defer cancel()
	if _, err := c.InsertMany(ctx, []river.InsertManyParams{{Args: preferenceSyncArgs{}, InsertOpts: opts}, {Args: repairArgs{}, InsertOpts: opts},
		{Args: popularityArgs{}, InsertOpts: opts}}); err != nil {
		e.log.Warn("contentkit: could not queue the signal plane's catch-up; the periodic jobs will", "err", err)
	}
}
