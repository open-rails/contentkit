package openrails_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/authkit/userinfo"
	ckopenrails "github.com/open-rails/contentkit/adapters/openrails"
	notice "github.com/open-rails/contentkit/notification/openrails"
	"github.com/open-rails/helpers/notify"
	"github.com/open-rails/helpers/notify/postgres"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
)

type accountDirectory struct {
	users map[string]userinfo.User
	err   error
}

func (d accountDirectory) Get(context.Context, []string) (map[string]userinfo.User, error) {
	return d.users, d.err
}
func (accountDirectory) Search(context.Context, string, int) ([]userinfo.User, error) {
	return nil, errors.New("notification mapping must never search by name or email")
}

func TestSameAccountsVerifiesTheCurrentSubject(t *testing.T) {
	id, other := uuid.NewString(), uuid.NewString()
	for _, tc := range []struct {
		name      string
		directory userinfo.Lookup
		customer  string
		deliver   bool
		wantError bool
	}{
		{"current account", accountDirectory{users: map[string]userinfo.User{id: {ID: id}}}, id, true, false},
		{"deleted account", accountDirectory{}, id, false, false},
		{"matching email is insufficient", accountDirectory{users: map[string]userinfo.User{other: {ID: other, Email: "same@example.test"}}}, id, false, false},
		{"wrong subject under requested key", accountDirectory{users: map[string]userinfo.User{id: {ID: other}}}, id, false, true},
		{"directory outage stays retryable", accountDirectory{err: errors.New("offline")}, id, false, true},
		{"no directory stays retryable", nil, id, false, true},
		{"invalid customer stays retryable", accountDirectory{}, "wrong", false, true},
		{"zero customer stays retryable", accountDirectory{}, uuid.Nil.String(), false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, deliver, err := ckopenrails.SameAccounts(tc.directory)(t.Context(), notice.Event{CustomerID: tc.customer})
			if (err != nil) != tc.wantError || deliver != tc.deliver || (deliver && got != id) || (!deliver && got != "") {
				t.Fatalf("mapped account = %q, %v, %v", got, deliver, err)
			}
		})
	}
}

// Two site processes use the typed embedded client, the same durable source
// consumer, and one destination receipt. The second process observes the ACK;
// an independent consumer can replay the fact without resetting read state.
func TestNotificationSourceSharesConsumerAndPreservesReadState(t *testing.T) {
	var schema string
	w := newWorld(t, func(c *openrails.Config) { schema = c.Database.Schema })
	ctx := t.Context()
	viewer := w.customer()
	id := billing.NotificationID(uuid.New())
	_, err := w.pool.Exec(ctx, `INSERT INTO `+pgx.Identifier{schema, "notification_events"}.Sanitize()+`
		(merchant_id,id,customer_id,event_type,data) VALUES ($1,$2,$3,'payment_method_failed','{"retry_status":"stopped","recovery_action":"update_payment_method"}')`,
		w.bill.MerchantID().UUID(), id.UUID(), viewer.ID)
	if err != nil {
		t.Fatal(err)
	}
	destination, err := postgres.New(w.pool, postgres.Config{Schema: w.schema, Scope: "contentkit:shared-accounts"})
	if err != nil {
		t.Fatal(err)
	}
	mapper := ckopenrails.SameAccounts(accountDirectory{users: map[string]userinfo.User{viewer.ID: {ID: viewer.ID}}})
	for i, consumer := range []string{"shared-accounts", "shared-accounts", "independent-replay"} {
		source, err := ckopenrails.NewNotificationSource(w.bill, "openrails", consumer)
		if err != nil {
			t.Fatal(err)
		}
		if err := source.Register(ctx); err != nil {
			t.Fatal(err)
		}
		result, err := source.Consume(ctx, destination, mapper, notice.ConsumeOptions{Filter: notice.BillingLifecycle})
		if err != nil {
			t.Fatal(err)
		}
		want := 1
		if i == 1 {
			want = 0
		}
		if result.Fetched != want || result.Delivered != want || result.Acknowledged != want {
			t.Fatalf("consumer %d delivered %+v, want %d", i, result, want)
		}
		page, err := destination.List(ctx, notify.Recipient{Audience: notify.AudienceUser, ID: viewer.ID}, notify.ListOptions{})
		if err != nil || len(page.Items) != 1 {
			t.Fatalf("destination after consumer %d = %+v, %v", i, page, err)
		}
		item := page.Items[0]
		if item.Origin.ID != id.String() || item.Origin.Scope != w.bill.MerchantID().String() || item.Origin.Subject != viewer.ID {
			t.Fatalf("typed source lost provenance: %+v", item.Origin)
		}
		if i == 0 {
			if _, err := destination.SetRead(ctx, item.Recipient, item.ID, true); err != nil {
				t.Fatal(err)
			}
		} else if item.ReadAt == nil {
			t.Fatal("another site or replay reset the shared receipt's read state")
		}
	}
}
