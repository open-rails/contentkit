package discoverystore

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/contentkit/access"
	discovery "github.com/open-rails/contentkit/discovery"
	"github.com/open-rails/contentkit/internal/activitylog"
	channelstore "github.com/open-rails/contentkit/internal/channelstore"
	entitlement "github.com/open-rails/contentkit/internal/entitlement"
	entitystore "github.com/open-rails/contentkit/internal/entitystore"
	"github.com/open-rails/contentkit/internal/hub"
	"github.com/open-rails/contentkit/internal/readingstore"
	"github.com/open-rails/contentkit/internal/resolve"
	"github.com/open-rails/contentkit/internal/subject"
	"github.com/open-rails/contentkit/kinds"
)

var (
	ErrInvalid = errors.New("discovery: invalid request")
)

var (
	ErrNotFound = errors.New("discovery: not found")
)

var (
	// ErrNoSignals is a read or write that needs the signal plane, which the
	// engine runs without.
	ErrNoSignals = errors.New("discovery: no signal plane")
)

var (
	// ErrSignalsDown is ErrNoSignals while the configured signal plane is
	// down: retry later.
	ErrSignalsDown = fmt.Errorf("%w: ClickHouse is down", ErrNoSignals)
)

// Options configure a Service.
type Options struct {
	Pool   *pgxpool.Pool
	Schema string
	Tenant string
	Kinds  kinds.Config
	// Visibility is the catalog's visibility rule; every read applies it.
	Visibility Visibility
	// Resolver decides direct reads: the anchor of a similar list and the
	// content a signal names.
	Resolver resolve.Resolver
	// Gate decides each card's access.
	Gate *entitlement.Gate
	// Reading is the readers' reading state, history and links.
	Reading *readingstore.Store
	// Activity stamps and queues the rows the API logs to ClickHouse; nil,
	// or nil Write, without a signal plane.
	Activity *activitylog.Log
	// Entities names the entities on cards and in typeahead.
	Entities *entitystore.Store
	// Channels resolves the primary authoring home independently of metadata.
	Channels *channelstore.Store
	// Media resolves cards' covers and ladders; nil leaves them out.
	Media discovery.Media
	// Hub is the keyword search client and the signal plane: ClickHouse's
	// activity log, which the reading state is folded from.
	Hub *hub.Hub
	// Tenure supplies elapsed access to configured tiers; nil omits it from
	// account statistics. Billing adapters may implement access.TenureReader.
	Tenure access.TenureReader
	Logger *slog.Logger
	Now    func() time.Time
}

// Service serves discovery for one tenant.
type Service struct {
	o         Options
	appID     uuid.UUID
	t         tables
	summaries summaryCache
}

type tables struct {
	facets, items, versions, versionText, codes, memberships, entities, prefs, favorites, reactions, counts, searches, erased, creators, comments, entityNames, normalize, rankings, entityRankings, reposts, reading string
}

// New validates o.
func New(o Options) (*Service, error) {
	if o.Pool == nil || strings.TrimSpace(o.Schema) == "" || strings.TrimSpace(o.Tenant) == "" {
		return nil, errors.New("discovery: Pool, Schema and Tenant are required")
	}
	if o.Visibility == nil || o.Resolver == nil || o.Gate == nil || o.Entities == nil || o.Hub == nil || o.Reading == nil {
		return nil, errors.New("discovery: Visibility, Resolver, Gate, Groups, Hub and Reading are required")
	}
	if err := o.Kinds.Validate(); err != nil {
		return nil, fmt.Errorf("discovery: Kinds: %w", err)
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	q := func(t string) string { return pgx.Identifier{o.Schema, t}.Sanitize() }
	app, err := uuid.Parse(o.Tenant)
	if err != nil {
		return nil, fmt.Errorf("discovery: Tenant %q is not an app id", o.Tenant)
	}
	s := &Service{o: o, appID: app, t: tables{facets: q("content_facets"), items: q("content_items"), versions: q("content_versions"),
		versionText: q("content_version_text"), codes: q("content_codes"), memberships: q("content_memberships"),
		entities: q("content_entities"), prefs: q("content_filter_prefs"), favorites: q("content_favorites"), reactions: q("content_reactions"), counts: q("content_interaction_counts"),
		searches: q("content_searches"), erased: q("content_erased_subjects"), creators: q("content_creators"),
		comments:    q("content_comments"),
		entityNames: q("content_entity_names"), normalize: q("contentkit_keyword_normalize"), rankings: q("content_popular_rankings"),
		entityRankings: q("content_entity_popular_rankings"), reposts: q("content_channel_reposts"), reading: q("reading_state")}}
	return s, nil
}

// signals is nil while the signal plane is up, else ErrNoSignals or, for a
// configured one that is down, ErrSignalsDown. Only reports and clearing the
// raw log need it; reading state is in PostgreSQL.
func (s *Service) signals() error {
	switch {
	case s.o.Hub.SignalPlane():
		return nil
	case s.o.Hub.SignalsConfigured():
		return ErrSignalsDown
	}
	return ErrNoSignals
}

// recommender ranks similar and "for you" lists for reader.
func (s *Service) recommender(reader uuid.UUID, known bool) *Recommender {
	return &Recommender{Candidates: Engagement{s: s}, Tenant: s.o.Tenant, reader: reader, known: known, s: s}
}

// app is the tenant as the UUID reading state is keyed by.
func (s *Service) app() uuid.UUID { return s.appID }

// reader is the subject reading state and activity belong to: the account,
// else the request's anonymous id; false with neither.
func (s *Service) reader(r discovery.Reader) (uuid.UUID, bool) {
	return subject.Of(s.app(), r.Actor, r.AnonymousID)
}

// languages is the configured languages, the fallback order of text.
func (s *Service) languages() []string { return s.o.Kinds.Languages }

// displayLanguage resolves a requested display language: a configured one,
// else the first configured.
func (s *Service) displayLanguage(l string) (string, error) {
	l = strings.ToLower(strings.TrimSpace(l))
	if l == "" {
		return s.o.Kinds.Languages[0], nil
	}
	return l, s.checkLanguage(l)
}

func (s *Service) checkLanguage(l string) error {
	for _, c := range s.o.Kinds.Languages {
		if c == l {
			return nil
		}
	}
	return fmt.Errorf("%w: language %q is not configured", ErrInvalid, l)
}
