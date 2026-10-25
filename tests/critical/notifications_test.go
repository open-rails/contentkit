package critical_test

import (
	"encoding/json"
	"errors"
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
	t.Run("CK-J25-S01_origin_and_recipient_deduplicate", func(t *testing.T) {
		user, token := w.person(t, 0)
		in := publication(user.ID)
		first, err := a.publishInbox(t.Context(), in)
		must(t, err)
		second, err := a.publishInbox(t.Context(), in)
		must(t, err)
		if len(first.Items) != 1 || len(second.Items) != 1 || first.Items[0].ID != second.Items[0].ID {
			t.Fatal("source replay created a second receipt")
		}
		inbox, err := a.accountInbox(token).List(t.Context(), inboxQuery{})
		must(t, err)
		if len(inbox.Items) != 1 || inbox.Items[0].ID != first.Items[0].ID {
			t.Fatalf("recipient inbox did not contain exactly its receipt: %+v", inbox)
		}
		other, otherToken := w.person(t, 0)
		in.Notifications[0].Recipient.ID = other.ID
		sharedOrigin, err := a.publishInbox(t.Context(), in)
		must(t, err)
		if len(sharedOrigin.Items) != 1 || sharedOrigin.Items[0].ID == first.Items[0].ID {
			t.Fatal("origin-only deduplication merged different recipients")
		}
		otherInbox, err := a.accountInbox(otherToken).List(t.Context(), inboxQuery{})
		must(t, err)
		if len(otherInbox.Items) != 1 || otherInbox.Items[0].ID != sharedOrigin.Items[0].ID {
			t.Fatal("second recipient did not receive its independent receipt")
		}
		inbox, err = a.accountInbox(token).List(t.Context(), inboxQuery{})
		must(t, err)
		if len(inbox.Items) != 1 || inbox.Items[0].ID != first.Items[0].ID {
			t.Fatal("second recipient changed the original inbox")
		}
	})
	t.Run("CK-J25-S02_other_user_cannot_read_or_mutate_receipt", func(t *testing.T) {
		owner, ownerToken := w.person(t, 0)
		_, stranger := w.person(t, 0)
		published, err := a.publishInbox(t.Context(), publication(owner.ID))
		must(t, err)
		if len(published.Items) != 1 {
			t.Fatal("receipt fixture missing")
		}
		own, err := a.accountInbox(ownerToken).Get(t.Context(), published.Items[0].ID)
		must(t, err)
		if own.ID != published.Items[0].ID || own.Recipient.ID != owner.ID {
			t.Fatal("the native item route did not return the owner's receipt")
		}
		client := a.accountInbox(stranger)
		inbox, err := client.List(t.Context(), inboxQuery{})
		must(t, err)
		if len(inbox.Items) != 0 {
			t.Fatal("another account received private notification data")
		}
		_, err = client.Get(t.Context(), published.Items[0].ID)
		requireError(t, err, 404, "not_found")
		_, err = client.SetRead(t.Context(), published.Items[0].ID, true)
		requireError(t, err, 404, "not_found")
		_, err = client.SetDismissed(t.Context(), published.Items[0].ID, true)
		requireError(t, err, 404, "not_found")
		ownerInbox, err := a.accountInbox(ownerToken).List(t.Context(), inboxQuery{})
		must(t, err)
		if len(ownerInbox.Items) != 1 || ownerInbox.Items[0].ID != published.Items[0].ID || ownerInbox.Items[0].ReadAt != nil || ownerInbox.Items[0].DismissedAt != nil {
			t.Fatal("refused stranger mutation still changed the owner's receipt")
		}
	})
	t.Run("CK-J25-S03_read_and_archive_do_not_resolve_source_work", func(t *testing.T) {
		user, token := w.person(t, 0)
		published, err := a.publishInbox(t.Context(), publication(user.ID))
		must(t, err)
		if len(published.Items) != 1 {
			t.Fatal("receipt fixture missing")
		}
		id, client := published.Items[0].ID, a.accountInbox(token)
		status := published.Items[0].Status
		if !published.Items[0].ActionRequested {
			t.Fatal("actionable notice lost producer intent")
		}
		read, err := client.SetRead(t.Context(), id, true)
		must(t, err)
		if read.ReadAt == nil || read.Status != status || read.ResolvedAt != nil {
			t.Fatal("reading resolved source work")
		}
		dismissed, err := client.SetDismissed(t.Context(), id, true)
		must(t, err)
		if dismissed.DismissedAt == nil || dismissed.Status != status || dismissed.ResolvedAt != nil {
			t.Fatal("archiving resolved source work")
		}
		active, err := client.List(t.Context(), inboxQuery{})
		must(t, err)
		if len(active.Items) != 0 {
			t.Fatal("archived receipt remained in the active inbox")
		}
		archivedOnly := true
		archived, err := client.List(t.Context(), inboxQuery{Dismissed: &archivedOnly})
		must(t, err)
		if len(archived.Items) != 1 || archived.Items[0].ID != id {
			t.Fatal("archived receipt disappeared")
		}
		_, err = client.SetDismissed(t.Context(), id, false)
		must(t, err)
		unread, err := client.SetRead(t.Context(), id, false)
		must(t, err)
		if unread.ReadAt != nil || unread.DismissedAt != nil || unread.Status != status || unread.ResolvedAt != nil {
			t.Fatal("restore/unread changed the source workflow")
		}
	})
	t.Run("CK-J25-S04_foreign_or_unknown_recipient_is_refused", func(t *testing.T) {
		foreign, foreignToken := w.person(t, 1)
		local, token := w.person(t, 0)
		publisher, err := a.inbox.Notifications("application")
		must(t, err)
		scope, err := a.inbox.NotificationScope()
		must(t, err)
		// Native publication accepts eligible recipients and terminally drops
		// accounts outside this tenant. A subsequent read is not publication.
		for index, recipient := range []string{foreign.ID, uuid.NewString()} {
			valid := publication(local.ID).Notifications[0]
			invalid := publication(recipient).Notifications[0]
			must(t, publisher.Publish(t.Context(), []notify.Publication{publicationFrom(valid, 1), publicationFrom(invalid, 1)}))
			inbox, err := a.accountInbox(token).List(t.Context(), inboxQuery{})
			must(t, err)
			if len(inbox.Items) != index+1 || inbox.Items[0].Origin.ID != valid.Origin.ID {
				t.Fatal("native publication did not deliver the valid tenant recipient")
			}
			var stored int
			must(t, w.pool.QueryRow(t.Context(), `SELECT
 (SELECT count(*) FROM contentkit_accounts.notify_items WHERE scope=$1 AND recipient=$2) +
 (SELECT count(*) FROM contentkit_accounts.notify_recipients WHERE scope=$1 AND recipient=$2)`, scope, recipient).Scan(&stored))
			if stored != 0 {
				t.Fatal("foreign or unknown recipient acquired inbox storage in this tenant")
			}
		}
		foreignInbox, err := w.apps[2].accountInbox(foreignToken).List(t.Context(), inboxQuery{})
		must(t, err)
		if len(foreignInbox.Items) != 0 {
			t.Fatal("foreign tenant received this app's private notice")
		}
		// Malformed mutations, unlike terminal non-delivery, refuse the entire
		// batch before the preceding valid mutation can take effect.
		fresh, freshToken := w.person(t, 0)
		valid := publication(fresh.ID).Notifications[0]
		invalid := publication("not-an-account-id").Notifications[0]
		err = publisher.Publish(t.Context(), []notify.Publication{publicationFrom(valid, 1), publicationFrom(invalid, 1)})
		if !errors.Is(err, notify.ErrInvalid) {
			t.Fatalf("malformed recipient batch accepted: %v", err)
		}
		inbox, err := a.accountInbox(freshToken).List(t.Context(), inboxQuery{})
		must(t, err)
		if len(inbox.Items) != 0 {
			t.Fatal("malformed batch partially published the preceding valid receipt")
		}
	})
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
