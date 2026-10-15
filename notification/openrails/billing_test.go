package openrails

import (
	"encoding/json"
	"testing"
)

func TestBillingLifecycleSelectsUnexpectedOutcomes(t *testing.T) {
	for _, tc := range []struct {
		kind, payload string
		want          bool
	}{
		{"payment_method_failed", `{"retry_status":"scheduled"}`, true},
		{"payment_method_update_required", `{}`, true},
		{"payment_authentication_required", `{}`, true},
		{"order_collection_stopped", `{}`, true},
		{"payment_method_failed", `{"source":"customer_checkout"}`, false},
		{"payment_method_failed", `{"source":"customer_subscription_change"}`, false},
		{"payment_method_failed", `{"source":"ccbill_webhook"}`, true},
		{"premium_ended", `{"access_ended":true}`, true},
		{"premium_ended", `{"reason":"user_cancel","ended_at":"2026-11-15T00:00:00Z"}`, false},
		{"premium_renewed", `{"downgrade_applied":true}`, true},
		{"premium_renewed", `{}`, false},
		{"premium_started", `{}`, false},
		{"subscription_canceled", `{}`, false},
		{"agreement_revoked", `{}`, false},
		{"future.new_kind", `{}`, false},
	} {
		t.Run(tc.kind+tc.payload, func(t *testing.T) {
			if got := BillingLifecycle(Event{EventType: tc.kind, Data: json.RawMessage(tc.payload)}); got != tc.want {
				t.Fatalf("got %t, want %t", got, tc.want)
			}
		})
	}
}
