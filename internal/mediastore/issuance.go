package mediastore

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/internal/ratelimit"
	media "github.com/open-rails/contentkit/media"
	"github.com/redis/go-redis/v9"
)

// Issuance limits how many items a viewer is given the token of per hour:
// each grant with access opens one item, and a scraper needs one per item.
// Opening the same item again within an hour of first opening it is free.
// With the ingress limiting each IP's downloads from the media host, this
// bounds what an account can pull: items per hour times what the ingress lets
// through.
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
	// Only sorted-set commands and MULTI/EXEC are used (no Lua).
	Redis redis.UniversalClient
	// KeyPrefix namespaces the Redis keys (default "contentkit:media:issue:").
	KeyPrefix string
}

// ErrNoViewerKey is a grant the issuance limit cannot count: an anonymous
// actor without Actor.IP. It is the host's bug, not the viewer's limit.
var ErrNoViewerKey = errors.New("media: an anonymous actor needs Actor.IP for the issuance limit")

const issuanceWindow = time.Hour

// issuance counts the distinct items each viewer opens in any hour.
type issuance struct {
	exempt              func(access.Actor) bool
	accounts, anonymous *ratelimit.Limiter
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
	limiter := func(perHour int) *ratelimit.Limiter {
		lim, err := ratelimit.New(ratelimit.Options{Windows: []ratelimit.Window{{Limit: perHour, Per: issuanceWindow}},
			Redis: o.Redis, Prefix: o.KeyPrefix, Errors: RedisErrors, Logger: log, Now: now})
		if err != nil {
			panic(err) // unreachable: the limits are positive
		}
		return lim
	}
	return &issuance{exempt: o.Exempt, accounts: limiter(o.PerHour), anonymous: limiter(o.AnonymousPerHour)}
}

// allow counts item for actor: nil, a LimitError saying how long until it
// may be opened, or ErrNoViewerKey. An editor of the item (editor) and
// exempt actors are not counted.
func (i *issuance) allow(ctx context.Context, actor access.Actor, editor bool, item string) error {
	if i == nil || editor || i.exempt != nil && i.exempt(actor) {
		return nil
	}
	key, lim := viewerKey(actor), i.anonymous
	if key == "" {
		return ErrNoViewerKey
	}
	if !actor.Anonymous && actor.ID != "" {
		lim = i.accounts
	}
	if ok, wait := lim.AllowMember(ctx, key, item); !ok {
		return &media.LimitError{RetryAfter: wait}
	}
	return nil
}
