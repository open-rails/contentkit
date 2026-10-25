package critical_test

import (
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/media/layout"
	"github.com/open-rails/contentkit/notification"
	"github.com/open-rails/helpers/notify"
)

func TestLegacyNotificationMerchantBinding(t *testing.T) {
	t.Run("CK-J25-S16_shared_apps_cannot_reassign_legacy_merchant", func(t *testing.T) {
		w := newWorld(t, func(c *ck.Config) { c.Accounts = &ck.AccountsConfig{Schema: c.Database.Schema} })
		origin := notify.OriginScope{System: "openrails", Scope: uuid.NewString()}
		store := func(a *application) *notification.Store {
			s, err := notification.New(notification.Options{Pool: w.pool, Schema: w.schema, Tenant: a.cfg.Scope.AppID.String(), AppID: a.cfg.Scope.AppID.String(), Shared: &notification.Config{Schema: w.schema, Namespace: (layout.Scope{TenantID: a.cfg.Scope.TenantID.String()}).Partition(), SharedOrigins: []notify.OriginScope{origin}}})
			must(t, err)
			return s
		}
		a, b, foreign := store(w.apps[0]), store(w.apps[1]), store(w.apps[2])
		var wg sync.WaitGroup
		failures := make(chan error, 2)
		for _, s := range []*notification.Store{a, b} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				failures <- s.BindMerchant(t.Context(), w.apps[0].cfg.Scope.TenantID.String(), origin)
			}()
		}
		wg.Wait()
		close(failures)
		for err := range failures {
			must(t, err)
		}
		if err := a.BindMerchant(t.Context(), w.apps[0].cfg.Scope.TenantID.String(), notify.OriginScope{System: origin.System, Scope: uuid.NewString()}); !errors.Is(err, notification.ErrMerchantBinding) {
			t.Fatalf("legacy account directory accepted a replacement merchant: %v", err)
		}
		if err := foreign.BindMerchant(t.Context(), w.apps[2].cfg.Scope.TenantID.String(), origin); !errors.Is(err, notification.ErrMerchantBinding) {
			t.Fatalf("legacy merchant crossed account tenants: %v", err)
		}
	})
}
