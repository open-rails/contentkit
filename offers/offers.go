// Package offers sells content, versions and groups. Staff and creators put
// an offer (terms) on a target; an engine River job syncs it to OpenRails as
// a product whose key and only entitlement is the target's access key, with
// one price under PriceKey. Prices are immutable, so a reprice is a new
// price; a withdrawal archives the product. It also serves free group joins
// and the viewer's library.
package offers

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/kinds"
)

// PriceKey names an offer's price under its product.
const PriceKey = "offer"

var (
	ErrInvalid   = errors.New("offers: invalid")
	ErrNotFound  = errors.New("offers: not found")
	ErrForbidden = errors.New("offers: forbidden")
	ErrConflict  = errors.New("offers: conflict")
)

// TargetKind is what an offer sells.
type TargetKind string

const (
	ContentTarget TargetKind = "content" // every version of a content item
	VersionTarget TargetKind = "version" // one version
	GroupTarget   TargetKind = "group"   // a sellable group: its members and its members-only content
)

// Target is one sold thing. Content is a version's content.
type Target struct {
	Kind    TargetKind `json:"kind"`
	ID      string     `json:"id"`
	Content string     `json:"content_id,omitempty"`
}

// State is an offer's lifecycle state.
type State string

const (
	Active    State = "active"
	Withdrawn State = "withdrawn"
)

// Offer is one offer on a target.
type Offer struct {
	ID     string     `json:"id"`
	Target Target     `json:"target"`
	Key    access.Key `json:"key"`
	Terms  Terms      `json:"terms"`
	State  State      `json:"state"`
	// Synced: OpenRails sells it on these terms (or has taken it off sale).
	Synced    bool      `json:"synced"`
	ProductID string    `json:"product_id,omitempty"`
	PriceID   string    `json:"price_id,omitempty"`
	LastError string    `json:"last_error,omitempty"`
	Revision  int64     `json:"revision"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Put sets a target's offer.
type Put struct {
	Target Target
	Terms  Terms
}

// Options configures a Service.
type Options struct {
	Pool   *pgxpool.Pool
	Schema string // ContentKit's schema
	Tenant string
	Kinds  kinds.Config
	Config Config
	// Billing syncs offers and grants free joins; Gate answers joins and the
	// library.
	Billing access.Billing
	Gate    *access.Gate
	// Queue is the River queue of the sync jobs; contentkit_offers when empty.
	Queue  string
	Logger *slog.Logger
}

// Service is the offers module.
type Service struct {
	pool    *pgxpool.Pool
	t       tables
	tenant  string
	kinds   kinds.Config
	cfg     Config
	billing access.Billing
	gate    *access.Gate
	queue   string
	log     *slog.Logger

	mu       sync.Mutex
	composed bool
	client   *river.Client[pgx.Tx]
}

type tables struct {
	offers, items, versions, text, creators, groups, groupNames string
}

// New validates o.
func New(o Options) (*Service, error) {
	switch {
	case o.Pool == nil:
		return nil, errors.New("offers: Pool is required")
	case o.Billing == nil:
		return nil, errors.New("offers: Billing is required")
	case o.Gate == nil || o.Gate.Tenant() != o.Tenant:
		return nil, errors.New("offers: a Gate of the tenant is required")
	case strings.TrimSpace(o.Schema) == "":
		return nil, errors.New("offers: Schema is required")
	}
	for k, m := range o.Config.Minimum {
		if k != ContentTarget && k != VersionTarget && k != GroupTarget || m < 0 {
			return nil, fmt.Errorf("offers: minimum %s %s", k, m)
		}
	}
	if o.Config.Currency != "" && !currencyRe.MatchString(o.Config.Currency) {
		return nil, fmt.Errorf("offers: currency %q", o.Config.Currency)
	}
	q := func(name string) string { return pgx.Identifier{o.Schema, name}.Sanitize() }
	s := &Service{pool: o.Pool, tenant: o.Tenant, kinds: o.Kinds, cfg: o.Config, billing: o.Billing, gate: o.Gate,
		queue: o.Queue, log: o.Logger,
		t: tables{offers: q("content_offers"), items: q("content_items"), versions: q("content_versions"), text: q("content_text"),
			creators: q("content_creators"), groups: q("content_groups"), groupNames: q("content_group_names")}}
	if s.queue == "" {
		s.queue = "contentkit_offers"
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	return s, nil
}

// key is a target's access key.
func (s *Service) key(t Target) (access.Key, error) {
	switch t.Kind {
	case ContentTarget:
		return access.ContentKey(s.tenant, t.ID)
	case VersionTarget:
		return access.VersionKey(s.tenant, t.Content, t.ID)
	case GroupTarget:
		return access.GroupKey(s.tenant, t.ID)
	}
	return access.Key{}, fmt.Errorf("%w: target kind %q", ErrInvalid, t.Kind)
}

// targetOf is the target a content, version or group key sells.
func targetOf(k access.Key) (Target, bool) {
	switch k.Kind() {
	case access.KeyContent:
		return Target{Kind: ContentTarget, ID: k.Content()}, true
	case access.KeyVersion:
		return Target{Kind: VersionTarget, ID: k.Version(), Content: k.Content()}, true
	case access.KeyGroup:
		return Target{Kind: GroupTarget, ID: k.Group()}, true
	}
	return Target{}, false
}

func (s *Service) languages() []string { return s.kinds.Languages }

// q runs fn in a transaction.
func (s *Service) tx(ctx context.Context, fn func(pgx.Tx) error) error {
	return pgx.BeginFunc(ctx, s.pool, fn)
}

// GroupAccess is a sellable group's sale as a card or page shows it: its
// key, its active offer and the viewer's standing.
type GroupAccess struct {
	Key access.Key `json:"key"`
	// Offer is the active offer as paywalls show it (Sales); nil while the
	// group sells nothing.
	Offer *Sale `json:"offer,omitempty"`
	// Member: the viewer holds the key (bought, subscribed or joined free).
	Member bool `json:"member"`
	// Editor: the viewer edits the group's content (its owner, a holder of
	// {kind}:content:write in its AuthKit scope), so opens it all and is
	// offered nothing.
	Editor bool `json:"editor,omitempty"`
	// Unknown: the billing read failed, so Member is false and may be wrong.
	Unknown bool `json:"unknown,omitempty"`
}

// GroupAccess answers, per group in order, its key, active offer and the
// viewer's standing: one offers read and at most one billing read
// (access.Gate.Holds). A failed billing read answers Unknown, not an error.
func (s *Service) GroupAccess(ctx context.Context, viewer access.Actor, groups []string) ([]GroupAccess, error) {
	out := make([]GroupAccess, len(groups))
	keys := make([]access.Key, len(groups))
	for i, id := range groups {
		k, err := access.GroupKey(s.tenant, id)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
		}
		keys[i], out[i].Key = k, k
	}
	live, err := s.Live(ctx, keys)
	if err != nil {
		return nil, err
	}
	held, err := s.gate.Holds(ctx, viewer, keys)
	unknown := errors.Is(err, access.ErrUnavailable)
	if err != nil && !unknown {
		return nil, err
	}
	for i, k := range keys {
		if sale, ok := saleOf(live[k.String()]); ok {
			out[i].Offer = &sale
		}
		out[i].Member, out[i].Unknown = held[i], unknown
	}
	return out, nil
}

// Joined is the groups whose key viewer holds, bought, subscribed or joined
// free: the request's one entitlements read (access.Gate.Filter), at most
// access.HeldLimit of them. A failed read answers none.
func (s *Service) Joined(ctx context.Context, viewer access.Actor) ([]string, error) {
	f, err := s.gate.Filter(ctx, viewer)
	if err != nil && !errors.Is(err, access.ErrUnavailable) {
		return nil, err
	}
	return f.Groups(), nil
}
