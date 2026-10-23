// Package discovery is ContentKit's public read surface over its own content:
// lists, keyword search, typeahead, similar and "for you" lists, the release
// date index, readers' content filters and history, and the signals and
// exposures clients send. Every read runs over content_facets under the
// catalog's one visibility rule (Visibility): access, site scope, content
// filters, listing and language apply inside ContentKit, and each content
// item shows as one card carrying the version that fits the viewer best.
//
// Similar and "for you" lists come from a Candidates source (Engagement, the
// co-engagement default, or a provider's) through the Recommender's shared
// policy; popular sorts rank population-normalized measured readership, or the
// labeled standing order of current reactions and favorites without it.
package discovery

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/groups"
	"github.com/open-rails/contentkit/internal/hub"
	"github.com/open-rails/contentkit/internal/resolve"
	"github.com/open-rails/contentkit/kinds"
	"github.com/open-rails/contentkit/popularity"
	"github.com/open-rails/contentkit/signal"
)

// Errors; match with errors.Is.
var (
	ErrInvalid  = errors.New("discovery: invalid request")
	ErrNotFound = errors.New("discovery: not found")
	// ErrNoSignals is a read or write that needs the signal plane, which the
	// engine runs without.
	ErrNoSignals = errors.New("discovery: no signal plane")
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
	Gate *access.Gate
	// ActivitySigner preserves trusted event-time access facts. Nil retains
	// legacy telemetry without normalized-population qualification.
	ActivitySigner ActivitySigner
	// MeasuredEngagement activates new measured writes only after the erasure
	// executor cohort understands their storage. False leaves this API disabled.
	MeasuredEngagement bool
	// ActivityCohorts are ordered, application-defined readership categories.
	ActivityCohorts []signal.ActivityCohort
	// Groups names the groups on cards and in typeahead.
	Groups *groups.Store
	// Media resolves cards' covers and ladders; nil leaves them out.
	Media Media
	// Sales fills cards' paywall offers; nil leaves them out.
	Sales Sales
	// Hub is the keyword search client and the signal plane. Without a
	// signal plane, or while it is down, history, signals, exposures,
	// view_status, similar, recommendations and popularity are unavailable.
	Hub *hub.Hub
	// Candidates sources similar and recommendations; nil is co-engagement
	// over the signal plane.
	Candidates Candidates
	// Tenure supplies elapsed access to configured tiers; nil omits it from
	// account statistics. Billing adapters may implement access.TenureReader.
	Tenure access.TenureReader
	// Policy ranks popular sorts; zero is popularity.PolicyV2 with an explicit
	// whole-query legacy fallback when trusted population history is absent.
	Policy popularity.Policy
	Logger *slog.Logger
	Now    func() time.Time
}

// Service serves discovery for one tenant.
type Service struct {
	o          Options
	t          tables
	historical *popularity.Ranker
	summaries  summaryCache
}

type tables struct {
	facets, items, versions, versionText, text, codes, memberships, groups, prefs, favorites, reactions, counts, searches, searchMarks, erased, creators, comments, eventStreams string
}

// New validates o.
func New(o Options) (*Service, error) {
	if o.Pool == nil || strings.TrimSpace(o.Schema) == "" || strings.TrimSpace(o.Tenant) == "" {
		return nil, errors.New("discovery: Pool, Schema and Tenant are required")
	}
	if o.Visibility == nil || o.Resolver == nil || o.Gate == nil || o.Groups == nil || o.Hub == nil {
		return nil, errors.New("discovery: Visibility, Resolver, Gate, Groups and Hub are required")
	}
	if err := o.Kinds.Validate(); err != nil {
		return nil, fmt.Errorf("discovery: Kinds: %w", err)
	}
	if err := signal.ValidateActivityCohorts(o.ActivityCohorts); err != nil {
		return nil, err
	}
	if o.Policy.Name == "" {
		o.Policy = popularity.PolicyV2
	}
	if err := o.Policy.Validate(); err != nil {
		return nil, err
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	q := func(t string) string { return pgx.Identifier{o.Schema, t}.Sanitize() }
	s := &Service{o: o, t: tables{facets: q("content_current_facets"), items: q("content_items"), versions: q("content_versions"),
		versionText: q("content_version_text"), text: q("content_text"), codes: q("content_codes"), memberships: q("content_memberships"),
		groups: q("content_groups"), prefs: q("content_filter_prefs"), favorites: q("content_favorites"), reactions: q("content_reactions"), counts: q("content_interaction_counts"),
		searches: q("content_searches"), searchMarks: q("content_search_marks"), erased: q("content_erased_subjects"), creators: q("content_creators"),
		comments: q("content_comments"), eventStreams: q("eventlog_streams")}}
	var err error
	s.historical, err = popularity.New(popularity.Config{Source: o.Hub, Policy: o.Policy, Availability: s,
		Cache: popularity.NewMemoryCache(0), MaxDepth: PopularityDepth})
	if err != nil {
		return nil, err
	}
	if o.Candidates != nil && !o.Hub.SignalsConfigured() {
		return nil, errors.New("discovery: Candidates needs the signal plane")
	}
	return s, nil
}

// signals is nil while the signal plane is up, else ErrNoSignals or, for a
// configured one that is down, ErrSignalsDown.
func (s *Service) signals() error {
	switch {
	case s.o.Hub.SignalPlane():
		return nil
	case s.o.Hub.SignalsConfigured():
		return ErrSignalsDown
	}
	return ErrNoSignals
}

// recommender ranks similar and "for you" lists over the signal plane as it
// is now.
func (s *Service) recommender() (*Recommender, error) {
	store := s.o.Hub.Signals()
	if store == nil {
		if err := s.signals(); err != nil {
			return nil, err
		}
		return nil, ErrSignalsDown
	}
	c := s.o.Candidates
	if c == nil {
		c = Engagement{Store: store, Tenant: s.o.Tenant}
	}
	return &Recommender{Candidates: c, Store: store, Tenant: s.o.Tenant}, nil
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
