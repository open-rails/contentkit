package openrails

import "encoding/json"

// BillingLifecycle selects unexpected billing outcomes. OpenRails keeps its
// complete event history; ContentKit avoids notifying about a user's own click.
func BillingLifecycle(event Event) bool {
	switch event.EventType {
	case "payment_method_failed", "payment_method_update_required", "payment_authentication_required", "order_collection_stopped":
		var data struct {
			Source string `json:"source"`
		}
		if json.Unmarshal(event.Data, &data) != nil {
			return false
		}
		return data.Source != "customer_checkout" && data.Source != "customer_subscription_change"
	case "premium_ended":
		var data struct {
			AccessEnded bool `json:"access_ended"`
		}
		return json.Unmarshal(event.Data, &data) == nil && data.AccessEnded
	case "premium_renewed":
		var data struct {
			DowngradeApplied bool `json:"downgrade_applied"`
		}
		return json.Unmarshal(event.Data, &data) == nil && data.DowngradeApplied
	default:
		return false
	}
}
