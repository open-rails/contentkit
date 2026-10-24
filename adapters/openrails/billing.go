// Package openrails adapts OpenRails to ContentKit: it is the one place that
// imports both. Billing implements access.Billing over the OpenRails Go
// client, embedded (Engine.MerchantBySlug) or remote alike.
package openrails

import (
	"context"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"

	"github.com/open-rails/contentkit/access"
)

// Client is the read-only OpenRails entitlement capability ContentKit needs.
// Products, prices and purchases are managed directly through OpenRails.
type Client interface {
	CheckEntitlements(context.Context, billing.CheckEntitlementsParams, ...openrails.RequestOption) (*billing.EntitlementCheck, error)
}

var _ Client = (*openrails.Client)(nil)

// Billing is access.Billing over c. Held is one CheckEntitlements: exact keys
// are chunked by billing.MaxEntitlementChecks (each further chunk is one more
// call) and the prefixes ride the first chunk.
func Billing(c Client) access.Billing { return adapter{c} }

type adapter struct{ c Client }

func (a adapter) Held(ctx context.Context, subject string, q access.Query) (access.Answer, error) {
	ans := access.Answer{Keys: make(map[string]bool, len(q.Keys)), Held: make(map[string]access.HeldKeys, len(q.Prefixes))}
	customer, err := billing.ParseCustomerID(subject)
	if err != nil {
		for _, p := range q.Prefixes {
			ans.Held[p] = access.HeldKeys{}
		}
		return ans, nil
	}
	limit := q.Limit
	if limit == 0 {
		limit = access.HeldLimit
	}
	keys := q.Keys
	for first := true; first || len(keys) > 0; first = false {
		n := min(len(keys), billing.MaxEntitlementChecks)
		params := billing.CheckEntitlementsParams{CustomerID: customer, Entitlements: keys[:n]}
		if first {
			params.Prefixes, params.PrefixLimit = q.Prefixes, limit
		}
		keys = keys[n:]
		if len(params.Entitlements) == 0 && len(params.Prefixes) == 0 {
			break
		}
		res, err := a.c.CheckEntitlements(ctx, params)
		if err != nil {
			return access.Answer{}, err
		}
		for _, k := range params.Entitlements {
			ans.Keys[k] = res.Entitlements[k]
		}
		for _, p := range params.Prefixes {
			if h, ok := res.Held[p]; ok {
				ans.Held[p] = access.HeldKeys{Keys: h.Keys, Truncated: h.Truncated}
			}
		}
	}
	return ans, nil
}
