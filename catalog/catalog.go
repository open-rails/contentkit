// Package catalog is ContentKit's own content: content items (a gallery, a
// movie, a creator or blog post), their per-language text, creators, versions
// with their text and segments, the state machine (draft, published,
// scheduled, deleted), the listing projection (content_facets), the one
// visibility rule every read applies, the events outbox and the catalog
// totals. Kinds are configuration (package kinds); nothing here names a host's
// nouns.
package catalog

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/authkit/auth"
	"github.com/open-rails/helpers/cache"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/groups"
	"github.com/open-rails/contentkit/internal/gate"
	"github.com/open-rails/contentkit/kinds"
	"github.com/open-rails/contentkit/offers"
)

// HeldKind is the review queue's kind for held content (content.Runtime's
// /moderation/held?kind=content).
const HeldKind = "content"

// DefaultDeletedRetention is how long deleted, never-sold content keeps its
// media and can be restored before PurgeDeleted erases it.
const DefaultDeletedRetention = 30 * 24 * time.Hour

// Querier is a pool or a transaction.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Groups answers each version's effective group ids (its content's
// memberships, its own and what they imply), read in q's transaction, and
// owner groups' cards: *groups.Store.
type Groups interface {
	EffectiveGroups(ctx context.Context, q groups.Querier, versionIDs []string) (map[string][]string, error)
	Cards(ctx context.Context, ids []groups.GroupID, o groups.CardOptions) ([]groups.GroupCard, error)
}

// Moderation screens text before it publishes: content.Runtime.
type Moderation interface {
	Screen(ctx context.Context, in content.ModerationInput) (content.Screening, error)
}

// Offers is *offers.Service: Resync re-syncs the targets' products in the
// writing transaction; Sales and Current fill reads' paywalls and editors'
// offers.
type Offers interface {
	Resync(ctx context.Context, tx pgx.Tx, targets []offers.Target) error
	Sales(ctx context.Context, keys []string) (map[string]offers.Sale, error)
	Current(ctx context.Context, a auth.Authenticator, actor access.Actor, targets []offers.Target) ([]*offers.Offer, error)
}

// Standing reads a signed-in person's standing with content items and
// versions, per ref in order: *discovery.Service.
type Standing interface {
	Standing(ctx context.Context, actor access.Actor, refs []contentref.ContentRef) ([]ViewerState, error)
}

// Sanitizer cleans rich text on write: content.ContentProcessor.
type Sanitizer = content.ContentProcessor

// Options configures a Store.
type Options struct {
	Pool   *pgxpool.Pool
	Schema string
	Tenant string
	// Kinds is the site's content model. Required.
	Kinds kinds.Config
	// Auth gates the routes and answers staff and owner-group permissions.
	Auth auth.Authenticator
	// Gate answers viewers' entitlements; nil builds one without billing
	// (only free rules and claimed tiers open).
	Gate *access.Gate
	// Offers renames the products of content whose title changes and answers
	// paywalls' offers; nil when nothing is sold.
	Offers Offers
	// Groups answers effective group ids for content_facets; nil: none. Wire
	// its Options.ContentChanged to Store.ContentChanged.
	Groups Groups
	// Moderation screens text of content that is or goes live; nil approves
	// unless contentkit.NewRuntime binds its content runtime (BindModeration).
	Moderation Moderation
	// Body sanitizes text bodies; nil strips tags.
	Body Sanitizer
	// DeletedRetention: see DefaultDeletedRetention.
	DeletedRetention time.Duration
	// DocumentsChanged runs in the writing transaction after a write queued
	// search documents, so the engine can sync them at once. Optional.
	DocumentsChanged func(ctx context.Context, q Querier) error
	Now              func() time.Time
	Logger           *slog.Logger
}

// Store is one tenant's catalog.
type Store struct {
	pool      *pgxpool.Pool
	schema    string
	qs        string
	tenant    string
	cfg       kinds.Config
	t         tables
	guard     *gate.Gate
	accessG   *access.Gate
	offers    Offers
	groups    Groups
	moderator Moderation
	body      Sanitizer
	media     *Media
	standing  Standing
	retention time.Duration
	documents func(ctx context.Context, q Querier) error
	now       func() time.Time
	log       *slog.Logger
	totals    *cache.Cache[totalsKey, Total]

	mu          sync.Mutex
	subscribers []Subscriber
}

type tables struct {
	items, text, creators, versions, versionText, segments, facets, events string
	groups, groupNames, memberships, prefs, counts, docs, offers           string
	reactions, favorites, comments, accessHistory                          string
}

// New validates opts and checks the schema has the catalog tables.
func New(ctx context.Context, opts Options) (*Store, error) {
	switch {
	case opts.Pool == nil:
		return nil, errors.New("catalog: Pool is required")
	case strings.TrimSpace(opts.Schema) == "" || strings.TrimSpace(opts.Tenant) == "":
		return nil, errors.New("catalog: Schema and Tenant are required")
	}
	if err := opts.Kinds.Validate(); err != nil {
		return nil, err
	}
	if err := gate.Check(opts.Auth, access.ContentWrite, access.ContentViewRestricted); err != nil {
		return nil, fmt.Errorf("catalog: Auth: %w", err)
	}
	q := func(name string) string { return pgx.Identifier{opts.Schema, name}.Sanitize() }
	s := &Store{
		pool: opts.Pool, schema: opts.Schema, qs: pgx.Identifier{opts.Schema}.Sanitize(), tenant: opts.Tenant, cfg: opts.Kinds,
		t: tables{items: q("content_items"), text: q("content_text"), creators: q("content_creators"), versions: q("content_versions"),
			versionText: q("content_version_text"), segments: q("content_segments"), facets: q("content_facets"), events: q("content_events"),
			groups: q("content_groups"), groupNames: q("content_group_names"), memberships: q("content_memberships"),
			prefs: q("content_filter_prefs"), counts: q("content_interaction_counts"), docs: q("content_search_documents"), offers: q("content_offers"),
			reactions: q("content_reactions"), favorites: q("content_favorites"), comments: q("content_comments"), accessHistory: q("content_access_history")},
		guard: &gate.Gate{Auth: opts.Auth, Log: opts.Logger}, accessG: opts.Gate, offers: opts.Offers, groups: opts.Groups, moderator: opts.Moderation,
		body: opts.Body, retention: opts.DeletedRetention, documents: opts.DocumentsChanged, now: opts.Now, log: opts.Logger,
	}
	if s.accessG == nil {
		g, err := access.NewGate(access.GateConfig{Tenant: opts.Tenant, Kinds: opts.Kinds, Logger: opts.Logger})
		if err != nil {
			return nil, fmt.Errorf("catalog: %w", err)
		}
		s.accessG = g
	} else if s.accessG.Tenant() != opts.Tenant {
		return nil, fmt.Errorf("catalog: Gate tenant %q differs from %q", s.accessG.Tenant(), opts.Tenant)
	}
	if s.body == nil {
		s.body = stripTags{}
	}
	if s.retention <= 0 {
		s.retention = DefaultDeletedRetention
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	var err error
	if s.totals, err = cache.New(totalsPolicy, s.loadTotals); err != nil {
		return nil, err
	}
	if _, err := s.pool.Exec(ctx, `SELECT 1 FROM `+s.t.items+` LIMIT 0; SELECT 1 FROM `+s.t.facets+` LIMIT 0; SELECT 1 FROM `+s.t.events+` LIMIT 0`); err != nil {
		return nil, fmt.Errorf("catalog: schema %q lacks the catalog tables (apply contentkit.Migrate): %w", s.schema, err)
	}
	return s, nil
}

// Close releases the totals cache.
func (s *Store) Close() { s.totals.Close() }

// BindModeration screens through m when Options.Moderation is nil, so one
// moderator serves comments and catalog text; call it before serving.
func (s *Store) BindModeration(m Moderation) {
	if s.moderator == nil {
		s.moderator = m
	}
}

// BindStanding fills detail reads' viewer state from st; call it once,
// before serving.
func (s *Store) BindStanding(st Standing) { s.standing = st }

// fillStanding attaches a signed-in person's standing with each item and
// its versions, in one read.
func (s *Store) fillStanding(ctx context.Context, actor access.Actor, items []*Content) error {
	if s.standing == nil || !actor.Member() {
		return nil
	}
	var refs []contentref.ContentRef
	var into []**ViewerState
	for _, c := range items {
		if c == nil {
			continue
		}
		ref := s.Ref(c.Kind, c.ID)
		refs, into = append(refs, ref), append(into, &c.Viewer)
		for i := range c.Versions {
			refs, into = append(refs, ref.WithVersion(c.Versions[i].ID)), append(into, &c.Versions[i].Viewer)
		}
	}
	if len(refs) == 0 {
		return nil
	}
	got, err := s.standing.Standing(ctx, actor, refs)
	if err != nil {
		return err
	}
	for i := range got {
		*into[i] = &got[i]
	}
	return nil
}

// Tenant is the store's tenant.
func (s *Store) Tenant() string { return s.tenant }

// Kinds is the store's content model.
func (s *Store) Kinds() kinds.Config { return s.cfg }

// Ref is a content item's reference.
func (s *Store) Ref(kind, id string) contentref.ContentRef { return contentref.New(s.tenant, kind, id) }

// defaultLanguage is the site's first language.
func (s *Store) defaultLanguage() string { return s.cfg.Languages[0] }

func (s *Store) begin(ctx context.Context) (pgx.Tx, error) {
	return s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
}
