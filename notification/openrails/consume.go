package openrails

import (
	"context"
	"errors"
	"fmt"

	"github.com/open-rails/helpers/notify"
)

// RecipientMapper resolves the event's verified merchant/customer coordinates
// through host-owned account records. deliver=false deliberately skips the
// event and allows its ACK. An error keeps it pending for retry. Email equality
// alone must never establish the association. Future event kinds pass through.
type RecipientMapper func(context.Context, Event) (recipientID string, deliver bool, err error)

// ConsumeOptions bounds one worker call. Optional events default to skip+ACK;
// opt in to map them, applying any per-user preferences inside RecipientMapper.
type ConsumeOptions struct {
	Limit           int
	IncludeOptional bool
}

// ConsumeResult counts this attempt, including idempotent destination replays.
// Failed events remain pending. Acknowledged counts only a successful source ACK.
type ConsumeResult struct{ Fetched, Delivered, Skipped, Acknowledged int }

// Consume publishes one bounded pending page, then ACKs only committed or
// explicitly skipped events. Each consumer has independent ACKs. A failed ACK
// safely replays destination publication; local read/dismiss state survives.
// One failed mapping/publication does not prevent other events in the page.
// The host owns scheduling and retries; there is no cursor or time watermark.
func (s *Source) Consume(ctx context.Context, destination notify.Publisher, mapper RecipientMapper, opts ConsumeOptions) (ConsumeResult, error) {
	var result ConsumeResult
	if destination == nil || mapper == nil || opts.Limit < 0 || opts.Limit > 100 {
		return result, fmt.Errorf("openrails notifications: publisher, mapper and valid limit are required")
	}
	if opts.Limit == 0 {
		opts.Limit = 100
	}
	events, err := s.pending(ctx, opts.Limit)
	if err != nil {
		return result, err
	}
	result.Fetched = len(events)
	ids := make([]string, 0, len(events))
	var failures []error
	for _, event := range events {
		if err := ctx.Err(); err != nil {
			failures = append(failures, err)
			break
		}
		if event.Optional && !opts.IncludeOptional {
			ids = append(ids, event.ID)
			result.Skipped++
			continue
		}
		recipient, deliver, err := mapper(ctx, event)
		if err != nil {
			failures = append(failures, fmt.Errorf("map notification %s: %w", event.ID, err))
			continue
		}
		if !deliver {
			ids = append(ids, event.ID)
			result.Skipped++
			continue
		}
		target := notify.Recipient{Audience: notify.AudienceUser, ID: recipient}
		if err := notify.ValidateRecipient(target); err != nil {
			failures = append(failures, fmt.Errorf("map notification %s: %w", event.ID, err))
			continue
		}
		_, err = destination.Publish(ctx, notify.Input{Recipient: target, Origin: notify.Origin{System: s.name, Scope: event.MerchantID, ID: event.ID, Subject: event.CustomerID}, Kind: event.EventType, Data: event.Data, CreatedAt: event.CreatedAt, ActionRequested: event.ActionRequested})
		if errors.Is(err, notify.ErrRecipientErased) {
			ids = append(ids, event.ID)
			result.Skipped++
			continue
		}
		if err != nil {
			failures = append(failures, fmt.Errorf("publish notification %s: %w", event.ID, err))
			continue
		}
		ids = append(ids, event.ID)
		result.Delivered++
	}
	if err := s.acknowledge(ctx, ids); err != nil {
		failures = append(failures, err)
	} else {
		result.Acknowledged = len(ids)
	}
	return result, errors.Join(failures...)
}
