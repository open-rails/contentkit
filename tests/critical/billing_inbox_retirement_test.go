package critical_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/notify"
	notice "github.com/open-rails/contentkit/notification/openrails"
	riverhelpers "github.com/open-rails/helpers/river"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/openrailstest/stripemock"
	"github.com/riverqueue/river"
)

// Produce a retained access-loss notice through real enrollment
// and revocation. The fixture's preexisting one-off notice remains an explicit
// optional-event control and must not appear through the handoff's selection.
func legacyAccessLoss(t *testing.T, i *billingInboxWorld) (billing.Event, billing.CustomerID, billing.SubscriptionID) {
	t.Helper()
	product, err := i.b.merchant.CreateProduct(t.Context(), billing.CreateProductParams{Key: "worker-membership", DisplayName: "Worker membership", Entitlements: []string{"worker:membership"}})
	must(t, err)
	hours := 24
	price, err := i.b.merchant.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: "daily", UnitAmount: 10_000_000, Currency: "USD", BillingIntervalHours: &hours, AccessDurationHours: &hours})
	must(t, err)
	total, reusable := int64(10_000_000), true
	input := billing.CreateMyOrderParams{Lines: []billing.OrderLineParams{{PriceID: price.ID}}, ExpectedTotal: &total, Reusable: &reusable, Payment: &billing.OrderPaymentParams{Token: i.b.stripe.NewPaymentMethod(stripemock.Card{Brand: "visa", Last4: "4242"})}}
	body, err := json.Marshal(input)
	must(t, err)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, i.b.server.URL+"/billing/v1/me/orders", bytes.NewReader(body))
	must(t, err)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "worker-mandatory-membership")
	authtest.Authorize(t, i.w.tenant[0], request, i.token)
	response, err := i.b.server.Client().Do(request)
	must(t, err)
	raw, err := io.ReadAll(response.Body)
	response.Body.Close()
	must(t, err)
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("real recurring purchase: HTTP%d %s", response.StatusCode, raw)
	}
	var order billing.Order
	must(t, json.Unmarshal(raw, &order))
	if order.Status != billing.OrderComplete || len(order.Lines) != 1 || order.Lines[0].SubscriptionID == nil {
		t.Fatalf("purchase did not enroll a settled subscription: %+v", order)
	}
	subscription := *order.Lines[0].SubscriptionID
	before, err := i.b.merchant.CheckEntitlements(t.Context(), billing.CheckEntitlementsParams{CustomerID: order.CustomerID, Entitlements: []string{"worker:membership"}})
	must(t, err)
	if !before.Entitlements["worker:membership"] {
		t.Fatal("source never granted the membership whose loss is notified")
	}
	i.b.clock.Advance(time.Second)
	canceled, err := i.b.merchant.CancelSubscription(t.Context(), subscription, billing.CancelSubscriptionParams{Reason: "Revoke access for worker delivery contract", RevokeAccess: true})
	must(t, err)
	if canceled.Status != billing.SubscriptionCanceled {
		t.Fatalf("source cancellation did not commit: %+v", canceled)
	}
	after, err := i.b.merchant.CheckEntitlements(t.Context(), billing.CheckEntitlementsParams{CustomerID: order.CustomerID, Entitlements: []string{"worker:membership"}})
	must(t, err)
	if after.Entitlements["worker:membership"] {
		t.Fatal("source still grants access after explicit revocation")
	}
	i.page, err = i.b.merchant.ListEvents(t.Context(), billing.EventQuery{Limit: 100})
	must(t, err)
	var selected []billing.Event
	optional := 0
	for _, event := range i.page.Events {
		if event.Source == "notification" && event.Type == "one_off_purchase_completed" {
			optional++
		}
		if event.Source != "notification" || event.Type != "premium_ended" {
			continue
		}
		var data struct {
			AccessEnded    bool                   `json:"access_ended"`
			SubscriptionID billing.SubscriptionID `json:"subscription_id"`
		}
		must(t, json.Unmarshal(event.Data, &data))
		if data.AccessEnded && data.SubscriptionID == subscription {
			selected = append(selected, event)
		}
	}
	if optional != 1 || len(selected) != 1 {
		t.Fatalf("real source must contain one optional control and one actual access-loss notice: optional=%d mandatory=%d", optional, len(selected))
	}
	return selected[0], order.CustomerID, subscription
}

type retiredInboxArgs struct{}

func (retiredInboxArgs) Kind() string { return "contentkit_openrails_inbox_sync" }

func TestBillingInboxRetirement(t *testing.T) {
	t.Run("CK-J25-S08_exact_handoff_retirement_and_no_runtime_worker", func(t *testing.T) {
		i := newBillingInboxWorld(t)
		expected, customer, subscription := legacyAccessLoss(t, i)
		ctx := t.Context()
		config := i.w.pool.Config()
		config.MaxConns = 1
		pool, err := pgxpool.NewWithConfig(ctx, config)
		must(t, err)
		defer pool.Close()
		destination := i.newStore(t, pool)
		// Engine construction already migrates its own River schema. Use a
		// separate owned schema so the first case truly has no job table.
		schema := i.w.schema + "_legacy_river"
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			_, err := i.w.pool.Exec(ctx, "DROP SCHEMA IF EXISTS "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
			must(t, err)
		})
		var absent bool
		must(t, pool.QueryRow(ctx, `SELECT to_regclass($1) IS NULL`, pgx.Identifier{schema, "river_job"}.Sanitize()).Scan(&absent))
		if !absent {
			t.Fatal("missing-table fixture already contains a River job table")
		}
		key := fmt.Sprintf("%x", sha256.Sum256([]byte(destination.Namespace()+"\x00critical-billing\x00"+i.b.merchant.MerchantID().String())))
		retire := func() error { return i.source.RetireInboxSync(ctx, pool, schema, destination.Namespace(), destination) }
		// Neither a missing River table nor a gap between periodic ticks is
		// evidence that the final source page was handed off.
		for _, state := range []string{"no_table", "between_ticks", "discarded"} {
			if state == "between_ticks" {
				must(t, riverhelpers.ApplyMigrations(ctx, pool, schema))
			}
			if state == "discarded" {
				_, err := pool.Exec(ctx, `INSERT INTO `+pgx.Identifier{schema, "river_job"}.Sanitize()+`
                    (kind,args,queue,max_attempts,state,finalized_at) VALUES
                    ('contentkit_openrails_inbox_sync',jsonb_build_object('key',$1::text),'legacy',1,'discarded',clock_timestamp())`, key)
				must(t, err)
			}
			if err := retire(); !errors.Is(err, notice.ErrInboxHandoffRequired) {
				t.Fatalf("%s skipped source handoff: %v", state, err)
			}
		}
		table := pgx.Identifier{schema, "river_job"}.Sanitize()
		_, err = pool.Exec(ctx, `INSERT INTO `+table+` (kind,args,queue,max_attempts,state,finalized_at) VALUES
            ('contentkit_openrails_inbox_sync',jsonb_build_object('key',$1::text),'legacy',1,'available',NULL),
            ('contentkit_openrails_inbox_sync','{"key":"other-source"}','legacy',1,'available',NULL),
            ('host_custom_event_reader',jsonb_build_object('key',$1::text),'legacy',1,'available',NULL),
            ('contentkit_openrails_inbox_sync',jsonb_build_object('key',$1::text),'legacy',1,'completed',clock_timestamp())`, key)
		must(t, err)
		result, err := i.source.Consume(ctx, destination, i.mapper, notice.ConsumeOptions{Limit: 100, Filter: notice.BillingLifecycle})
		must(t, err)
		if result.Delivered != 1 || result.HasMore {
			t.Fatalf("explicit final handoff: %+v", result)
		}
		before := i.receipts(t)
		if len(before) != 1 || before[0].Kind != "premium_ended" {
			t.Fatalf("wrong selected legacy receipt: %+v", before)
		}
		item := before[0]
		if item.ID == "" || item.Revision <= 0 || item.Recipient.Audience != notify.AudienceUser || item.Recipient.ID != customer.String() || item.Origin.ID != expected.ID || item.Origin.System != i.source.Origin().System || item.Origin.Scope != i.b.merchant.MerchantID().String() {
			t.Fatalf("handoff changed the exact source or recipient identity: %+v", item)
		}
		var delivered billing.NotificationData
		must(t, json.Unmarshal(item.Data, &delivered))
		if !delivered.AccessEnded || delivered.SubscriptionID != subscription {
			t.Fatalf("handoff changed the retained lifecycle fact: %+v", delivered)
		}
		_, err = pool.Exec(ctx, `UPDATE `+table+` SET state='running' WHERE kind='contentkit_openrails_inbox_sync' AND args->>'key'=$1 AND state='available'`, key)
		must(t, err)
		if err := retire(); err == nil {
			t.Fatal("retired a running legacy worker")
		}
		_, err = pool.Exec(ctx, `UPDATE `+table+` SET state='available' WHERE kind='contentkit_openrails_inbox_sync' AND args->>'key'=$1 AND state='running'`, key)
		must(t, err)
		must(t, retire())
		must(t, retire())
		var cancelled, completed, available, discarded int
		must(t, pool.QueryRow(ctx, `SELECT count(*) FILTER(WHERE state='cancelled'),count(*) FILTER(WHERE state='completed'),count(*) FILTER(WHERE state='available'),count(*) FILTER(WHERE state='discarded') FROM `+table).Scan(&cancelled, &completed, &available, &discarded))
		if cancelled != 1 || completed != 1 || available != 2 || discarded != 1 {
			t.Fatalf("retirement touched another source or history: %d %d %d %d", cancelled, completed, available, discarded)
		}
		if !reflect.DeepEqual(before, i.receipts(t)) {
			t.Fatal("retirement changed adopted receipts")
		}
		i.requireSourceUnchanged(t)
		fleet, err := riverhelpers.New(ctx, i.w.pool, &river.Config{Schema: i.w.schema + "_river", Queues: map[string]river.QueueConfig{}}, i.a.engine.RiverJobs())
		must(t, err)
		_, err = fleet.Insert(ctx, retiredInboxArgs{}, nil)
		var unknown *river.UnknownJobKindError
		if !errors.As(err, &unknown) || unknown.Kind != (retiredInboxArgs{}).Kind() {
			t.Fatalf("retired poller remains registered: %v", err)
		}
	})
}
