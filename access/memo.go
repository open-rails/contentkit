package access

import (
	"context"
	"sync"
)

type memoCtxKey struct{}

// requestMemo holds, per gate and viewer, the Filter and the exact keys one
// request has read.
type requestMemo struct {
	mu      sync.Mutex
	entries map[memoSlot]*memoEntry
}

type memoSlot struct {
	gate    *Gate
	subject string
}

type memoEntry struct {
	mu     sync.Mutex
	filter *Filter
	keys   map[string]bool
}

// WithMemo returns ctx with a request memo: within it a viewer's Filter is
// read once and Decide answers from it (or from keys already read), so
// Filter then Decide is one billing read. ContentKit's content and media
// handlers install it; a host installs it in its own middleware. A ctx
// that already has one is returned unchanged.
func WithMemo(ctx context.Context) context.Context {
	if _, ok := ctx.Value(memoCtxKey{}).(*requestMemo); ok {
		return ctx
	}
	return context.WithValue(ctx, memoCtxKey{}, &requestMemo{entries: map[memoSlot]*memoEntry{}})
}

// memoFor returns the memo entry of (g, subject), nil without a memo.
func memoFor(ctx context.Context, g *Gate, subject string) *memoEntry {
	m, ok := ctx.Value(memoCtxKey{}).(*requestMemo)
	if !ok {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	slot := memoSlot{gate: g, subject: subject}
	e := m.entries[slot]
	if e == nil {
		e = &memoEntry{keys: map[string]bool{}}
		m.entries[slot] = e
	}
	return e
}

func (e *memoEntry) loadFilter() *Filter {
	if e == nil {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.filter
}

func (e *memoEntry) storeFilter(f *Filter) {
	if e == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.filter == nil {
		e.filter = f
	}
}

func (e *memoEntry) key(k string) (held, ok bool) {
	if e == nil {
		return false, false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	held, ok = e.keys[k]
	return held, ok
}

func (e *memoEntry) storeKeys(answers map[string]bool) {
	if e == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for k, v := range answers {
		e.keys[k] = v
	}
}
