package openrails

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
)

type tenureStub struct {
	Client
	params billing.AccessTenureParams
	result *billing.AccessTenure
	err    error
}

func (c *tenureStub) GetAccessTenure(_ context.Context, p billing.AccessTenureParams, _ ...openrails.RequestOption) (*billing.AccessTenure, error) {
	c.params = p
	return c.result, c.err
}

func TestTenureReadsAuthoritativeOpenRailsQuery(t *testing.T) {
	at := time.Now()
	customer := billing.CustomerID(uuid.New())
	c := &tenureStub{result: &billing.AccessTenure{AsOf: at, Seconds: map[string]int64{"premium": 123}}}
	got, err := (adapter{c}).Tenure(t.Context(), customer.String(), []string{"premium"}, at)
	if err != nil || got["premium"] != 123 || c.params.CustomerID != customer || !c.params.At.Equal(at) || len(c.params.Entitlements) != 1 || c.params.Entitlements[0] != "premium" {
		t.Fatalf("tenure = %v, params = %+v, error = %v", got, c.params, err)
	}
	c.result = &billing.AccessTenure{Seconds: map[string]int64{}}
	if got, err = (adapter{c}).Tenure(t.Context(), customer.String(), []string{"premium"}, at); err == nil || got != nil {
		t.Fatalf("missing tenure became zero: %v, %v", got, err)
	}
	c.err = errors.New("billing unavailable")
	if got, err = (adapter{c}).Tenure(t.Context(), customer.String(), []string{"premium"}, at); err == nil || got != nil {
		t.Fatalf("billing failure became zero: %v, %v", got, err)
	}
}
