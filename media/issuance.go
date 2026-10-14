package media

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/open-rails/contentkit/access"
)

// Issuance limits how many items a viewer is given the token of per hour:
// each grant with access opens one item, and a scraper needs one per item.
// Opening the same item again within the hour is free. With the ingress
// limiting each IP's downloads from the media host, this bounds what an
// account can pull: items per hour times what the ingress lets through.
//
// With Redis set every replica shares the count; otherwise each process
// keeps its own (logged at start). Redis errors fail open to the
// per-process count (RedisErrors).
type Issuance struct {
	// PerHour is the items an account may open per hour (default 120);
	// AnonymousPerHour is per Actor.IP (an IPv6 address counts as its /64),
	// where one address may be many people (default 600). The read API
	// fills an anonymous actor's IP from the connection; a host calling
	// Reader.Grant itself must set it (ErrNoViewerKey otherwise: without it
	// every anonymous viewer would share one count).
	PerHour, AnonymousPerHour int
	Disabled                  bool
	// Exempt actors are not limited (staff); an item's editors never are.
	Exempt func(access.Actor) bool
	// Redis is a Redis or Microsoft Garnet client shared by the replicas.
	// Only SADD, SCARD, SREM, PEXPIRE and MULTI/EXEC are used (no Lua).
	Redis redis.UniversalClient
	// KeyPrefix namespaces the Redis keys (default "contentkit:media:issue:").
	KeyPrefix string
}

// ErrRateLimited is a refusal that a later attempt will pass.
var ErrRateLimited = errors.New("media: rate limited")

// ErrNoViewerKey is a grant the issuance limit cannot count: an anonymous
// actor without Actor.IP. It is the host's bug, not the viewer's limit.
var ErrNoViewerKey = errors.New("media: an anonymous actor needs Actor.IP for the issuance limit")

// LimitError is ErrRateLimited with how long to wait.
type LimitError struct{ RetryAfter time.Duration }

func (e *LimitError) Error() string {
	return fmt.Sprintf("media: rate limited; retry in %s", e.RetryAfter.Round(time.Second))
}
func (e *LimitError) Is(target error) bool { return target == ErrRateLimited }

const issuanceWindow = time.Hour

// issuance counts the distinct members (items) of each key (viewer) over a
// sliding hour: prev*(1-elapsed/window) + current, as the request limiter does.
type issuance struct {
	o      Issuance
	now    func() time.Time
	local  *distinctCounter
	log    *slog.Logger
	down   atomic.Int64 // unix ms until which Redis is skipped
	window int64        // ms
}

func newIssuance(o Issuance, now func() time.Time, log *slog.Logger) *issuance {
	if o.Disabled {
		return nil
	}
	if o.PerHour <= 0 {
		o.PerHour = 120
	}
	if o.AnonymousPerHour <= 0 {
		o.AnonymousPerHour = 600
	}
	if o.KeyPrefix == "" {
		o.KeyPrefix = "contentkit:media:issue:"
	}
	if o.Redis == nil {
		log.Info("media issuance limit is per process (no Issuance.Redis): assuming a single replica")
	}
	return &issuance{o: o, now: now, log: log, window: issuanceWindow.Milliseconds(),
		local: &distinctCounter{max: limiterMaxKeys, keys: map[string]*distinctWindow{}}}
}

// allow counts item for actor: nil, a LimitError saying how long until it
// may be opened, or ErrNoViewerKey. An editor of the item (editor) and
// exempt actors are not counted.
func (i *issuance) allow(ctx context.Context, actor access.Actor, editor bool, item string) error {
	if i == nil || editor || i.o.Exempt != nil && i.o.Exempt(actor) {
		return nil
	}
	key, limit := viewerKey(actor), int64(i.o.AnonymousPerHour)
	if key == "" {
		return ErrNoViewerKey
	}
	if !actor.Anonymous && actor.ID != "" {
		limit = int64(i.o.PerHour)
	}
	if ok, wait := i.count(ctx, key, item, limit); !ok {
		return &LimitError{RetryAfter: wait}
	}
	return nil
}

func (i *issuance) count(ctx context.Context, key, item string, limit int64) (bool, time.Duration) {
	t := i.now().UnixMilli()
	idx, elapsed := t/i.window, t%i.window
	if i.o.Redis != nil && t >= i.down.Load() {
		ok, wait, err := i.shared(ctx, key, item, limit, idx, elapsed)
		if err == nil {
			return ok, wait
		}
		RedisErrors.Add(1)
		if i.down.Swap(t+redisBackoff.Milliseconds()) <= t-redisBackoff.Milliseconds() {
			i.log.Warn("media issuance limit: Redis unavailable, failing open to the per-process count", "err", err.Error())
		}
	}
	cur, prev, added := i.local.add(key, item, idx)
	if !added || i.within(cur, prev, limit, elapsed) {
		return true, 0
	}
	i.local.remove(key, item)
	return false, i.wait(cur-1, prev, limit, elapsed)
}

// within reports cur items beside what is left of the previous window's
// within limit: prev*(window-elapsed)/window + cur <= limit.
func (i *issuance) within(cur, prev, limit, elapsed int64) bool {
	return prev*(i.window-elapsed) <= (limit-cur)*i.window
}

// wait is how long until one more item fits beside cur: in this window once
// enough of the previous one has slid out, else in the next, where this
// window is the previous one.
func (i *issuance) wait(cur, prev, limit, elapsed int64) time.Duration {
	if room := limit - cur - 1; room >= 0 && prev > 0 {
		return time.Duration(max(i.window-room*i.window/prev-elapsed, 1)) * time.Millisecond
	}
	wait := i.window - elapsed
	if cur > limit-1 {
		wait += i.window - (limit-1)*i.window/cur
	}
	return time.Duration(wait) * time.Millisecond
}

func (i *issuance) shared(ctx context.Context, key, item string, limit, idx, elapsed int64) (bool, time.Duration, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), redisTimeout)
	defer cancel()
	base := i.o.KeyPrefix + "{" + key + "}:" // one hash slot per viewer (cluster-safe MULTI)
	cur, prev := base+strconv.FormatInt(idx, 10), base+strconv.FormatInt(idx-1, 10)
	var added, card, last *redis.IntCmd
	if _, err := i.o.Redis.TxPipelined(ctx, func(p redis.Pipeliner) error {
		added = p.SAdd(ctx, cur, item)
		card = p.SCard(ctx, cur)
		p.PExpire(ctx, cur, time.Duration(2*i.window+1000)*time.Millisecond)
		last = p.SCard(ctx, prev)
		return nil
	}); err != nil {
		return false, 0, err
	}
	if added.Val() == 0 || i.within(card.Val(), last.Val(), limit, elapsed) {
		return true, 0, nil // opened already this hour, or within the limit
	}
	// A refused item does not count against the viewer.
	if err := i.o.Redis.SRem(ctx, cur, item).Err(); err != nil {
		return false, 0, err
	}
	return false, i.wait(card.Val()-1, last.Val(), limit, elapsed), nil
}

// distinctCounter is the per-process count: each key's members in the
// current window and how many it had in the previous one, for at most max
// keys.
type distinctCounter struct {
	mu   sync.Mutex
	max  int
	keys map[string]*distinctWindow
}

type distinctWindow struct {
	idx  int64
	cur  map[string]struct{}
	prev int64
}

// add puts member in key's window idx and returns the window's size, the
// previous window's, and whether member was new.
func (c *distinctCounter) add(key, member string, idx int64) (cur, prev int64, added bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	w := c.keys[key]
	if w == nil {
		if len(c.keys) >= c.max {
			c.prune(idx)
		}
		w = &distinctWindow{idx: idx, cur: map[string]struct{}{}}
		c.keys[key] = w
	}
	if w.idx != idx {
		w.prev = 0
		if w.idx == idx-1 {
			w.prev = int64(len(w.cur))
		}
		w.idx, w.cur = idx, map[string]struct{}{}
	}
	_, had := w.cur[member]
	w.cur[member] = struct{}{}
	return int64(len(w.cur)), w.prev, !had
}

// prune makes room: the windows that ended go (forgetting them changes
// nothing); if every key is live, an arbitrary half goes, and each of those
// viewers starts a fresh count.
func (c *distinctCounter) prune(idx int64) {
	for k, w := range c.keys {
		if w.idx < idx-1 {
			delete(c.keys, k)
		}
	}
	if len(c.keys) < c.max {
		return
	}
	n := 0
	for k := range c.keys {
		if n++; n > c.max/2 {
			break
		}
		delete(c.keys, k)
	}
}

func (c *distinctCounter) remove(key, member string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if w := c.keys[key]; w != nil {
		delete(w.cur, member)
	}
}
