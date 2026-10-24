package critical_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/notification"
	"github.com/open-rails/helpers/notify"
)

func TestNotificationReceiptBoundaries(t *testing.T) {
	w := newWorld(t)
	a := w.apps[0]
	admin := a.engine.Client()
	publication := func(recipient string) notification.Publication {
		return notification.Publication{Notifications: []notify.Input{{Recipient: notify.Recipient{Audience: "user", ID: recipient}, Origin: notify.Origin{System: "critical", Scope: a.cfg.Scope.AppID.String(), ID: uuid.NewString()}, Kind: "critical.notice", Data: json.RawMessage(`{"message":"Private recipient data"}`), CreatedAt: time.Now(), Status: "open", ActionRequested: true}}}
	}
	t.Run("CK-J25-S01_origin_and_recipient_deduplicate", func(t *testing.T) {
		user, token := w.person(t, 0)
		in := publication(user.ID)
		first, err := admin.PostNotificationsPublish(t.Context(), in)
		must(t, err)
		second, err := admin.PostNotificationsPublish(t.Context(), in)
		must(t, err)
		if len(first.Items) != 1 || len(second.Items) != 1 || first.Items[0].ID != second.Items[0].ID {
			t.Fatal("source replay created a second receipt")
		}
		inbox, err := a.client(token).GetNotificationsInbox(t.Context(), ck.GetNotificationsInboxQuery{})
		must(t, err)
		if len(inbox.Items) != 1 || inbox.Items[0].ID != first.Items[0].ID {
			t.Fatalf("recipient inbox did not contain exactly its receipt: %+v", inbox)
		}
	})
	t.Run("CK-J25-S02_other_user_cannot_read_or_mutate_receipt", func(t *testing.T) {
		owner, _ := w.person(t, 0)
		_, stranger := w.person(t, 0)
		published, err := admin.PostNotificationsPublish(t.Context(), publication(owner.ID))
		must(t, err)
		if len(published.Items) != 1 {
			t.Fatal("receipt fixture missing")
		}
		client := a.client(stranger)
		inbox, err := client.GetNotificationsInbox(t.Context(), ck.GetNotificationsInboxQuery{})
		must(t, err)
		if len(inbox.Items) != 0 {
			t.Fatal("another account received private notification data")
		}
		_, err = client.PutNotificationRead(t.Context(), published.Items[0].ID)
		requireError(t, err, 404, "not_found")
		_, err = client.PutNotificationDismiss(t.Context(), published.Items[0].ID)
		requireError(t, err, 404, "not_found")
	})
	t.Run("CK-J25-S03_read_and_archive_do_not_resolve_source_work", func(t *testing.T) {
		user, token := w.person(t, 0)
		published, err := admin.PostNotificationsPublish(t.Context(), publication(user.ID))
		must(t, err)
		if len(published.Items) != 1 {
			t.Fatal("receipt fixture missing")
		}
		id, client := published.Items[0].ID, a.client(token)
		read, err := client.PutNotificationRead(t.Context(), id)
		must(t, err)
		if read.ReadAt == nil || read.Status != "open" || read.ResolvedAt != nil {
			t.Fatal("reading resolved source work")
		}
		dismissed, err := client.PutNotificationDismiss(t.Context(), id)
		must(t, err)
		if dismissed.DismissedAt == nil || dismissed.Status != "open" || dismissed.ResolvedAt != nil {
			t.Fatal("archiving resolved source work")
		}
		active, err := client.GetNotificationsInbox(t.Context(), ck.GetNotificationsInboxQuery{})
		must(t, err)
		if len(active.Items) != 0 {
			t.Fatal("archived receipt remained in the active inbox")
		}
		archivedOnly := true
		archived, err := client.GetNotificationsInbox(t.Context(), ck.GetNotificationsInboxQuery{Dismissed: &archivedOnly})
		must(t, err)
		if len(archived.Items) != 1 || archived.Items[0].ID != id {
			t.Fatal("archived receipt disappeared")
		}
		_, err = client.DeleteNotificationDismiss(t.Context(), id)
		must(t, err)
		unread, err := client.DeleteNotificationRead(t.Context(), id)
		must(t, err)
		if unread.ReadAt != nil || unread.DismissedAt != nil || unread.Status != "open" || unread.ResolvedAt != nil {
			t.Fatal("restore/unread changed the source workflow")
		}
	})
	t.Run("CK-J25-S04_foreign_or_unknown_recipient_is_refused", func(t *testing.T) {
		foreign, _ := w.person(t, 1)
		_, err := admin.PostNotificationsPublish(t.Context(), publication(foreign.ID))
		requireError(t, err, 400, "invalid_request")
		_, err = admin.PostNotificationsPublish(t.Context(), publication(uuid.NewString()))
		requireError(t, err, 400, "invalid_request")
	})
	t.Run("CK-J25-S05_erased_recipient_cannot_be_recreated_by_delayed_publish", func(t *testing.T) {
		user, token := w.person(t, 0)
		in := publication(user.ID)
		_, err := admin.PostNotificationsPublish(t.Context(), in)
		must(t, err)
		must(t, a.engine.AnonymizeAccount(t.Context(), user.ID))
		for _, delayed := range []notification.Publication{in, publication(user.ID)} {
			got, err := admin.PostNotificationsPublish(t.Context(), delayed)
			must(t, err)
			if len(got.Items) != 0 {
				t.Fatal("erased recipient acquired a delayed receipt")
			}
		}
		inbox, err := a.client(token).GetNotificationsInbox(t.Context(), ck.GetNotificationsInboxQuery{})
		must(t, err)
		if len(inbox.Items) != 0 {
			t.Fatal("erased inbox became visible again")
		}
	})
}
