package openrails

import (
	"context"
	"fmt"
	"time"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
)

var _ access.TenureReader = adapter{}

// tenureClient is optional so custom billing clients implementing the existing
// Client contract can keep serving without access-history statistics.
type tenureClient interface {
	GetAccessTenure(context.Context, billing.AccessTenureParams, ...openrails.RequestOption) (*billing.AccessTenure, error)
}

// Tenure reads OpenRails' authoritative aggregate; the adapter does not load
// windows or calculate durations itself.
func (a adapter) Tenure(ctx context.Context, subject string, keys []string, at time.Time) (map[string]int64, error) {
	client, ok := a.c.(tenureClient)
	if !ok {
		return nil, fmt.Errorf("billing client does not support access tenure")
	}
	customer, err := billing.ParseCustomerID(subject)
	if err != nil {
		return nil, fmt.Errorf("access tenure: %w", err)
	}
	out := make(map[string]int64, len(keys))
	for len(keys) > 0 {
		n := min(len(keys), billing.MaxEntitlementChecks)
		page, err := client.GetAccessTenure(ctx, billing.AccessTenureParams{CustomerID: customer, Entitlements: keys[:n], At: at})
		if err != nil {
			return nil, err
		}
		if page == nil {
			return nil, fmt.Errorf("billing returned no access tenure")
		}
		for _, key := range keys[:n] {
			seconds, present := page.Seconds[key]
			if !present {
				return nil, fmt.Errorf("billing omitted access tenure for %q", key)
			}
			out[key] = seconds
		}
		keys = keys[n:]
	}
	return out, nil
}
