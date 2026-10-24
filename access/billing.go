package access

import (
	"context"
	"time"
)

// TenureReader optionally extends Billing with elapsed access to its
// entitlement keys. Tenure answers seconds per requested key as of at, merging
// overlapping product-access windows and excluding future time. An unavailable
// history returns an error, not an invented zero. Adapters may implement it to
// populate the reader's account statistics without a host-specific endpoint.
type TenureReader interface {
	Tenure(ctx context.Context, subject string, keys []string, at time.Time) (map[string]int64, error)
}

// HeldLimit is the keys one billing read returns per keyspace; a viewer
// holding more gets an incomplete Filter.
const HeldLimit = 1000

// Billing reads authoritative entitlement grants from the billing service.
// Products, prices, checkout, subscriptions and grants are managed directly
// through that service's API; ContentKit does not copy its catalog or queue
// billing mutations. adapters/openrails supports embedded and HTTP clients.
type Billing interface {
	// Held answers q for subject in one read. A subject that cannot hold
	// keys (no customer id) answers an empty Answer, not an error.
	Held(ctx context.Context, subject string, q Query) (Answer, error)
}

// Query is one entitlement read: exact keys and keyspace prefixes.
type Query struct {
	Keys     []string // exact keys
	Prefixes []string // each ending in ':'
	Limit    int      // keys per prefix; 0 is HeldLimit
}

// HeldKeys is the keys a subject holds under one prefix, in byte order.
type HeldKeys struct {
	Keys      []string
	Truncated bool // more than Query.Limit keys are held
}

// Answer answers a Query.
type Answer struct {
	Keys map[string]bool     // every requested key; absent means not held
	Held map[string]HeldKeys // one entry per requested prefix
}
