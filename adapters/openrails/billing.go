// Package openrails adapts OpenRails to ContentKit: it is the one place that
// imports both. Billing implements access.Billing over the OpenRails Go
// client, embedded (Engine.MerchantBySlug) or remote alike.
package openrails

import (
	"context"
	"fmt"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"

	"github.com/open-rails/contentkit/access"
)

// Client is the OpenRails methods the adapter needs; *openrails.Client
// implements it.
type Client interface {
	CheckEntitlements(context.Context, billing.CheckEntitlementsParams, ...openrails.RequestOption) (*billing.EntitlementCheck, error)
	ListProducts(context.Context, billing.ProductListParams, ...openrails.RequestOption) (*billing.ListPage[billing.Product], error)
	CreateProduct(context.Context, billing.CreateProductParams, ...openrails.RequestOption) (*billing.Product, error)
	UpdateProduct(context.Context, billing.ProductID, billing.UpdateProductParams, ...openrails.RequestOption) (*billing.Product, error)
	CreatePrice(context.Context, billing.CreatePriceParams, ...openrails.RequestOption) (*billing.Price, error)
	UpdatePrice(context.Context, billing.PriceID, billing.UpdatePriceParams, ...openrails.RequestOption) (*billing.Price, error)
	CreateProductAccess(context.Context, billing.CreateProductAccessBatchParams, ...openrails.RequestOption) ([]billing.ProductAccessGrant, error)
	ListProductAccess(context.Context, billing.ProductAccessListParams, ...openrails.RequestOption) (*billing.ListPage[billing.ProductAccessGrant], error)
	RevokeProductAccess(context.Context, billing.ProductAccessID, billing.RevokeProductAccessParams, ...openrails.RequestOption) error
	ListPrices(context.Context, billing.PriceListParams, ...openrails.RequestOption) (*billing.ListPage[billing.Price], error)
	ListSubscriptions(context.Context, billing.SubscriptionListParams, ...openrails.RequestOption) (*billing.ListPage[billing.Subscription], error)
	CancelSubscription(context.Context, billing.SubscriptionID, billing.CancelSubscriptionParams, ...openrails.RequestOption) (*billing.Subscription, error)
}

var _ Client = (*openrails.Client)(nil)

// Billing is access.Billing over c. Held is one CheckEntitlements: exact keys
// are chunked by billing.MaxEntitlementChecks (each further chunk is one more
// call) and the prefixes ride the first chunk.
func Billing(c Client) access.Billing { return adapter{c} }

type adapter struct{ c Client }

func (a adapter) Held(ctx context.Context, subject string, q access.Query) (access.Answer, error) {
	ans := access.Answer{Keys: make(map[string]bool, len(q.Keys)), Held: make(map[string]access.HeldKeys, len(q.Prefixes))}
	customer, err := billing.ParseCustomerID(subject)
	if err != nil {
		for _, p := range q.Prefixes {
			ans.Held[p] = access.HeldKeys{}
		}
		return ans, nil
	}
	limit := q.Limit
	if limit == 0 {
		limit = access.HeldLimit
	}
	keys := q.Keys
	for first := true; first || len(keys) > 0; first = false {
		n := min(len(keys), billing.MaxEntitlementChecks)
		params := billing.CheckEntitlementsParams{CustomerID: customer, Entitlements: keys[:n]}
		if first {
			params.Prefixes, params.PrefixLimit = q.Prefixes, limit
		}
		keys = keys[n:]
		if len(params.Entitlements) == 0 && len(params.Prefixes) == 0 {
			break
		}
		res, err := a.c.CheckEntitlements(ctx, params)
		if err != nil {
			return access.Answer{}, err
		}
		for _, k := range params.Entitlements {
			ans.Keys[k] = res.Entitlements[k]
		}
		for _, p := range params.Prefixes {
			if h, ok := res.Held[p]; ok {
				ans.Held[p] = access.HeldKeys{Keys: h.Keys, Truncated: h.Truncated}
			}
		}
	}
	return ans, nil
}

func (a adapter) Products(ctx context.Context, keys []string) (map[string]access.Product, error) {
	out := make(map[string]access.Product, len(keys))
	for len(keys) > 0 {
		n := min(len(keys), billing.MaxBatchItems)
		page := billing.ProductListParams{Keys: keys[:n], PageRequest: billing.PageRequest{Limit: billing.MaxPageLimit}}
		keys = keys[n:]
		for {
			res, err := a.c.ListProducts(ctx, page)
			if err != nil {
				return nil, err
			}
			for _, p := range res.Items {
				out[p.Key] = product(p)
			}
			if res.Next == "" {
				break
			}
			page.Cursor = res.Next
		}
	}
	return out, nil
}

func (a adapter) CreateProducts(ctx context.Context, products []access.NewProduct) ([]access.Product, error) {
	out := make([]access.Product, 0, len(products))
	for _, p := range products {
		res, err := a.c.CreateProduct(ctx, billing.CreateProductParams{Key: p.Key, DisplayName: p.DisplayName, Entitlements: p.Entitlements,
			RequiredEntitlements: p.RequiredEntitlements})
		if err != nil {
			return nil, fmt.Errorf("create product %s: %w", p.Key, err)
		}
		out = append(out, product(*res))
	}
	return out, nil
}

func (a adapter) UpdateProducts(ctx context.Context, updates []access.ProductUpdate) ([]access.Product, error) {
	out := make([]access.Product, 0, len(updates))
	for _, u := range updates {
		id, err := billing.ParseProductID(u.ID)
		if err != nil {
			return nil, err
		}
		var params billing.UpdateProductParams
		if u.DisplayName != nil {
			params.DisplayName = catalog.Value(*u.DisplayName)
		}
		if u.Archived != nil {
			params.Archived = catalog.Value(*u.Archived)
		}
		if u.RequiredEntitlements != nil {
			params.RequiredEntitlements = catalog.Value(append([]string{}, *u.RequiredEntitlements...))
		}
		res, err := a.c.UpdateProduct(ctx, id, params)
		if err != nil {
			return nil, fmt.Errorf("update product %s: %w", u.ID, err)
		}
		out = append(out, product(*res))
	}
	return out, nil
}

func (a adapter) CreatePrices(ctx context.Context, prices []access.NewPrice) ([]access.Price, error) {
	out := make([]access.Price, 0, len(prices))
	for _, p := range prices {
		id, err := billing.ParseProductID(p.ProductID)
		if err != nil {
			return nil, err
		}
		res, err := a.c.CreatePrice(ctx, billing.CreatePriceParams{ProductID: id, Key: p.Key, UnitAmount: p.Amount, Currency: p.Currency,
			AccessDurationHours: p.AccessHours, BillingIntervalHours: p.IntervalHours, PSPs: p.PSPs})
		if err != nil {
			return nil, fmt.Errorf("create price %s on %s: %w", p.Key, p.ProductID, err)
		}
		out = append(out, price(*res))
	}
	return out, nil
}

func (a adapter) ArchivePrices(ctx context.Context, ids []string) error {
	for _, s := range ids {
		id, err := billing.ParsePriceID(s)
		if err != nil {
			return err
		}
		if _, err := a.c.UpdatePrice(ctx, id, billing.UpdatePriceParams{Archived: catalog.Value(true)}); err != nil {
			return fmt.Errorf("archive price %s: %w", s, err)
		}
	}
	return nil
}

func (a adapter) Grant(ctx context.Context, grants []access.Grant) error {
	items := make([]billing.CreateProductAccessParams, 0, len(grants))
	for _, g := range grants {
		customer, product, err := ids(g)
		if err != nil {
			return err
		}
		items = append(items, billing.CreateProductAccessParams{CustomerID: customer, ProductID: product, Reason: billing.GrantReasonComp})
	}
	for len(items) > 0 {
		n := min(len(items), billing.MaxBatchItems)
		if _, err := a.c.CreateProductAccess(ctx, billing.CreateProductAccessBatchParams{Items: items[:n]}); err != nil {
			return err
		}
		items = items[n:]
	}
	return nil
}

func (a adapter) Revoke(ctx context.Context, grants []access.Grant) error {
	for _, g := range grants {
		customer, product, err := ids(g)
		if err != nil {
			return err
		}
		page := billing.ProductAccessListParams{CustomerIDs: []billing.CustomerID{customer}, ProductIDs: []billing.ProductID{product},
			LiveOnly: true, PageRequest: billing.PageRequest{Limit: billing.MaxPageLimit}}
		for {
			res, err := a.c.ListProductAccess(ctx, page)
			if err != nil {
				return err
			}
			for _, w := range res.Items {
				if w.SourceType != billing.ProductAccessSourceGrant {
					continue
				}
				if err := a.c.RevokeProductAccess(ctx, w.ID, billing.RevokeProductAccessParams{Reason: "revoked by ContentKit"}); err != nil {
					return err
				}
			}
			if res.Next == "" {
				break
			}
			page.Cursor = res.Next
		}
	}
	return nil
}

// StopRenewals cancels, at the end of its paid period, every live
// subscription to a recurring price of the products, archived prices
// included.
func (a adapter) StopRenewals(ctx context.Context, productIDs []string) error {
	for _, s := range productIDs {
		product, err := billing.ParseProductID(s)
		if err != nil {
			return err
		}
		recurring := true
		prices := billing.PriceListParams{ProductID: product, Recurring: &recurring, PageRequest: billing.PageRequest{Limit: billing.MaxPageLimit}}
		for {
			res, err := a.c.ListPrices(ctx, prices)
			if err != nil {
				return err
			}
			for _, p := range res.Items {
				if err := a.stopPrice(ctx, p.ID); err != nil {
					return err
				}
			}
			if res.Next == "" {
				break
			}
			prices.Cursor = res.Next
		}
	}
	return nil
}

func (a adapter) stopPrice(ctx context.Context, price billing.PriceID) error {
	page := billing.SubscriptionListParams{PriceID: price, PageRequest: billing.PageRequest{Limit: billing.MaxPageLimit}}
	for {
		res, err := a.c.ListSubscriptions(ctx, page)
		if err != nil {
			return err
		}
		for _, sub := range res.Items {
			if sub.Status == billing.SubscriptionCanceled || sub.CanceledAt != nil || sub.EndedAt != nil {
				continue
			}
			if _, err := a.c.CancelSubscription(ctx, sub.ID, billing.CancelSubscriptionParams{Reason: "the offer no longer renews"}); err != nil {
				return fmt.Errorf("stop subscription %s: %w", sub.ID, err)
			}
		}
		if res.Next == "" {
			return nil
		}
		page.Cursor = res.Next
	}
}

func ids(g access.Grant) (billing.CustomerID, billing.ProductID, error) {
	customer, err := billing.ParseCustomerID(g.Subject)
	if err != nil {
		return customer, billing.ProductID{}, fmt.Errorf("subject %q is not a customer: %w", g.Subject, err)
	}
	product, err := billing.ParseProductID(g.ProductID)
	return customer, product, err
}

func product(p billing.Product) access.Product {
	out := access.Product{ID: p.ID.String(), Key: p.Key, DisplayName: p.DisplayName, Entitlements: p.Entitlements,
		RequiredEntitlements: p.RequiredEntitlements, Archived: p.Archived}
	for _, pr := range p.Prices {
		if !pr.Archived {
			out.Prices = append(out.Prices, price(pr))
		}
	}
	return out
}

func price(p billing.Price) access.Price {
	return access.Price{ID: p.ID.String(), ProductID: p.ProductID.String(), Key: p.Key, Amount: p.UnitAmount, Currency: p.Currency,
		AccessHours: p.AccessDurationHours, IntervalHours: p.BillingIntervalHours, Archived: p.Archived}
}
