package openrails

import (
	"context"
	"fmt"

	"github.com/open-rails/authkit/userinfo"
	notice "github.com/open-rails/contentkit/notification/openrails"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
)

// NewNotificationSource uses the same typed client for embedded and remote
// OpenRails. Name identifies the billing system in stored provenance; consumer
// must be identical for every process writing the same shared inbox.
func NewNotificationSource(client *openrails.Client, name, consumer string) (*notice.Source, error) {
	if client == nil || client.MerchantID().IsZero() {
		return nil, fmt.Errorf("openrails notifications: a merchant-bound client is required")
	}
	if name == "" {
		name = "openrails"
	}
	events, err := client.NotificationEvents(consumer)
	if err != nil {
		return nil, err
	}
	return notice.New(notice.Options{Name: name, MerchantID: client.MerchantID().String(), Consumer: consumer,
		Backend: notificationBackend{consumer: events, merchant: client.MerchantID()}})
}

type notificationBackend struct {
	consumer *openrails.NotificationConsumer
	merchant billing.MerchantID
}

func (b notificationBackend) Register(ctx context.Context) error { return b.consumer.Register(ctx) }
func (b notificationBackend) Retire(ctx context.Context) error   { return b.consumer.Retire(ctx) }

func (b notificationBackend) Pending(ctx context.Context, limit int) ([]notice.Event, error) {
	events, err := b.consumer.Pending(ctx, limit)
	if err != nil {
		return nil, err
	}
	out := make([]notice.Event, 0, len(events))
	for _, event := range events {
		out = append(out, notice.Event{ID: event.ID.String(), MerchantID: event.MerchantID.String(),
			CustomerID: event.CustomerID.String(), EventType: event.EventType, Data: event.Data,
			CreatedAt: event.CreatedAt, ActionRequested: event.ActionRequested, Optional: event.Optional})
	}
	return out, nil
}

func (b notificationBackend) Acknowledge(ctx context.Context, ids []string) error {
	refs := make([]billing.NotificationEventRef, 0, len(ids))
	for _, id := range ids {
		parsed, err := billing.ParseNotificationID(id)
		if err != nil || parsed.IsZero() {
			return fmt.Errorf("openrails notifications: invalid event ID %q", id)
		}
		refs = append(refs, billing.NotificationEventRef{MerchantID: b.merchant, ID: parsed})
	}
	return b.consumer.Acknowledge(ctx, refs)
}

// SameAccounts verifies a shared account UUID against the current host
// directory. Use it only when OpenRails customers and authenticated users share
// that identity namespace. It never associates accounts by email or name.
func SameAccounts(users userinfo.Lookup) notice.RecipientMapper {
	return func(ctx context.Context, event notice.Event) (string, bool, error) {
		if users == nil {
			return "", false, fmt.Errorf("openrails notifications: account directory is required")
		}
		customer, err := billing.ParseCustomerID(event.CustomerID)
		if err != nil || customer.IsZero() {
			return "", false, fmt.Errorf("openrails notifications: invalid customer ID %q", event.CustomerID)
		}
		id := customer.UUID().String()
		found, err := users.Get(ctx, []string{id})
		if err != nil {
			return "", false, fmt.Errorf("openrails notifications: resolve account: %w", err)
		}
		user, exists := found[id]
		if !exists {
			return "", false, nil
		}
		if user.ID != id {
			return "", false, fmt.Errorf("openrails notifications: account directory returned a different subject")
		}
		return id, true, nil
	}
}
