package openrails

import (
	"context"
	"fmt"

	"github.com/open-rails/helpers/notify"
)

// Binding is a trusted host's account association. Email equality is not a
// binding; resolve these IDs from the authenticated account/merchant link.
type Binding struct{ CustomerID, RecipientID string }

// ScanOptions bounds one worker invocation. Continue with the returned cursor;
// after Complete, restart periodically with an empty cursor to catch notices
// committed late. MaxPages defaults to 10 and must not exceed 100.
type ScanOptions struct {
	Cursor   string
	MaxPages int
}

// ScanResult reports successful publish calls (including idempotent replays).
// On failure NextCursor resumes at the start of the incomplete source page.
type ScanResult struct {
	Processed  int
	NextCursor string
	Complete   bool
}

// Sync copies bounded retained-inbox pages into the application's shared
// notification publisher. Historical action requests remain general notices;
// live business resolution stays with OpenRails. It never marks source notices read.
// Imported history remains until the destination's explicit retention/erasure
// policy removes it, even if OpenRails supersedes or expires its source row.
func (s *Source) Sync(ctx context.Context, destination notify.Publisher, binding Binding, opts ScanOptions) (ScanResult, error) {
	result := ScanResult{NextCursor: opts.Cursor}
	recipient := notify.Recipient{Audience: notify.AudienceUser, ID: binding.RecipientID}
	if err := notify.ValidateRecipient(recipient); err != nil {
		return result, err
	}
	if destination == nil || binding.CustomerID == "" || opts.MaxPages < 0 || opts.MaxPages > 100 {
		return result, fmt.Errorf("openrails notifications: destination, customer and valid page limit are required")
	}
	if opts.MaxPages == 0 {
		opts.MaxPages = 10
	}
	for range opts.MaxPages {
		page, err := s.Fetch(ctx, binding.CustomerID, result.NextCursor)
		if err != nil {
			return result, err
		}
		for _, n := range page.Data {
			_, err := destination.Publish(ctx, notify.Input{Recipient: recipient, Origin: notify.Origin{System: s.name, Scope: n.MerchantID, ID: n.ID}, Kind: n.EventType, Data: n.Data, CreatedAt: n.CreatedAt, ActionRequested: n.ActionRequested})
			if err != nil {
				return result, fmt.Errorf("openrails notifications: import: %w", err)
			}
			result.Processed++
		}
		if page.NextCursor == nil || *page.NextCursor == "" {
			result.NextCursor = ""
			result.Complete = true
			return result, nil
		}
		result.NextCursor = *page.NextCursor
	}
	return result, nil
}
