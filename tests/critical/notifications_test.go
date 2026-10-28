package critical_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/authkit/notify"
)

func TestNotificationReceiptBoundaries(t *testing.T) {
	w := newWorld(t)
	a := w.apps[0]

	publication := func(recipient string) inboxPublication {
		return inboxPublication{Notifications: []notify.Input{{Recipient: notify.Recipient{Audience: "user", ID: recipient}, Origin: notify.Origin{System: "critical", Scope: a.cfg.Scope.AppID.String(), ID: uuid.NewString()}, Kind: "critical.notice", Data: json.RawMessage(`{"message":"Private recipient data"}`), CreatedAt: time.Now(), Status: "open", ActionRequested: true}}}
	}
	t.Run("CK-J25-S05_erased_recipient_cannot_be_recreated_by_delayed_publish", func(t *testing.T) {
		user, token := w.person(t, 0)
		in := publication(user.ID)
		initial, err := a.publishInbox(t.Context(), in)
		must(t, err)
		if len(initial.Items) != 1 {
			t.Fatal("pre-erasure receipt fixture missing")
		}
		must(t, a.engine.AnonymizeAccount(t.Context(), user.ID))
		for _, delayed := range []inboxPublication{in, publication(user.ID)} {
			got, err := a.publishInbox(t.Context(), delayed)
			must(t, err) // Native publication acknowledges terminal non-delivery.
			if len(got.Items) != 0 {
				t.Fatal("erased recipient acquired a delayed receipt")
			}
		}
		inbox, err := a.accountInbox(token).List(t.Context(), inboxQuery{})
		must(t, err)
		if len(inbox.Items) != 0 {
			t.Fatal("erased inbox became visible again")
		}
		scope, err := a.inbox.NotificationScope()
		must(t, err)
		var receipts, fences int
		must(t, w.pool.QueryRow(t.Context(), `SELECT
 (SELECT count(*) FROM contentkit_accounts.notify_items WHERE scope=$1 AND recipient=$2 AND app_id=$3),
 (SELECT count(*) FROM contentkit_accounts.notify_app_erasures WHERE scope=$1 AND recipient=$2 AND app_id=$3)`, scope, user.ID, a.cfg.Scope.AppID.String()).Scan(&receipts, &fences))
		if receipts != 0 || fences != 1 {
			t.Fatalf("delayed publication recreated erased storage or lost its fence: receipts=%d fences=%d", receipts, fences)
		}
	})
}
