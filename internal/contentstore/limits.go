package contentstore

import (
	"context"
	"expvar"
	"fmt"
	"log/slog"
	"time"

	"github.com/open-rails/contentkit/access"
	content "github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/internal/ratelimit"
	"github.com/redis/go-redis/v9"
)

// Limits are per-actor interaction limits: an actor is its user id, or its
// IP when anonymous. Every attempt spends one, so undoing (unfavorite,
// neutral) costs the same as doing. A nil field takes its default; an action
// with several rates must pass all of them.
type Limits struct {
	Comment         []content.Rate // default 5 per 5 minutes and 20 per hour
	CommentReaction []content.Rate // default 30 per minute
	Reaction        []content.Rate // default 30 per minute
	Favorite        []content.Rate // default 20 per minute
	PollVote        []content.Rate // default 30 per minute

	// Redis (or Garnet) shares the limits across replicas; pass the host's
	// client. Without it each process counts alone, so N replicas allow N
	// times the limit. Redis errors fall back to the per-process count.
	Redis     redis.UniversalClient
	KeyPrefix string // default "contentkit:content:rl:"; the tenant follows it
	// Disabled turns every limit off.
	Disabled bool
}

var defaultLimits = map[content.Action][]content.Rate{
	content.ActionComment:         {{Count: 5, Per: 5 * time.Minute}, {Count: 20, Per: time.Hour}},
	content.ActionCommentReaction: {{Count: 30, Per: time.Minute}},
	content.ActionReaction:        {{Count: 30, Per: time.Minute}},
	content.ActionFavorite:        {{Count: 20, Per: time.Minute}},
	content.ActionPollVote:        {{Count: 30, Per: time.Minute}},
}

// RateLimitRedisErrors counts shared-limit Redis failures, each served by the
// per-process count; published as expvar "contentkit_content_ratelimit_redis_errors".
var RateLimitRedisErrors = expvar.NewInt("contentkit_content_ratelimit_redis_errors")

// newLimiters builds one limiter per action; nil when disabled.
func newLimiters(l Limits, tenant string, log *slog.Logger) (map[content.Action]*ratelimit.Limiter, error) {
	if l.Disabled {
		return nil, nil
	}
	set := map[content.Action][]content.Rate{
		content.ActionComment: l.Comment, content.ActionCommentReaction: l.CommentReaction,
		content.ActionReaction: l.Reaction, content.ActionFavorite: l.Favorite, content.ActionPollVote: l.PollVote,
	}
	prefix := l.KeyPrefix
	if prefix == "" {
		prefix = "contentkit:content:rl:"
	}
	if l.Redis == nil {
		log.Info("content interaction limits are per process (no Limits.Redis): assuming a single replica")
	}
	out := make(map[content.Action]*ratelimit.Limiter, len(defaultLimits))
	for a, def := range defaultLimits {
		rates := set[a]
		if rates == nil {
			rates = def
		}
		ws := make([]ratelimit.Window, len(rates))
		for i, r := range rates {
			ws[i] = ratelimit.Window{Limit: r.Count, Per: r.Per}
		}
		lim, err := ratelimit.New(ratelimit.Options{Windows: ws, Redis: l.Redis, Prefix: prefix + tenant + ":" + string(a) + ":",
			Errors: RateLimitRedisErrors, Logger: log})
		if err != nil {
			return nil, fmt.Errorf("content: Limits %s: %w", a, err)
		}
		out[a] = lim
	}
	return out, nil
}

// limit spends one of actor's a allowance: per subject, and per invoker
// within it when someone acts on its behalf. An actor with neither a subject
// nor an IP has no key and is not limited here.
func (rt *Runtime) limit(ctx context.Context, a content.Action, actor access.Actor) error {
	l := rt.limiters[a]
	key := ""
	switch {
	case actor.ID != "" && !actor.Anonymous && actor.Invoker.ID != "":
		key = "u:" + actor.ID + "|" + actor.Invoker.Issuer + "|" + actor.Invoker.ID
	case actor.ID != "" && !actor.Anonymous:
		key = "u:" + actor.ID
	case actor.IP != "":
		key = "ip:" + actor.IP
	}
	if l == nil || key == "" {
		return nil
	}
	if ok, wait := l.Allow(ctx, key); !ok {
		return &content.RateLimitError{Action: a, RetryAfter: wait}
	}
	return nil
}
