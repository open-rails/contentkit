// Package openrails adapts OpenRails to ContentKit: it is the one place that
// imports both. Entitlements implements access.Entitlements over the batch
// Client.CheckEntitlements.
package openrails

import (
	"context"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"

	"github.com/open-rails/contentkit/access"
)

// Client is the OpenRails method the adapter needs; *openrails.Client
// (embedded or remote) implements it.
type Client interface {
	CheckEntitlements(context.Context, billing.CustomerID, billing.CheckEntitlementsParams, ...openrails.RequestOption) (*billing.EntitlementCheck, error)
}

var _ Client = (*openrails.Client)(nil)

// Entitlements answers each Held call with one CheckEntitlements: exact keys
// are chunked by billing.MaxEntitlementChecks (each further chunk is one more
// call) and the prefixes ride the first chunk. A subject that is not a
// customer UUID holds nothing and costs no call.
func Entitlements(c Client) access.Entitlements { return entitlements{c} }

type entitlements struct{ c Client }

func (e entitlements) Held(ctx context.Context, subject string, q access.Query) (access.Answer, error) {
	ans := access.Answer{Keys: make(map[string]bool, len(q.Keys)), Held: make(map[string]access.HeldKeys, len(q.Prefixes))}
	customer, err := billing.ParseCustomerID(subject)
	if err != nil {
		for _, p := range q.Prefixes {
			ans.Held[p] = access.HeldKeys{}
		}
		return ans, nil
	}
	keys := q.Keys
	for first := true; first || len(keys) > 0; first = false {
		n := min(len(keys), billing.MaxEntitlementChecks)
		params := billing.CheckEntitlementsParams{Entitlements: keys[:n]}
		if first {
			params.Prefixes, params.PrefixLimit = q.Prefixes, q.Limit
		}
		keys = keys[n:]
		if len(params.Entitlements) == 0 && len(params.Prefixes) == 0 {
			break
		}
		res, err := e.c.CheckEntitlements(ctx, customer, params)
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
