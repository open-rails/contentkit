package critical_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/authkit/authtest"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/access"
	ckbilling "github.com/open-rails/contentkit/adapters/openrails"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/openrails"
	openrailshttp "github.com/open-rails/openrails/adapters/http"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/openrailstest/stripemock"
)

// realBilling wires the real commerce authority into ContentKit. Only the
// external Stripe and FX protocols are local fixtures; no billing service,
// product repository, entitlement reader or application auth is replaced.
type realBilling struct {
	engine   *openrails.Engine
	merchant *openrails.Client
	clock    *clockwork.FakeClock
	stripe   *stripemock.Mock
	server   *httptest.Server
}

func attachBilling(t *testing.T, w *world, a *application, configure ...func(*openrails.Deps)) *realBilling {
	t.Helper()
	b := &realBilling{clock: clockwork.NewFakeClockAt(w.clock.now())}
	b.stripe = stripemock.New(stripemock.Options{Clock: b.clock.Now})
	t.Cleanup(b.stripe.Close)
	schema := "ckb_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:20]
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_, err := w.pool.Exec(ctx, "DROP SCHEMA IF EXISTS "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		must(t, err)
	})
	cfg := openrails.Config{Database: openrails.DatabaseConfig{Schema: schema, RiverSchema: schema},
		ProviderWriteMode: openrails.ProviderWritesFull, CatalogReconciliationInterval: "0", ReturnOrigins: []string{"https://critical.invalid"},
		Merchants: map[string]openrails.MerchantDeclaration{"reader": {DisplayName: "Reader", Issuer: authtest.Issuer, PSPs: map[string]openrails.PSPConfig{
			"stripe": openrails.StripePSP{AccountID: "acct_ckcritical", Environment: openrails.EnvironmentTest, SecretKey: "sk_test_critical", WebhookSigningSecret: "whsec_critical", PublishableKey: "pk_test_critical"}.PSPConfig(),
		}}}}
	merchant := cfg.Merchants["reader"]
	merchant.CardVault = "stripe"
	cfg.Merchants["reader"] = merchant
	var err error
	deps := openrails.Deps{Postgres: w.pool, Clock: b.clock, FXTransport: fixedFXWire(t), StripeTransport: b.stripe.Transport(), UserInfo: w.tenant[0].UserInfo()}
	for _, apply := range configure {
		apply(&deps)
	}
	b.engine, err = openrails.New(t.Context(), cfg, deps)
	must(t, err)
	t.Cleanup(func() { must(t, b.engine.Close(context.Background())) })
	b.merchant, err = b.engine.MerchantBySlug("reader")
	must(t, err)
	mux := http.NewServeMux()
	must(t, openrailshttp.Mount(mux, b.engine, openrails.Routes{Prefix: "/billing", Auth: w.auth.Authenticator()}))
	b.server = httptest.NewServer(mux)
	t.Cleanup(b.server.Close)
	// Replace this fixture's HTTP server and engine only; the database and
	// real identity directory remain the same borrowed dependencies.
	a.server.Close()
	must(t, a.engine.Close(t.Context()))
	a.deps.Billing = ckbilling.Billing(b.merchant)
	a.cfg.Database.SkipMigrations = true
	a.engine, err = ck.New(t.Context(), a.cfg, a.deps)
	must(t, err)
	routes, err := a.engine.Routes(ck.Routes{Prefix: "/content", Groups: a.engine.RouteGroups()})
	must(t, err)
	mux = http.NewServeMux()
	for _, route := range routes {
		mux.Handle(route.Path, route.Handler)
	}
	a.server = httptest.NewServer(mux)
	return b
}

type localFX struct {
	target    *url.URL
	transport http.RoundTripper
}

func (p localFX) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.URL.Scheme, r.URL.Host, r.Host = p.target.Scheme, p.target.Host, p.target.Host
	return p.transport.RoundTrip(r)
}
func fixedFXWire(t *testing.T) http.RoundTripper {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		base := strings.TrimSuffix(r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:], ".json")
		rates := map[string]int{}
		for _, currency := range billing.Currencies() {
			rates[strings.ToLower(currency.Code)] = 1
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"date": time.Now().UTC().Format(time.DateOnly), base: rates})
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	must(t, err)
	return localFX{target: u, transport: http.DefaultTransport}
}

func TestCommerceControlsContentAccess(t *testing.T) {
	w := newWorld(t)
	a := w.apps[0]
	b := attachBilling(t, w, a)
	user, credential := w.person(t, 0)
	viewer := a.client(credential)
	item, err := a.engine.Client().PostContents(t.Context(), catalog.ContentInput{Kind: "article", Access: &access.Rule{Level: access.TierLevel, Tiers: []string{"gold"}}, Text: map[string]catalog.Text{"en": {Title: "Purchased article", Body: "Purchased body"}}})
	must(t, err)
	_, err = a.engine.Client().PostContentPublish(t.Context(), item.ID, catalog.PublishInput{})
	must(t, err)
	product, err := b.merchant.CreateProduct(t.Context(), billing.CreateProductParams{Key: "gold", DisplayName: "Gold access", Entitlements: []string{"gold"}})
	must(t, err)
	t.Run("CK-J06-S03_paid_through_cancellation_preserves_access_until_expiry", func(t *testing.T) {
		locked, err := viewer.GetContent(t.Context(), item.ID, ck.GetContentQuery{})
		must(t, err)
		if locked.Decision.Allowed || locked.Body != "" {
			t.Fatal("unpurchased content was already open")
		}
		hours := 24
		price, err := b.merchant.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: "gold-day", UnitAmount: 10_000_000, Currency: "USD", BillingIntervalHours: &hours, AccessDurationHours: &hours})
		must(t, err)
		total, reusable := int64(10_000_000), true
		paymentMethod := b.stripe.NewPaymentMethod(stripemock.Card{Brand: "visa", Last4: "4242"})
		body, err := json.Marshal(billing.CreateMyOrderParams{Lines: []billing.OrderLineParams{{PriceID: price.ID}}, ExpectedTotal: &total, Reusable: &reusable, Payment: &billing.OrderPaymentParams{Token: paymentMethod}})
		must(t, err)
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, b.server.URL+"/billing/v1/me/orders", bytes.NewReader(body))
		must(t, err)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", "ck-paid-membership")
		authtest.Authorize(t, w.tenant[0], req, credential)
		res, err := b.server.Client().Do(req)
		must(t, err)
		data, err := io.ReadAll(res.Body)
		res.Body.Close()
		must(t, err)
		if res.StatusCode != 201 {
			t.Fatalf("real customer purchase returned %d: %s", res.StatusCode, data)
		}
		var order billing.Order
		must(t, json.Unmarshal(data, &order))
		if order.Status != billing.OrderComplete || len(order.Lines) != 1 || order.Lines[0].SubscriptionID == nil || order.Payment.PaymentID == nil {
			t.Fatalf("purchase did not settle an actual subscription: %+v", order)
		}
		payment, err := b.merchant.GetPayment(t.Context(), *order.Payment.PaymentID)
		must(t, err)
		if payment.Status != billing.PaymentSucceeded || payment.Amount != 10_000_000 {
			t.Fatalf("unexpected settled payment: %+v", payment)
		}
		sub, err := b.merchant.CancelSubscription(t.Context(), *order.Lines[0].SubscriptionID, billing.CancelSubscriptionParams{Reason: "Stop renewal; keep paid access", RevokeAccess: false})
		must(t, err)
		if sub.CurrentPeriodEndsAt == nil {
			t.Fatal("cancel lost the purchased access deadline")
		}
		for _, sample := range []struct {
			name    string
			at      time.Time
			allowed bool
		}{
			{"paid period", b.clock.Now(), true}, {"before expiry", sub.CurrentPeriodEndsAt.Add(-time.Microsecond), true}, {"at expiry", *sub.CurrentPeriodEndsAt, false},
		} {
			t.Run(sample.name, func(t *testing.T) {
				b.clock.Advance(sample.at.Sub(b.clock.Now()))
				w.clock.set(sample.at)
				got, err := viewer.GetContent(t.Context(), item.ID, ck.GetContentQuery{})
				must(t, err)
				if got.Decision.Allowed != sample.allowed || (got.Body != "") != sample.allowed {
					t.Fatalf("content access at %s = %+v", sample.name, got.Decision)
				}
			})
		}
		if len(b.stripe.Unexpected()) != 0 {
			t.Fatalf("unmodeled external Stripe requests: %v", b.stripe.Unexpected())
		}
		_ = user // the access is derived from this actual AuthKit account's credential.
	})
}
