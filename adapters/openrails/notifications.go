package openrails

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/open-rails/authkit/userinfo"
	notice "github.com/open-rails/contentkit/notification/openrails"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
)

// NewNotificationSource uses the same typed client for embedded and remote
// OpenRails. Name identifies the billing system in stored provenance. The inbox
// owns its continuation; no source consumer registration or ACK is needed.
func NewNotificationSource(client *openrails.Client, name string) (*notice.Source, error) {
	if client == nil || client.MerchantID().IsZero() {
		return nil, fmt.Errorf("openrails notifications: a merchant-bound client is required")
	}
	if name == "" {
		name = "openrails"
	}
	return notice.New(notice.Options{Name: name, MerchantID: client.MerchantID().String(),
		Backend: notificationBackend{client: client}})
}

type notificationBackend struct {
	client *openrails.Client
}

func (b notificationBackend) ListEvents(ctx context.Context, after string, limit int) (notice.EventPage, error) {
	page, err := b.client.ListEvents(ctx, billing.EventQuery{After: after, Limit: limit})
	if err != nil {
		var status *billing.StatusError
		if errors.As(err, &status) && status.Code == billing.CodeEventCursorExpired {
			return notice.EventPage{}, errors.Join(notice.ErrCursorExpired, err)
		}
		return notice.EventPage{}, err
	}
	out := make([]notice.Event, 0, len(page.Events))
	for _, event := range page.Events {
		out = append(out, notice.Event{ID: event.ID, MerchantID: event.MerchantID.String(),
			Source: event.Source, Version: event.Version, EventType: event.Type, Data: event.Data,
			Subject: notice.EventSubject{Type: event.Subject.Type, ID: event.Subject.ID}, CreatedAt: event.OccurredAt})
	}
	return notice.EventPage{Events: out, NextCursor: page.NextCursor, HasMore: page.HasMore, RetentionSeconds: page.RetentionSeconds}, nil
}

// SameAccounts verifies a shared account UUID against the current host
// directory. Use it only when OpenRails customers and authenticated users share
// that identity namespace. Native OpenRails accounts are admitted; any external
// issuer must be explicitly named. Unknown historical issuers are skipped.
// It never associates accounts by email or name.
func SameAccounts(users userinfo.Lookup, issuers ...string) notice.RecipientMapper {
	return func(ctx context.Context, event notice.Event) (string, bool, error) {
		if event.AccountIssuer == nil || *event.AccountIssuer != "" && !slices.Contains(issuers, *event.AccountIssuer) {
			return "", false, nil
		}
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
