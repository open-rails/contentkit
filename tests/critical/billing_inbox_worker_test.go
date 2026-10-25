package critical_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/open-rails/authkit/authtest"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/helpers/notify"
	riverhelpers "github.com/open-rails/helpers/river"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/openrailstest/stripemock"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

// Produce the worker's mandatory access-loss notice through real enrollment
// and revocation. The fixture's preexisting one-off notice remains an explicit
// optional-event control and must not appear through the default worker policy.
func workerAccessLoss(t *testing.T, i *billingInboxWorld) (billing.Event, billing.CustomerID, billing.SubscriptionID) {
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

type inboxWorkerReplay struct{ body json.RawMessage }

func (inboxWorkerReplay) Kind() string                   { return "contentkit_openrails_inbox_sync" }
func (r inboxWorkerReplay) MarshalJSON() ([]byte, error) { return r.body, nil }

func TestBillingInboxRegisteredWorkers(t *testing.T) {
	for _, wrapper := range []string{"OpenRailsNotifications", "OpenRailsNotificationsContext"} {
		t.Run("CK-J25-S06_"+wrapper+"_registered_worker_delivers_real_access_loss", func(t *testing.T) {
			i := newBillingInboxWorld(t)
			expected, customer, subscription := workerAccessLoss(t, i)
			// Discard the helper's unstarted attachment so each wrapper must wire
			// a fresh engine's actual returned contribution on its own.
			i.a.server.Close()
			must(t, i.a.engine.Close(t.Context()))
			var err error
			i.a.engine, err = ck.New(t.Context(), i.a.cfg, i.a.deps)
			must(t, err)
			i.a.server = mountEngine(t, i.a.engine)
			var attachment riverhelpers.Contribution
			if wrapper == "OpenRailsNotifications" {
				attachment, err = i.a.engine.OpenRailsNotifications(i.source, i.mapper)
			} else {
				startup, cancel := context.WithCancel(t.Context())
				attachment, err = i.a.engine.OpenRailsNotificationsContext(startup, i.source, i.mapper)
				cancel() // a completed startup call must not own the worker's lifetime
			}
			must(t, err)
			cursor, err := i.store.EventCursor(t.Context(), i.source.Origin())
			must(t, err)
			if cursor != "" || len(i.receipts(t)) != 0 {
				t.Fatal("attachment consumed facts before its worker ran")
			}
			fleet, err := riverhelpers.New(t.Context(), i.w.pool, &river.Config{Schema: i.w.schema + "_river", Queues: map[string]river.QueueConfig{}}, i.a.engine.RiverJobs(), attachment)
			must(t, err)
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()
				if err := fleet.StopAndCancel(ctx); err != nil {
					t.Errorf("stop owned inbox fleet: %v", err)
				}
			})
			events, unsubscribe := fleet.Subscribe(river.EventKindJobCompleted, river.EventKindJobFailed)
			defer unsubscribe()
			must(t, i.a.engine.Start(t.Context(), ck.WithRiverClient(fleet)))
			must(t, fleet.Start(t.Context()))
			var first *rivertype.JobRow
			deadline := time.NewTimer(20 * time.Second)
			defer deadline.Stop()
			for first == nil {
				select {
				case event := <-events:
					if event == nil || event.Job.Kind != "contentkit_openrails_inbox_sync" {
						continue
					}
					if event.Kind != river.EventKindJobCompleted {
						t.Fatalf("actual inbox worker failed: %+v", event.Job.Errors)
					}
					first = event.Job
				case <-deadline.C:
					t.Fatal("attachment's scheduled River worker did not execute")
				}
			}
			if !strings.HasPrefix(first.Queue, "contentkit_notifications_") {
				t.Fatalf("notice ran on no source-specific contributed queue: %s", first.Queue)
			}
			cursor, err = i.store.EventCursor(t.Context(), i.source.Origin())
			must(t, err)
			if cursor != i.page.NextCursor {
				t.Fatal("registered worker did not commit the complete source-page cursor")
			}
			receipts := i.receipts(t)
			if len(receipts) != 1 {
				t.Fatalf("worker must deliver the mandatory notice and skip the optional purchase: %+v", receipts)
			}
			item := receipts[0]
			if item.ID == "" || item.Revision <= 0 || item.Kind != "premium_ended" || item.Recipient.Audience != notify.AudienceUser || item.Recipient.ID != customer.String() || item.Origin.ID != expected.ID || item.Origin.System != i.source.Origin().System || item.Origin.Scope != i.b.merchant.MerchantID().String() {
				t.Fatalf("wrong durable recipient receipt: %+v", item)
			}
			var delivered struct {
				AccessEnded    bool                   `json:"access_ended"`
				SubscriptionID billing.SubscriptionID `json:"subscription_id"`
			}
			must(t, json.Unmarshal(item.Data, &delivered))
			if !delivered.AccessEnded || delivered.SubscriptionID != subscription {
				t.Fatalf("worker changed the retained lifecycle fact: %+v", delivered)
			}
			// Redeliver the real scheduler-produced payload through the same
			// registered worker; never call Source.Consume from this contract.
			for range 2 {
				job, err := fleet.Insert(t.Context(), inboxWorkerReplay{body: first.EncodedArgs}, &river.InsertOpts{Queue: first.Queue, MaxAttempts: 1})
				must(t, err)
				waitFor(t, func() bool {
					current, err := fleet.JobGet(t.Context(), job.Job.ID)
					must(t, err)
					if current.State == rivertype.JobStateDiscarded {
						t.Fatalf("inbox replay failed: %+v", current.Errors)
					}
					return current.State == rivertype.JobStateCompleted
				})
			}
			if !reflect.DeepEqual(receipts, i.receipts(t)) {
				t.Fatal("registered worker replay duplicated or changed the recipient receipt")
			}
			cursor, err = i.store.EventCursor(t.Context(), i.source.Origin())
			must(t, err)
			if cursor != i.page.NextCursor {
				t.Fatal("worker replay changed the settled continuation")
			}
			i.requireSourceUnchanged(t)
		})
	}
}
