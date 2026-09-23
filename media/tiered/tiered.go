// Package tiered is an optional visibility policy: it maps an item's level to
// entitlement keys and asks a Checker which ones the actor holds. It imports no
// billing client; a host adapts OpenRails in one expression:
//
//	checker := tiered.CheckerFunc(func(ctx context.Context, subject string, keys []string) (map[string]bool, error) {
//		return client.CheckEntitlements(ctx, subject, keys, time.Time{}) // at most openrails.MaxEntitlementChecks keys
//	})
package tiered

import (
	"context"
	"errors"
	"fmt"

	"github.com/open-rails/contentkit/access"
)

// Level is an item's visibility level.
type Level string

const (
	Public     Level = "public"      // anyone
	Members    Level = "members"     // membership key
	PPV        Level = "ppv"         // purchase key
	MembersPPV Level = "members_ppv" // purchase key; membership is checked only at checkout
	Premium    Level = "premium"     // premium key
)

// Checker reports which of keys the subject holds. Missing keys are not held.
type Checker interface {
	Check(ctx context.Context, subject string, keys []string) (map[string]bool, error)
}

// CheckerFunc adapts a function to Checker.
type CheckerFunc func(ctx context.Context, subject string, keys []string) (map[string]bool, error)

func (f CheckerFunc) Check(ctx context.Context, subject string, keys []string) (map[string]bool, error) {
	return f(ctx, subject, keys)
}

// Policy is one item's level and the host's entitlement keys for it. A
// purchase is ownership: a set Purchase key grants every paid level, so a
// buyer keeps access after the level or their membership changes.
type Policy struct {
	Level      Level
	Membership string // e.g. "channel:{id}:membership"
	Purchase   string // e.g. "post:{billing key}"
	Premium    string // e.g. "site:premium"
}

// ErrPolicy reports a policy with an unknown level or a missing required key.
var ErrPolicy = errors.New("tiered: invalid policy")

// keys returns the entitlement keys any one of which grants p; nil for Public.
func (p Policy) keys() ([]string, error) {
	var need string
	switch p.Level {
	case Public:
		return nil, nil
	case Members:
		need = p.Membership
	case PPV, MembersPPV:
		need = p.Purchase
	case Premium:
		need = p.Premium
	default:
		return nil, fmt.Errorf("%w: unknown level %q", ErrPolicy, p.Level)
	}
	if need == "" {
		return nil, fmt.Errorf("%w: level %q has no key", ErrPolicy, p.Level)
	}
	if p.Purchase != "" && p.Purchase != need {
		return []string{need, p.Purchase}, nil
	}
	return []string{need}, nil
}

// Decide reports whether actor may access an item under p.
func Decide(ctx context.Context, c Checker, actor access.Actor, p Policy) (bool, error) {
	ok, err := DecideAll(ctx, c, actor, []Policy{p})
	if err != nil {
		return false, err
	}
	return ok[0], nil
}

// DecideAll decides many items with one Checker call. Anonymous actors get
// only Public items and never reach the Checker. Any error denies everything.
func DecideAll(ctx context.Context, c Checker, actor access.Actor, ps []Policy) ([]bool, error) {
	out := make([]bool, len(ps))
	grants := make([][]string, len(ps))
	var keys []string
	seen := map[string]bool{}
	for i, p := range ps {
		k, err := p.keys()
		if err != nil {
			return nil, err
		}
		if k == nil {
			out[i] = true
			continue
		}
		grants[i] = k
		for _, key := range k {
			if !seen[key] {
				seen[key] = true
				keys = append(keys, key)
			}
		}
	}
	if len(keys) == 0 || actor.Anonymous || actor.ID == "" {
		return out, nil
	}
	held, err := c.Check(ctx, actor.ID, keys)
	if err != nil {
		return nil, fmt.Errorf("tiered: check entitlements: %w", err)
	}
	for i, k := range grants {
		for _, key := range k {
			out[i] = out[i] || held[key]
		}
	}
	return out, nil
}
