package media

import (
	"context"
	"errors"
	"expvar"
	"log/slog"
	"math"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

// ViewerLimit is the read API's per-viewer limit: every read, playlist,
// download, slot and video-images request counts. Zero fields take the
// defaults (2/s sustained, burst 120: a page of reads and an HLS session each
// fit, bulk link harvesting does not).
//
// With Redis set, every replica shares one limit per viewer; otherwise each
// process keeps its own (a single-replica assumption, logged at Handler), so
// N replicas allow N times the limit. Redis errors fail open to the
// per-process limit (see RedisErrors): the limit is abuse protection, and
// tokens and visibility checks still gate every file.
type ViewerLimit struct {
	PerSecond float64
	Burst     int
	// Disabled turns limiting off, e.g. when the host limits upstream.
	Disabled bool
	// Redis is a Redis or Microsoft Garnet client shared by the replicas;
	// pass the host's own. Only INCR, PEXPIRE, GET, DECR and MULTI/EXEC are
	// used (no Lua: Garnet ships with scripting off).
	Redis redis.UniversalClient
	// KeyPrefix namespaces the Redis keys (default "contentkit:media:rl:").
	KeyPrefix string
}

// RedisErrors counts shared-limit Redis failures (each served by the
// per-process limit instead); published as expvar
// "contentkit_media_ratelimit_redis_errors".
var RedisErrors = expvar.NewInt("contentkit_media_ratelimit_redis_errors")

const (
	defaultViewerRate  = 2
	defaultViewerBurst = 120
	defaultKeyPrefix   = "contentkit:media:rl:"
	limiterShards      = 32
	limiterMaxKeys     = 1 << 16 // per shard; idle full buckets are dropped first
	redisTimeout       = 100 * time.Millisecond
	redisBackoff       = time.Second // after an error, skip Redis this long
)

// viewerLimiter takes one request from key's allowance, or reports how long
// until one is available.
type viewerLimiter interface {
	allow(ctx context.Context, key string) (bool, time.Duration)
}

func newViewerLimiter(l ViewerLimit, now func() time.Time, log *slog.Logger) viewerLimiter {
	if l.Disabled {
		return nil
	}
	if l.PerSecond <= 0 {
		l.PerSecond = defaultViewerRate
	}
	if l.Burst <= 0 {
		l.Burst = defaultViewerBurst
	}
	mem := newMemoryLimiter(l, now)
	if l.Redis == nil {
		log.Info("media read rate limit is per process (no ViewerLimit.Redis): assuming a single replica")
		return mem
	}
	if l.KeyPrefix == "" {
		l.KeyPrefix = defaultKeyPrefix
	}
	window := time.Duration(float64(l.Burst) / l.PerSecond * float64(time.Second))
	return &redisLimiter{rdb: l.Redis, prefix: l.KeyPrefix, limit: int64(l.Burst),
		window: max(window.Milliseconds(), 1), now: now, local: mem, log: log}
}

// redisLimiter is a sliding-window counter shared through Redis/Garnet: at
// most Burst requests in any Burst/PerSecond window, estimated from the
// current and previous fixed windows (prev*(1-elapsed/window) + current).
// Keys are {prefix}{viewer}:{window index} and expire after two windows.
// Window indexes come from each replica's clock, so replicas need NTP.
type redisLimiter struct {
	rdb    redis.UniversalClient
	prefix string
	limit  int64
	window int64 // ms
	now    func() time.Time
	local  *memoryLimiter
	log    *slog.Logger
	down   atomic.Int64 // unix ms until which Redis is skipped
}

func (r *redisLimiter) allow(ctx context.Context, key string) (bool, time.Duration) {
	t := r.now().UnixMilli()
	if t < r.down.Load() {
		return r.local.allow(ctx, key)
	}
	ok, wait, err := r.take(ctx, key, t)
	if err != nil {
		RedisErrors.Add(1)
		if r.down.Swap(t+redisBackoff.Milliseconds()) <= t-redisBackoff.Milliseconds() {
			r.log.Warn("media read rate limit: Redis unavailable, failing open to the per-process limit", "err", err.Error())
		}
		return r.local.allow(ctx, key)
	}
	return ok, wait
}

func (r *redisLimiter) take(ctx context.Context, key string, t int64) (bool, time.Duration, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), redisTimeout)
	defer cancel()
	idx, elapsed := t/r.window, t%r.window
	base := r.prefix + "{" + key + "}:" // one hash slot per viewer (cluster-safe MULTI)
	cur, prev := base+strconv.FormatInt(idx, 10), base+strconv.FormatInt(idx-1, 10)
	var incr *redis.IntCmd
	var last *redis.StringCmd
	_, err := r.rdb.TxPipelined(ctx, func(p redis.Pipeliner) error {
		incr = p.Incr(ctx, cur)
		p.PExpire(ctx, cur, time.Duration(2*r.window+1000)*time.Millisecond)
		last = p.Get(ctx, prev)
		return nil
	})
	if err != nil && !errors.Is(err, redis.Nil) {
		return false, 0, err
	}
	n, err := incr.Result()
	if err != nil {
		return false, 0, err
	}
	p, err := last.Int64()
	if err != nil && !errors.Is(err, redis.Nil) {
		return false, 0, err
	}
	weight := float64(r.window-elapsed) / float64(r.window)
	if float64(p)*weight+float64(n) <= float64(r.limit) {
		return true, 0, nil
	}
	// Denied requests do not count against the viewer.
	if err := r.rdb.Decr(ctx, cur).Err(); err != nil {
		return false, 0, err
	}
	n--
	wait := r.window - elapsed // the next window, where this one becomes prev
	if room := r.limit - n - 1; room >= 0 && p > 0 {
		// The earliest elapsed' at which p*(window-elapsed')/window + n + 1 <= limit.
		at := r.window - int64(math.Floor(float64(room)*float64(r.window)/float64(p)))
		wait = max(at-elapsed, 1)
	}
	return false, time.Duration(wait) * time.Millisecond, nil
}

// memoryLimiter is an in-process token bucket per viewer key.
type memoryLimiter struct {
	rate   float64
	burst  float64
	now    func() time.Time
	shards [limiterShards]limiterShard
}

type limiterShard struct {
	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	at     time.Time
}

func newMemoryLimiter(l ViewerLimit, now func() time.Time) *memoryLimiter {
	v := &memoryLimiter{rate: l.PerSecond, burst: float64(l.Burst), now: now}
	for i := range v.shards {
		v.shards[i].buckets = map[string]*bucket{}
	}
	return v
}

func (v *memoryLimiter) allow(_ context.Context, key string) (bool, time.Duration) {
	s := &v.shards[fnv32(key)%limiterShards]
	now := v.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.buckets[key]
	if !ok {
		if len(s.buckets) >= limiterMaxKeys {
			v.prune(s, now)
		}
		b = &bucket{tokens: v.burst, at: now}
		s.buckets[key] = b
	}
	b.tokens = math.Min(v.burst, b.tokens+now.Sub(b.at).Seconds()*v.rate)
	b.at = now
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	return false, time.Duration(math.Ceil((1-b.tokens)/v.rate*1000)) * time.Millisecond
}

// prune drops buckets that have refilled: forgetting them changes nothing.
func (v *memoryLimiter) prune(s *limiterShard, now time.Time) {
	for k, b := range s.buckets {
		if b.tokens+now.Sub(b.at).Seconds()*v.rate >= v.burst {
			delete(s.buckets, k)
		}
	}
	if len(s.buckets) >= limiterMaxKeys { // all active: shed an arbitrary half
		n := 0
		for k := range s.buckets {
			if n++; n > limiterMaxKeys/2 {
				break
			}
			delete(s.buckets, k)
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
