// Package ratelimit admits events per key against one or more sliding
// windows ("at most Limit in any Per"), in process or shared through Redis
// (or Garnet). Both stores keep the same log of admitted event times, so a
// limit and its retry time mean the same thing with and without Redis.
package ratelimit

import (
	"context"
	"errors"
	"expvar"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

// Window admits at most Limit events in any Per.
type Window struct {
	Limit int
	Per   time.Duration
}

// Options configure a Limiter.
type Options struct {
	Windows []Window // an event must fit every one
	// Redis shares the log across replicas; nil keeps it per process.
	Redis  redis.UniversalClient
	Prefix string // Redis key prefix
	// Errors counts Redis failures; each is served by the per-process log.
	Errors *expvar.Int
	Logger *slog.Logger
	Now    func() time.Time // default time.Now
}

const (
	shards       = 32
	maxKeys      = 1 << 16 // per shard
	redisTimeout = 100 * time.Millisecond
	redisBackoff = time.Second // after an error, skip Redis this long
	expireLeeway = time.Second
)

// Limiter admits events from per-key allowances.
type Limiter struct {
	windows []window
	rdb     redis.UniversalClient
	prefix  string
	errs    *expvar.Int
	log     *slog.Logger
	now     func() time.Time
	local   *memory
	down    atomic.Int64 // unix ms until which Redis is skipped
}

type window struct {
	limit int
	per   int64 // ms
}

// New validates o and builds its Limiter.
func New(o Options) (*Limiter, error) {
	if len(o.Windows) == 0 {
		return nil, errors.New("ratelimit: no windows")
	}
	l := &Limiter{rdb: o.Redis, prefix: o.Prefix, errs: o.Errors, log: o.Logger, now: o.Now}
	for _, w := range o.Windows {
		if w.Limit <= 0 || w.Per < time.Millisecond {
			return nil, fmt.Errorf("ratelimit: %d per %s: need a positive limit and a period of at least 1ms", w.Limit, w.Per)
		}
		l.windows = append(l.windows, window{limit: w.Limit, per: w.Per.Milliseconds()})
	}
	if l.now == nil {
		l.now = time.Now
	}
	if l.log == nil {
		l.log = slog.Default()
	}
	l.local = newMemory(l.windows)
	return l, nil
}

// Allow admits one event for key, or refuses it (not counting it) with how
// long until one fits. A Redis error falls back to the per-process log for
// redisBackoff.
func (l *Limiter) Allow(ctx context.Context, key string) (bool, time.Duration) {
	t := l.now().UnixMilli()
	if l.rdb == nil || t < l.down.Load() {
		return l.local.allow(key, t)
	}
	ok, wait, err := l.redisAllow(ctx, key, t)
	if err != nil {
		if l.errs != nil {
			l.errs.Add(1)
		}
		if l.down.Swap(t+redisBackoff.Milliseconds()) <= t-redisBackoff.Milliseconds() {
			l.log.Warn("rate limit: Redis unavailable, counting per process", "prefix", l.prefix, "err", err.Error())
		}
		return l.local.allow(key, t)
	}
	return ok, wait
}

// redisAllow logs the event in one sorted set per window (score: its time in
// ms) in one MULTI, dropping times a window no longer covers, and reads the
// count and the entry Limit places from the newest; a refusal takes the event
// back out. Keys are {prefix}{key}:{per} (one hash slot per key, so MULTI is
// cluster-safe) and expire a window after their last event. Times come from
// each replica's clock, so replicas need NTP. No Lua: Garnet ships with
// scripting off.
func (l *Limiter) redisAllow(ctx context.Context, key string, t int64) (bool, time.Duration, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), redisTimeout)
	defer cancel()
	member := strconv.FormatInt(t, 36) + ":" + strconv.FormatUint(rand.Uint64(), 36)
	keys := make([]string, len(l.windows))
	cards := make([]*redis.IntCmd, len(l.windows))
	edges := make([]*redis.ZSliceCmd, len(l.windows))
	_, err := l.rdb.TxPipelined(ctx, func(p redis.Pipeliner) error {
		for i, w := range l.windows {
			keys[i] = l.prefix + "{" + key + "}:" + strconv.FormatInt(w.per, 10)
			p.ZRemRangeByScore(ctx, keys[i], "-inf", strconv.FormatInt(t-w.per, 10))
			p.ZAdd(ctx, keys[i], redis.Z{Score: float64(t), Member: member})
			cards[i] = p.ZCard(ctx, keys[i])
			// With this event newest, the entry Limit+1 from the end is the
			// one whose expiry makes room.
			edges[i] = p.ZRangeWithScores(ctx, keys[i], int64(-w.limit-1), int64(-w.limit-1))
			p.PExpire(ctx, keys[i], time.Duration(w.per)*time.Millisecond+expireLeeway)
		}
		return nil
	})
	if err != nil {
		return false, 0, err
	}
	var wait int64
	for i, w := range l.windows {
		n, err := cards[i].Result()
		if err != nil {
			return false, 0, err
		}
		if n <= int64(w.limit) {
			continue
		}
		edge, err := edges[i].Result()
		if err != nil {
			return false, 0, err
		}
		if len(edge) == 1 {
			wait = max(wait, int64(edge[0].Score)+w.per-t)
		}
		wait = max(wait, 1)
	}
	if wait == 0 {
		return true, 0, nil
	}
	if _, err := l.rdb.Pipelined(ctx, func(p redis.Pipeliner) error {
		for _, k := range keys {
			p.ZRem(ctx, k, member)
		}
		return nil
	}); err != nil {
		return false, 0, err
	}
	return false, time.Duration(wait) * time.Millisecond, nil
}

// memory is the per-process store: per key and window, the admitted event
// times still inside the window, oldest first.
type memory struct {
	windows []window
	shards  [shards]shard
}

type shard struct {
	mu   sync.Mutex
	keys map[string][][]int64
}

func newMemory(ws []window) *memory {
	m := &memory{windows: ws}
	for i := range m.shards {
		m.shards[i].keys = map[string][][]int64{}
	}
	return m
}

func (m *memory) allow(key string, t int64) (bool, time.Duration) {
	s := &m.shards[fnv32(key)%shards]
	s.mu.Lock()
	defer s.mu.Unlock()
	logs, ok := s.keys[key]
	if !ok {
		if len(s.keys) >= maxKeys {
			m.prune(s, t)
		}
		logs = make([][]int64, len(m.windows))
		s.keys[key] = logs
	}
	var wait int64
	for i, w := range m.windows {
		logs[i] = expire(logs[i], t-w.per)
		if n := len(logs[i]); n >= w.limit {
			wait = max(wait, logs[i][n-w.limit]+w.per-t, 1)
		}
	}
	if wait > 0 {
		return false, time.Duration(wait) * time.Millisecond
	}
	for i := range logs {
		logs[i] = append(logs[i], t)
	}
	return true, 0
}

// expire drops the times at or before cutoff.
func expire(log []int64, cutoff int64) []int64 {
	n := 0
	for n < len(log) && log[n] <= cutoff {
		n++
	}
	if n == 0 {
		return log
	}
	return append(log[:0], log[n:]...)
}

// prune drops keys with no time left in any window (forgetting them changes
// nothing); when every key is live it sheds an arbitrary half.
func (m *memory) prune(s *shard, t int64) {
	for k, logs := range s.keys {
		live := false
		for i, w := range m.windows {
			live = live || len(logs[i]) > 0 && logs[i][len(logs[i])-1] > t-w.per
		}
		if !live {
			delete(s.keys, k)
		}
	}
	if len(s.keys) >= maxKeys {
		n := 0
		for k := range s.keys {
			if n++; n > maxKeys/2 {
				break
			}
			delete(s.keys, k)
		}
	}
}

func fnv32(s string) uint32 {
	h := uint32(2166136261)
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= 16777619
	}
	return h
}
