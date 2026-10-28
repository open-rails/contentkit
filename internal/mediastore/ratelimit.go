package mediastore

import (
	"context"
	"expvar"
	"log/slog"
	"time"

	"github.com/open-rails/contentkit/internal/ratelimit"
	"github.com/redis/go-redis/v9"
)

// RateLimit is a per-key request limit: the read API's per viewer
// (HandlerOptions.Limit) and commits' per uploader (UploadOptions.Commits).
// Zero fields take the use's defaults.
//
// With Redis set, every replica shares one limit per key; otherwise each
// process keeps its own (a single-replica assumption, logged at start), so
// N replicas allow N times the limit. Redis errors fail open to the
// per-process limit (see RedisErrors): the limit is abuse protection, and
// tokens, permissions and visibility checks still gate everything.
type RateLimit struct {
	PerSecond float64
	Burst     int
	// Disabled turns limiting off, e.g. when the host limits upstream.
	Disabled bool
	// Redis is a Redis or Microsoft Garnet client shared by the replicas;
	// pass the host's own. Only sorted-set commands and MULTI/EXEC are used
	// (no Lua: Garnet ships with scripting off).
	Redis redis.UniversalClient
	// KeyPrefix namespaces the Redis keys (defaults "contentkit:media:rl:"
	// for viewers, "contentkit:media:commit:" for uploaders).
	KeyPrefix string
}

// RedisErrors counts shared-limit Redis failures (each served by the
// per-process limit instead); published as expvar
// "contentkit_media_ratelimit_redis_errors".
var RedisErrors = expvar.NewInt("contentkit_media_ratelimit_redis_errors")

var (
	viewerLimit = RateLimit{PerSecond: 2, Burst: 120, KeyPrefix: "contentkit:media:rl:"}
	commitLimit = RateLimit{PerSecond: 1, Burst: 30, KeyPrefix: "contentkit:media:commit:"}
)

// newRateLimiter is l with def's values for its zero fields: at most Burst
// requests per key in any Burst/PerSecond. name is the use in logs.
func newRateLimiter(l, def RateLimit, name string, now func() time.Time, log *slog.Logger) *ratelimit.Limiter {
	if l.Disabled {
		return nil
	}
	if l.PerSecond <= 0 {
		l.PerSecond = def.PerSecond
	}
	if l.Burst <= 0 {
		l.Burst = def.Burst
	}
	if l.KeyPrefix == "" {
		l.KeyPrefix = def.KeyPrefix
	}
	if l.Redis == nil {
		log.Info("media " + name + " rate limit is per process (no RateLimit.Redis): assuming a single replica")
	}
	per := max(time.Duration(float64(l.Burst)/l.PerSecond*float64(time.Second)), time.Millisecond)
	lim, err := ratelimit.New(ratelimit.Options{Windows: []ratelimit.Window{{Limit: l.Burst, Per: per}},
		Redis: l.Redis, Prefix: l.KeyPrefix, Errors: RedisErrors, Logger: log, Now: now})
	if err != nil {
		panic(err) // unreachable: Burst and the period are positive
	}
	return lim
}

// CommitLimiter is the commit limit (UploadOptions.Commits) for commits a
// host forwards to another site's ContentKit, which exempts the forwarder:
// each person's allowance is taken before forwarding.
type CommitLimiter struct{ l *ratelimit.Limiter }

// NewCommitLimiter is l with the commit defaults for its zero fields.
func NewCommitLimiter(l RateLimit) CommitLimiter {
	return CommitLimiter{newRateLimiter(l, commitLimit, "forwarded commit", time.Now, slog.Default())}
}

// Allow takes one commit from uploader's allowance, or reports how long
// until one is available.
func (c CommitLimiter) Allow(ctx context.Context, uploader string) (bool, time.Duration) {
	if c.l == nil {
		return true, 0
	}
	return c.l.Allow(ctx, uploader)
}
