package access

import "context"

const (
	// MaxPrefixes bounds Query.Prefixes (and so a Gate's member and owned kinds).
	MaxPrefixes = 10
	// DefaultHeldLimit is the keys read per keyspace when GateConfig.HeldLimit is 0.
	DefaultHeldLimit = 1000
	// MaxHeldLimit bounds GateConfig.HeldLimit.
	MaxHeldLimit = 10000
)

// Query is one billing read: exact keys and keyspace prefixes.
type Query struct {
	Keys     []string // exact keys; an implementation over a bounded API chunks them
	Prefixes []string // at most MaxPrefixes, each ending in ':'
	Limit    int      // keys per prefix
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

// Entitlements is ContentKit's only billing read: which keys subject holds
// now. One Held call is one read (adapters/openrails: one CheckEntitlements).
// A subject that cannot hold keys answers an empty Answer, not an error.
type Entitlements interface {
	Held(ctx context.Context, subject string, q Query) (Answer, error)
}
