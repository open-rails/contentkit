// Package discovery is ContentKit's public read surface over its own content:
// lists, keyword search, typeahead, similar and "for you" lists, the release
// date index, viewers' content filters and history, and the signals and
// exposures clients send. Every read runs over content_facets under the
// catalog's one visibility rule (Visibility): access, site scope, content
// filters, listing and language apply inside ContentKit, and each content
// item shows as one card carrying the version that fits the viewer best.
//
// Similar and "for you" lists come from a Candidates source (Engagement, the
// co-engagement default, or a provider's) through the Recommender's shared
// policy; popular sorts read the policy scores an engine job copies from the
// signal plane into content_facets.popularity.
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
)

// Errors; match with errors.Is.
var (
	ErrInvalid  = errors.New("discovery: invalid request")
	ErrNotFound = errors.New("discovery: not found")
	// ErrNoSignals is a read or write that needs the signal plane, which the
	// engine runs without.
	ErrNoSignals = errors.New("discovery: no signal plane")
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
	// Groups names the groups on cards and in typeahead.
	Groups *groups.Store
	// Covers resolves cards' covers; nil leaves them out.
	Covers Covers
	// Hub is the keyword search client and the signal plane. Without a
	// signal plane, history, signals, exposures, view_status, similar,
	// recommendations and popularity are unavailable.
	Hub *hub.Hub
	// Candidates sources similar and recommendations; nil is co-engagement
	// over the signal plane.
	Candidates Candidates
	// Policy ranks popular sorts; zero is popularity.PolicyV1.
	Policy popularity.Policy
	Logger *slog.Logger
	Now    func() time.Time
}

// Service serves discovery for one tenant.
type Service struct {
	o    Options
	t    tables
	recs *Recommender // nil without a signal plane
}

type tables struct {
	facets, items, versions, versionText, text, codes, memberships, groups, prefs, favorites, reactions, searches, searchMarks, erased string
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
	if o.Policy.Name == "" {
		o.Policy = popularity.PolicyV1
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
	s := &Service{o: o, t: tables{facets: q("content_facets"), items: q("content_items"), versions: q("content_versions"),
		versionText: q("content_version_text"), text: q("content_text"), codes: q("content_codes"), memberships: q("content_memberships"),
		groups: q("content_groups"), prefs: q("content_filter_prefs"), favorites: q("content_favorites"), reactions: q("content_reactions"),
		searches: q("content_searches"), searchMarks: q("content_search_marks"), erased: q("content_erased_subjects")}}
	if store := o.Hub.Signals(); store != nil {
		c := o.Candidates
		if c == nil {
			c = Engagement{Store: store, Tenant: o.Tenant}
		}
		s.recs = &Recommender{Candidates: c, Store: store, Tenant: o.Tenant}
	} else if o.Candidates != nil {
		return nil, errors.New("discovery: Candidates needs the signal plane")
	}
	return s, nil
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
