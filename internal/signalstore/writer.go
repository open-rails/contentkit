package signalstore

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// FlushEvery is how long the writer gathers rows before one INSERT.
const FlushEvery = time.Second

// MaxBuffered bounds the rows a process holds while ClickHouse is down or
// slow; past it the oldest are dropped.
const MaxBuffered = 50_000

// flushWait bounds one flush.
const flushWait = 30 * time.Second

// Writer is a process's activity buffer: requests add rows and return, and
// one goroutine writes them every FlushEvery. Analytics is best effort: rows
// that cannot be written wait for the next flush, and the oldest are dropped
// past MaxBuffered.
type Writer struct {
	tenant string
	store  func() *Store // nil while ClickHouse is down
	log    *slog.Logger
	every  time.Duration

	mu      sync.Mutex
	rows    []Row
	dropped int
	flushMu sync.Mutex
	stop    chan struct{}
	done    chan struct{}
}

// NewWriter buffers tenant's rows for the store store returns at each flush.
func NewWriter(tenant string, store func() *Store, log *slog.Logger) *Writer {
	if log == nil {
		log = slog.Default()
	}
	return &Writer{tenant: tenant, store: store, log: log, every: FlushEvery}
}

// Add queues rows for the next flush.
func (w *Writer) Add(rows ...Row) {
	if w == nil || len(rows) == 0 {
		return
	}
	w.mu.Lock()
	w.rows = append(w.rows, rows...)
	if over := len(w.rows) - MaxBuffered; over > 0 {
		w.rows = append(w.rows[:0], w.rows[over:]...)
		w.dropped += over
	}
	w.mu.Unlock()
}

// Start flushes every FlushEvery until Close.
func (w *Writer) Start() {
	if w == nil || w.stop != nil {
		return
	}
	w.stop, w.done = make(chan struct{}), make(chan struct{})
	go func() {
		defer close(w.done)
		t := time.NewTicker(w.every)
		defer t.Stop()
		for {
			select {
			case <-w.stop:
				return
			case <-t.C:
				ctx, cancel := context.WithTimeout(context.Background(), flushWait)
				if err := w.Flush(ctx); err != nil {
					w.log.Warn("contentkit: activity not written yet; it is retried", "err", err)
				}
				cancel()
			}
		}
	}()
}

// Flush writes every buffered row now. Rows it cannot write stay queued.
func (w *Writer) Flush(ctx context.Context) error {
	if w == nil {
		return nil
	}
	w.flushMu.Lock()
	defer w.flushMu.Unlock()
	w.mu.Lock()
	rows, dropped := w.rows, w.dropped
	w.rows, w.dropped = nil, 0
	w.mu.Unlock()
	if dropped > 0 {
		w.log.Warn("contentkit: activity buffer full; oldest rows dropped", "rows", dropped)
	}
	if len(rows) == 0 {
		return nil
	}
	store := w.store()
	written, err := 0, errDown
	if store != nil {
		written, err = store.Insert(ctx, w.tenant, rows)
	}
	if err != nil {
		w.mu.Lock()
		w.rows = append(rows[written:], w.rows...)
		if over := len(w.rows) - MaxBuffered; over > 0 {
			w.rows = w.rows[over:]
			w.dropped += over
		}
		w.mu.Unlock()
	}
	return err
}

// Close stops the loop and writes what is buffered within ctx.
func (w *Writer) Close(ctx context.Context) error {
	if w == nil {
		return nil
	}
	if w.stop != nil {
		close(w.stop)
		<-w.done
		w.stop = nil
	}
	return w.Flush(ctx)
}

type downError struct{}

func (downError) Error() string { return "signal: ClickHouse is down" }

var errDown error = downError{}
