package content

import (
	"context"
	"errors"
	"expvar"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/internal/ratelimit"
)

// Action names one rate-limited interaction.
type Action string

const (
	ActionComment         Action = "comment"          // comment create, top-level or reply
	ActionCommentReaction Action = "comment_reaction" // like, dislike, neutral on a comment
	ActionPostReaction    Action = "post_reaction"    // like, dislike, neutral on a post
	ActionReaction        Action = "reaction"         // like, dislike, neutral on host content
	ActionFavorite        Action = "favorite"         // favorite and unfavorite
	ActionPollVote        Action = "poll_vote"        // multiple-choice votes and free-text answers
)

// Rate allows at most Count interactions in any Per (a sliding window).
type Rate struct {
	Count int
	Per   time.Duration
}

// Limits are per-actor interaction limits: an actor is its user id, or its
// IP when anonymous. Every attempt spends one, so undoing (unfavorite,
// neutral) costs the same as doing. A nil field takes its default; an action
// with several rates must pass all of them.
type Limits struct {
	Comment         []Rate // default 5 per 5 minutes and 20 per hour
	CommentReaction []Rate // default 30 per minute
	PostReaction    []Rate // default 30 per minute
	Reaction        []Rate // default 30 per minute
	Favorite        []Rate // default 20 per minute
	PollVote        []Rate // default 30 per minute

	// Redis (or Garnet) shares the limits across replicas; pass the host's
	// client. Without it each process counts alone, so N replicas allow N
	// times the limit. Redis errors fall back to the per-process count.
	Redis     redis.UniversalClient
	KeyPrefix string // default "contentkit:content:rl:"; the tenant follows it
	// Disabled turns every limit off.
	Disabled bool
}

var defaultLimits = map[Action][]Rate{
	ActionComment:         {{5, 5 * time.Minute}, {20, time.Hour}},
	ActionCommentReaction: {{30, time.Minute}},
	ActionPostReaction:    {{30, time.Minute}},
	ActionReaction:        {{30, time.Minute}},
	ActionFavorite:        {{20, time.Minute}},
	ActionPollVote:        {{30, time.Minute}},
}

// RateLimitRedisErrors counts shared-limit Redis failures, each served by the
// per-process count; published as expvar "contentkit_content_ratelimit_redis_errors".
var RateLimitRedisErrors = expvar.NewInt("contentkit_content_ratelimit_redis_errors")

// ErrRateLimited is a refusal a later attempt passes. -> 429 rate_limited
var ErrRateLimited = errors.New("content: rate limited")

// RateLimitError is ErrRateLimited for one action, with how long to wait.
type RateLimitError struct {
	Action     Action
	RetryAfter time.Duration
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("content: %s rate limited; retry in %s", e.Action, e.RetryAfter.Round(time.Second))
}

func (e *RateLimitError) Is(target error) bool { return target == ErrRateLimited }

// newLimiters builds one limiter per action; nil when disabled.
func newLimiters(l Limits, tenant string, log *slog.Logger) (map[Action]*ratelimit.Limiter, error) {
	if l.Disabled {
		return nil, nil
	}
	set := map[Action][]Rate{
		ActionComment: l.Comment, ActionCommentReaction: l.CommentReaction, ActionPostReaction: l.PostReaction,
		ActionReaction: l.Reaction, ActionFavorite: l.Favorite, ActionPollVote: l.PollVote,
	}
	prefix := l.KeyPrefix
	if prefix == "" {
		prefix = "contentkit:content:rl:"
	}
	if l.Redis == nil {
		log.Info("content interaction limits are per process (no Limits.Redis): assuming a single replica")
	}
	out := make(map[Action]*ratelimit.Limiter, len(defaultLimits))
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

// limit spends one of actor's a allowance. An actor with neither a user id
// nor an IP has no key and is not limited here.
func (rt *Runtime) limit(ctx context.Context, a Action, actor access.Actor) error {
	l := rt.limiters[a]
	key := ""
	switch {
	case actor.ID != "" && !actor.Anonymous:
		key = "u:" + actor.ID
	case actor.IP != "":
		key = "ip:" + actor.IP
	}
	if l == nil || key == "" {
		return nil
	}
	if ok, wait := l.Allow(ctx, key); !ok {
		return &RateLimitError{Action: a, RetryAfter: wait}
	}
	return nil
}
