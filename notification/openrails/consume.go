package openrails

import (
	"context"
	"errors"
	"fmt"

	"github.com/open-rails/authkit/notify"
	notification "github.com/open-rails/contentkit/migration/legacyinbox"
)

// RecipientMapper resolves verified source coordinates to a local account.
// False deliberately skips a fact; errors leave the whole page for retry.
// Email equality alone must never establish the association.
type RecipientMapper func(context.Context, Event) (recipientID string, deliver bool, err error)

// Inbox owns both publication and progress. Implementations must commit the
// effects and cursor atomically; the producer stores no consumer state.
type Inbox interface {
	EventCursor(context.Context, notify.OriginScope) (string, error)
	ImportEvents(context.Context, notify.OriginScope, string, string, []notify.Input) (notification.EventImportResult, error)
}

type ConsumeOptions struct {
	Limit           int
	IncludeOptional bool
	Filter          func(Event) bool
}

type ConsumeResult struct {
	Fetched, Delivered, Skipped int
	HasMore                     bool
}

// Consume reads one page and atomically commits its notices and continuation.
// A failed mapping or write commits neither. Other source facts are skipped
// but advance the cursor, so a busy host-event log cannot stall the inbox.
func (s *Source) Consume(ctx context.Context, destination Inbox, mapper RecipientMapper, opts ConsumeOptions) (ConsumeResult, error) {
	var result ConsumeResult
	if destination == nil || mapper == nil || opts.Limit < 0 || opts.Limit > 100 {
		return result, fmt.Errorf("openrails notifications: inbox, mapper and valid limit are required")
	}
	if opts.Limit == 0 {
		opts.Limit = 100
	}
	after, err := destination.EventCursor(ctx, s.Origin())
	if err != nil {
		return result, err
	}
	page, err := s.read(ctx, after, opts.Limit)
	if err != nil {
		return result, err
	}
	result.Fetched, result.HasMore = len(page.Events), page.HasMore
	inputs := make([]notify.Input, 0, len(page.Events))
	skipped := 0
	for _, event := range page.Events {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if event.Source != "notification" || event.Optional && !opts.IncludeOptional || opts.Filter != nil && !opts.Filter(event) {
			skipped++
			continue
		}
		recipient, deliver, err := mapper(ctx, event)
		if err != nil {
			return result, fmt.Errorf("map notification %s: %w", event.ID, err)
		}
		if !deliver {
			skipped++
			continue
		}
		target := notify.Recipient{Audience: notify.AudienceUser, ID: recipient}
		if err := notify.ValidateRecipient(target); err != nil {
			return result, fmt.Errorf("map notification %s: %w", event.ID, err)
		}
		inputs = append(inputs, notify.Input{Recipient: target, Origin: notify.Origin{System: s.name, Scope: event.MerchantID, ID: event.ID, Subject: event.CustomerID}, Kind: event.EventType, Data: event.Data, CreatedAt: event.CreatedAt, Visibility: notify.VisibilityTenant, ActionRequested: event.ActionRequested})
	}
	committed, err := destination.ImportEvents(ctx, s.Origin(), after, page.NextCursor, inputs)
	if errors.Is(err, notification.ErrEventCursorChanged) {
		// Another replica did this work; its next scheduled read continues there.
		return result, nil
	}
	if err != nil {
		return result, err
	}
	result.Delivered, result.Skipped = committed.Published, skipped+committed.Skipped
	return result, nil
}
