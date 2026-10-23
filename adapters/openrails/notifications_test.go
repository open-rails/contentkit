package openrails_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	"github.com/open-rails/authkit/userinfo"
	ckopenrails "github.com/open-rails/contentkit/adapters/openrails"
	"github.com/open-rails/contentkit/notification"
	notice "github.com/open-rails/contentkit/notification/openrails"
	"github.com/open-rails/helpers/notify"
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
	native := ""
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
			got, deliver, err := ckopenrails.SameAccounts(tc.directory)(t.Context(), notice.Event{CustomerID: tc.customer, AccountIssuer: &native})
			if (err != nil) != tc.wantError || deliver != tc.deliver || (deliver && got != id) || (!deliver && got != "") {
				t.Fatalf("mapped account = %q, %v, %v", got, deliver, err)
			}
		})
	}
	foreign := "https://foreign.test"
	for _, issuer := range []*string{nil, &foreign} {
		_, deliver, err := ckopenrails.SameAccounts(accountDirectory{users: map[string]userinfo.User{id: {ID: id}}})(t.Context(), notice.Event{CustomerID: id, AccountIssuer: issuer})
		if err != nil || deliver {
			t.Fatalf("unverified issuer received notice: %v %v", deliver, err)
		}
	}
	trusted := "https://accounts.test"
	_, deliver, err := ckopenrails.SameAccounts(accountDirectory{users: map[string]userinfo.User{id: {ID: id}}}, trusted)(t.Context(), notice.Event{CustomerID: id, AccountIssuer: &trusted})
	if err != nil || !deliver {
		t.Fatalf("verified issuer refused: %v %v", deliver, err)
	}
}

// Two site processes use the typed embedded client, the same durable source
// log, and one destination cursor. A replay never resets inbox read state.
func TestNotificationSourceSharesInboxCursorAndPreservesReadState(t *testing.T) {
	var schema string
	w := newWorld(t, func(c *openrails.Config) { schema = c.Database.Schema })
	ctx := t.Context()
	var tenant iam.TenantID
	accounts, _ := authtest.New(t,
		authtest.WithConfig(func(c *authkit.Config) { tenant = c.DefaultTenantID }),
		authtest.WithDeps(func(d *authkit.Deps) { d.Postgres = w.pool }))
	account := authtest.NewUser(t, accounts)
	bound, err := accounts.ForTenant(ctx, tenant)
	if err != nil {
		t.Fatal(err)
	}
	directory := bound.UserInfo()
	viewer := account.ID
	customer, err := billing.ParseCustomerID(viewer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.bill.UpdateCustomer(ctx, customer, billing.UpdateCustomerParams{}); err != nil {
		t.Fatal(err)
	}
	id := billing.NotificationID(uuid.New())
	_, err = w.pool.Exec(ctx, `INSERT INTO `+pgx.Identifier{schema, "notification_events"}.Sanitize()+`
		(merchant_id,id,customer_id,event_type,data) VALUES ($1,$2,$3,'payment_method_failed','{"retry_status":"stopped","recovery_action":"update_payment_method"}')`,
		w.bill.MerchantID().UUID(), id.UUID(), viewer)
	if err != nil {
		t.Fatal(err)
	}
	var recipientLookupError error
	if err := notification.Migrate(ctx, w.pool, w.schema); err != nil {
		t.Fatal(err)
	}
	destination, err := notification.New(notification.Options{Pool: w.pool, Schema: w.schema, Tenant: tenant.String(), AppID: uuid.NewString(),
		AccountExists: func(ctx context.Context, id string) (bool, error) {
			if recipientLookupError != nil {
				return false, recipientLookupError
			}
			users, err := directory.Get(ctx, []string{id})
			user, exists := users[id]
			return exists && user.ID == id, err
		},
		AccountExistsTx: func(ctx context.Context, tx pgx.Tx, id string) (bool, error) {
			if recipientLookupError != nil {
				return false, recipientLookupError
			}
			reader, ok := directory.(interface {
				GetTx(context.Context, pgx.Tx, []string) (map[string]userinfo.User, error)
			})
			if !ok {
				return false, errors.New("native directory lacks transactional lookup")
			}
			users, err := reader.GetTx(ctx, tx, []string{id})
			user, exists := users[id]
			return exists && user.ID == id, err
		}})
	if err != nil {
		t.Fatal(err)
	}
	// A caller-owned transaction uses the same membership
	// check without borrowing another connection, and rolls back as one unit.
	tx, err := w.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := destination.PublishTx(ctx, tx, notify.Input{Recipient: notify.Recipient{Audience: notify.AudienceUser, ID: viewer}, Origin: notify.Origin{System: "openrails", Scope: w.bill.MerchantID().String(), ID: "rolled-back"}, Kind: "payment_method_failed", CreatedAt: time.Now(), Visibility: notify.VisibilityTenant}); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	mapper := ckopenrails.SameAccounts(directory)
	failedSource, err := ckopenrails.NewNotificationSource(w.bill, "openrails")
	if err != nil {
		t.Fatal(err)
	}
	recipientLookupError = errors.New("temporary recipient lookup outage")
	failedResult, err := failedSource.Consume(ctx, destination, mapper, notice.ConsumeOptions{Filter: notice.BillingLifecycle})
	if !errors.Is(err, recipientLookupError) || failedResult.Delivered != 0 {
		t.Fatalf("lookup failure committed a page: %+v %v", failedResult, err)
	}
	recipientLookupError = nil
	for i := range 3 {
		if i == 2 {
			_, err := w.pool.Exec(ctx, `UPDATE `+pgx.Identifier{w.schema, "notification_event_cursors"}.Sanitize()+` SET cursor='' WHERE namespace=$1`, tenant.String())
			if err != nil {
				t.Fatal(err)
			}
		}
		source, err := ckopenrails.NewNotificationSource(w.bill, "openrails")
		if err != nil {
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
		if result.Delivered != want {
			t.Fatalf("reader %d delivered %+v, want %d", i, result, want)
		}
		page, err := destination.List(ctx, notify.Recipient{Audience: notify.AudienceUser, ID: viewer}, notify.ListOptions{})
		if err != nil || len(page.Items) != 1 {
			t.Fatalf("destination after consumer %d = %+v, %v", i, page, err)
		}
		item := page.Items[0]
		if item.Origin.ID != id.String() || item.Origin.Scope != w.bill.MerchantID().String() || item.Origin.Subject != viewer {
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
