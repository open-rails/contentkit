package access

import "context"

// HeldLimit is the keys one billing read returns per keyspace; a viewer
// holding more gets an incomplete Filter.
const HeldLimit = 1000

// Billing is ContentKit's billing dependency: which keys a viewer holds, the
// catalog edits behind offers, and free grants. adapters/openrails implements
// it over an embedded or a remote OpenRails. Every method is batch-shaped.
type Billing interface {
	// Held answers q for subject in one read. A subject that cannot hold
	// keys (no customer id) answers an empty Answer, not an error.
	Held(ctx context.Context, subject string, q Query) (Answer, error)
	// Products reads the products with these keys, live or archived, each
	// with its live prices; a key without a product is absent.
	Products(ctx context.Context, keys []string) (map[string]Product, error)
	// CreateProducts creates products, in order.
	CreateProducts(ctx context.Context, products []NewProduct) ([]Product, error)
	// UpdateProducts renames, archives or restores products, in order. An
	// archived product is not sold; its buyers and subscribers keep it.
	UpdateProducts(ctx context.Context, updates []ProductUpdate) ([]Product, error)
	// CreatePrices creates prices, in order. Prices are immutable: equal terms
	// under a key answer the existing price, and other terms under the key of
	// a live price replace (archive) it.
	CreatePrices(ctx context.Context, prices []NewPrice) ([]Price, error)
	// ArchivePrices takes prices off sale; buyers and subscribers keep theirs.
	ArchivePrices(ctx context.Context, ids []string) error
	// Grant gives subjects products free and indefinitely.
	Grant(ctx context.Context, grants []Grant) error
	// Revoke takes back subjects' free grants of products; revoking what is
	// not granted changes nothing.
	Revoke(ctx context.Context, grants []Grant) error
	// StopRenewals stops every subscription to products from renewing: each
	// ends with its paid period. A stopped one stays as it is.
	StopRenewals(ctx context.Context, productIDs []string) error
}

// Query is one entitlement read: exact keys and keyspace prefixes.
type Query struct {
	Keys     []string // exact keys
	Prefixes []string // each ending in ':'
	Limit    int      // keys per prefix; 0 is HeldLimit
}

// HeldKeys is the keys a subject holds under one prefix, in byte order.
type HeldKeys struct {
	Keys      []string
	Truncated bool // more than Query.Limit keys are held
}

// Answer answers a Query.
type Answer struct {
	Keys map[string]bool     // every requested key; absent means not held
	Held map[string]HeldKeys // one entry per requested prefix
}

// Product is a billing product: ContentKit sells each access key as one
// product whose key and only entitlement is that key.
type Product struct {
	ID           string
	Key          string
	DisplayName  string
	Entitlements []string
	Archived     bool
	Prices       []Price // its live prices
}

// NewProduct creates a product.
type NewProduct struct {
	Key          string
	DisplayName  string
	Entitlements []string
}

// ProductUpdate changes a product's set fields.
type ProductUpdate struct {
	ID          string
	DisplayName *string
	Archived    *bool
}

// Price is one way to buy a product: Amount micros of Currency.
type Price struct {
	ID        string
	ProductID string
	Key       string
	Amount    int64
	Currency  string
	// AccessHours is how long a purchase opens it; nil is forever.
	AccessHours *int
	// IntervalHours renews it every so many hours; nil is a one-time charge.
	IntervalHours *int
	Archived      bool
}

// NewPrice creates a price on a product.
type NewPrice struct {
	ProductID     string
	Key           string
	Amount        int64
	Currency      string
	AccessHours   *int
	IntervalHours *int
	// PSPs are the payment providers selling it.
	PSPs []string
}

// Grant names a subject and a product given free.
type Grant struct {
	Subject   string
	ProductID string
}
