// Package catalog is ContentKit's own content: content items (a gallery, a
// movie, a creator or blog post), their per-language text, creators, versions
// with their text and segments, the state machine (draft, published,
// scheduled, deleted), the listing projection (content_facets), the one
// visibility rule every read applies, the retained event log and the catalog
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
	"github.com/open-rails/contentkit/channels"
	"github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/entities"
	"github.com/open-rails/contentkit/internal/gate"
	"github.com/open-rails/contentkit/kinds"
)

// DefaultDeletedRetention is how long deleted content keeps its
// media and can be restored before PurgeDeleted erases it.
const DefaultDeletedRetention = 30 * 24 * time.Hour

// Querier is a pool or a transaction.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Entities answers each version's effective entity ids (its content's
// memberships, its own and what they imply), read in q's transaction, and
// owner entities' cards: *entities.Store.
type Entities interface {
	EffectiveEntities(ctx context.Context, q entities.Querier, versionIDs []string) (map[string][]string, error)
	Cards(ctx context.Context, ids []entities.EntityID, o entities.CardOptions) ([]entities.EntityCard, error)
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
	// Auth gates the routes and answers staff and owner-entity permissions.
	Auth auth.Authenticator
	// Gate answers viewers' entitlements; nil builds one without billing
	// (only free rules and claimed tiers open).
	Gate *access.Gate
	// Entities answers effective entity ids for content_facets; nil: none. Wire
	// its Options.ContentChanged to Store.ContentChanged and its
	// Options.NamesChanged to Store.EntitiesRenamed.
	Entities Entities
	Channels *channels.Store
	// Body sanitizes text bodies; nil strips tags.
	Body Sanitizer
	// DeletedRetention: see DefaultDeletedRetention.
	DeletedRetention time.Duration
	// EventRetention keeps committed facts independently of reader progress. Zero is ninety days.
	EventRetention time.Duration
	// OnPurge records required application cleanup in the same transaction.
	OnPurge func(context.Context, pgx.Tx, []contentref.ContentRef) error
	// AfterPurge completes durable cross-store cleanup after local deletion.
	// It runs even when some local deletions failed, and retries via this sweep.
	AfterPurge func(context.Context) error

	// DocumentsChanged queues search refresh with the catalog write.
	DocumentsChanged func(ctx context.Context, q Querier) error
	Now              func() time.Time
	Logger           *slog.Logger
}

// Store is one tenant's catalog.
type Store struct {
	pool           *pgxpool.Pool
	schema         string
	qs             string
	tenant         string
	cfg            kinds.Config
	t              tables
	guard          *gate.Gate
	accessG        *access.Gate
	entities       Entities
	channels       *channels.Store
	body           Sanitizer
	media          *Media
	publicationMu  sync.RWMutex
	publication    PublicationScheduler
	standing       Standing
	retention      time.Duration
	eventRetention time.Duration
	onPurge        func(context.Context, pgx.Tx, []contentref.ContentRef) error
	afterPurge     func(context.Context) error
	documents      func(ctx context.Context, q Querier) error
	now            func() time.Time
	log            *slog.Logger
	totals         *cache.Cache[totalsKey, Total]
}

type tables struct {
	items, creators, versions, versionText, segments, facets string
	entities, entityNames, memberships, prefs, counts, docs  string
	reactions, favorites, comments, accessHistory            string
}

func catalogTables(schema string) tables {
	q := func(name string) string { return pgx.Identifier{schema, name}.Sanitize() }
	return tables{items: q("content_items"), creators: q("content_creators"), versions: q("content_versions"),
		versionText: q("content_version_text"), segments: q("content_segments"), facets: q("content_facets"),
		entities: q("content_entities"), entityNames: q("content_entity_names"), memberships: q("content_memberships"),
		prefs: q("content_filter_prefs"), counts: q("content_interaction_counts"), docs: q("content_search_documents"),
		reactions: q("content_reactions"), favorites: q("content_favorites"), comments: q("content_comments"), accessHistory: q("content_access_history")}
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
	opts.Kinds = opts.Kinds.StorageModel()
	if err := gate.Check(opts.Auth, access.ContentWrite, access.ContentViewRestricted); err != nil {
		return nil, fmt.Errorf("catalog: Auth: %w", err)
	}
	s := &Store{
		pool: opts.Pool, schema: opts.Schema, qs: pgx.Identifier{opts.Schema}.Sanitize(), tenant: opts.Tenant, cfg: opts.Kinds,
		t:     catalogTables(opts.Schema),
		guard: &gate.Gate{Auth: opts.Auth, Log: opts.Logger}, accessG: opts.Gate, entities: opts.Entities, channels: opts.Channels,
		body: opts.Body, retention: opts.DeletedRetention, eventRetention: opts.EventRetention, onPurge: opts.OnPurge, afterPurge: opts.AfterPurge, documents: opts.DocumentsChanged,
		now: opts.Now, log: opts.Logger,
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
	if s.eventRetention < 0 {
		return nil, errors.New("catalog: EventRetention must not be negative")
	}
	if s.eventRetention == 0 {
		s.eventRetention = DefaultEventRetention
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
	if _, err := s.pool.Exec(ctx, `SELECT 1 FROM `+s.t.items+` LIMIT 0; SELECT 1 FROM `+s.t.facets+` LIMIT 0; SELECT epoch FROM `+s.qs+`.content_event_epoch LIMIT 0; SELECT boundary,recorded_at FROM `+s.qs+`.eventlog_events LIMIT 0; SELECT ordinal FROM `+s.qs+`.eventlog_pending LIMIT 0`); err != nil {
		return nil, fmt.Errorf("catalog: schema %q lacks the catalog tables (apply contentkit.Migrate): %w", s.schema, err)
	}
	return s, nil
}

// Close releases the totals cache.
func (s *Store) Close() { s.totals.Close() }

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

// Ref is a content item's reference.
func (s *Store) Ref(kind, id string) contentref.ContentRef { return contentref.New(s.tenant, kind, id) }

// defaultLanguage is the site's first language.
func (s *Store) defaultLanguage() string { return s.cfg.Languages[0] }

func (s *Store) begin(ctx context.Context) (pgx.Tx, error) {
	return s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
}
