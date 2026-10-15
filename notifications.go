package contentkit

import (
	"context"
	"errors"
	"time"

	source "github.com/open-rails/contentkit/notification/openrails"
	riverhelpers "github.com/open-rails/helpers/river"
)

type billingNotificationJobs struct{ source.Jobs }

// OpenRailsNotifications attaches an authoritative billing source before Start
// and returns its shared River contribution. Hosts supply only verified account
// mapping; delivery selection, retry and inbox presentation are ContentKit's.
func (e *Engine) OpenRailsNotifications(s *source.Source, mapper source.RecipientMapper) (riverhelpers.Contribution, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return e.OpenRailsNotificationsContext(ctx, s, mapper)
}

// OpenRailsNotificationsContext is OpenRailsNotifications with a caller's startup
// context. Scoped engines verify the persisted tenant/merchant binding first.
func (e *Engine) OpenRailsNotificationsContext(ctx context.Context, s *source.Source, mapper source.RecipientMapper) (riverhelpers.Contribution, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.started || e.closed {
		return riverhelpers.Contribution{}, errors.New("contentkit: attach notification sources before Start")
	}
	if s == nil || mapper == nil {
		return riverhelpers.Contribution{}, errors.New("contentkit: notification source and mapper required")
	}
	if e.scope != nil {
		if err := e.notices.BindMerchant(ctx, e.scope.TenantID.String(), s.Origin()); err != nil {
			return riverhelpers.Contribution{}, err
		}
	}
	if err := e.notices.AllowOrigin(s.Origin()); err != nil {
		return riverhelpers.Contribution{}, err
	}
	if e.notificationJobs == nil {
		e.notificationJobs = &billingNotificationJobs{}
	}
	return e.notificationJobs.Add(s, e.notices, mapper, e.notices.Namespace())
}
