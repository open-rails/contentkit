package discovery

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"testing"
	"time"
)

type testTenure struct {
	failed bool
	calls  int
}

func (r *testTenure) Tenure(_ context.Context, subject string, keys []string, at time.Time) (map[string]int64, error) {
	r.calls++
	if subject != "reader" || !slices.Equal(keys, []string{"premium"}) || at.IsZero() {
		return nil, errors.New("incorrect subject, tiers or time")
	}
	if r.failed {
		return nil, errors.New("billing unavailable")
	}
	return map[string]int64{"premium": 42}, nil
}

func TestMyStatsServesBillingTenureWithoutCouplingCommentAvailability(t *testing.T) {
	f := newFixture(t, nil, "")
	reader := &testTenure{}
	f.svc.o.Tenure = reader
	var got MyStats
	if status, code := f.call(http.MethodGet, "main.test", "reader", "/me/stats", nil, &got); status != http.StatusOK || got.AccessSeconds["premium"] != 42 {
		t.Fatalf("stats with billing: %d %s %+v", status, code, got)
	}
	reader.failed = true
	got = MyStats{}
	if status, code := f.call(http.MethodGet, "main.test", "reader", "/me/stats", nil, &got); status != http.StatusOK || got.AccessSeconds != nil || got.Comments != 0 {
		t.Fatalf("billing failure hid content stats or invented tenure: %d %s %+v", status, code, got)
	}
	before := reader.calls
	if status, _ := f.call(http.MethodGet, "main.test", "", "/me/stats", nil, nil); status != http.StatusUnauthorized || reader.calls != before {
		t.Fatal("anonymous account read reached billing")
	}
}
