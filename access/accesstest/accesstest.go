// Package accesstest provides an in-memory access.Entitlements and a counting
// wrapper, so host tests can assert "one billing read per page".
package accesstest

import (
	"context"
	"slices"
	"strings"
	"sync"

	"github.com/open-rails/contentkit/access"
)

// Entitlements holds keys per subject in memory. The zero value holds
// nothing; it is safe for concurrent use.
type Entitlements struct {
	mu   sync.Mutex
	held map[string]map[string]bool
	err  error
}

// Grant gives subject keys (key strings, e.g. access.Own(ref).String()).
func (e *Entitlements) Grant(subject string, keys ...string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.held == nil {
		e.held = map[string]map[string]bool{}
	}
	if e.held[subject] == nil {
		e.held[subject] = map[string]bool{}
	}
	for _, k := range keys {
		e.held[subject][k] = true
	}
}

// Revoke takes keys back from subject.
func (e *Entitlements) Revoke(subject string, keys ...string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, k := range keys {
		delete(e.held[subject], k)
	}
}

// Fail makes every Held call fail with err (nil restores answers).
func (e *Entitlements) Fail(err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.err = err
}

// Held answers q the way OpenRails does: every exact key, and per prefix the
// held keys in byte order, at most q.Limit (default access.DefaultHeldLimit).
func (e *Entitlements) Held(_ context.Context, subject string, q access.Query) (access.Answer, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.err != nil {
		return access.Answer{}, e.err
	}
	held := e.held[subject]
	ans := access.Answer{Keys: make(map[string]bool, len(q.Keys)), Held: make(map[string]access.HeldKeys, len(q.Prefixes))}
	for _, k := range q.Keys {
		ans.Keys[k] = held[k]
	}
	limit := q.Limit
	if limit <= 0 {
		limit = access.DefaultHeldLimit
	}
	for _, p := range q.Prefixes {
		var keys []string
		for k := range held {
			if strings.HasPrefix(k, p) {
				keys = append(keys, k)
			}
		}
		slices.Sort(keys)
		h := access.HeldKeys{Keys: keys}
		if len(keys) > limit {
			h = access.HeldKeys{Keys: keys[:limit], Truncated: true}
		}
		ans.Held[p] = h
	}
	return ans, nil
}

// CountingEntitlements wraps Next and records every Held call.
type CountingEntitlements struct {
	Next access.Entitlements

	mu      sync.Mutex
	queries []access.Query
}

// Held records q and delegates to Next.
func (c *CountingEntitlements) Held(ctx context.Context, subject string, q access.Query) (access.Answer, error) {
	c.mu.Lock()
	c.queries = append(c.queries, q)
	c.mu.Unlock()
	return c.Next.Held(ctx, subject, q)
}

// Calls returns the number of Held calls so far.
func (c *CountingEntitlements) Calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.queries)
}

// Queries returns the recorded queries in call order.
func (c *CountingEntitlements) Queries() []access.Query {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.queries)
}

// Reset forgets the recorded calls.
func (c *CountingEntitlements) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.queries = nil
}
