package notification

import (
	"context"
	"github.com/open-rails/helpers/notify"
)

// EraseAppRecipient fences only this configured app's notices, never the
// tenant inbox or another app's receipts. It also identifies the app's legacy
// native origin without including shared billing origins.
func (s *Store) EraseAppRecipient(ctx context.Context, r notify.Recipient) error {
	return s.Store.EraseAppRecipient(ctx, r, s.appID, s.nativeOrigin)
}

// ErasedRecipients is the canonical account/app erasure list for replay after
// restoring only this app's data. It never returns a sibling's app-only fences.
func (s *Store) ErasedRecipients(ctx context.Context, after string, limit int) ([]notify.Recipient, error) {
	return s.Store.AppErasedRecipients(ctx, notify.AudienceUser, s.appID, s.nativeOrigin, after, limit)
}
