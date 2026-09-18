// Package taxonomy is ContentKit's generic catalog: tenant-scoped nodes (tags,
// artists, creators, characters, series, seasons, voice actors, ...) with
// localized names and aliases, typed node relationships and typed assignments
// of host content (a work or one of its versions) to nodes. Effective tags of
// a version are the deduplicated union of its work's and its own assignments;
// a multi-node filter must hold on one eligible version. Per-language counts
// and typeahead documents derive from the host's keyword documents and
// eligibility join, never from triggers.
package taxonomy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/search"
)

// Sentinel errors; callers match them with errors.Is.
var (
	ErrInvalid  = errors.New("taxonomy: invalid input")
	ErrNotFound = errors.New("taxonomy: not found")
	ErrConflict = errors.New("taxonomy: conflict")
)

// TaxonomyID identifies one node within a tenant. Hosts adopting existing
// tables keep their ids as text; omitted ids are generated (uuid).
type TaxonomyID = contentref.TaxonomyID

// State of a node. A merged node keeps its row and an alias_of edge to the
// surviving node; deleted nodes keep their assignments but serve nothing.
type State string

const (
	StateActive  State = "active"
	StateMerged  State = "merged"
	StateDeleted State = "deleted"
)

// NameKind distinguishes the one canonical name per language from aliases.
type NameKind string

const (
	NameCanonical NameKind = "name"
	NameAlias     NameKind = "alias"
)

// Relation of an edge (from, relation, to): "from is <relation> of to".
type Relation string

const (
	RelationAliasOf  Relation = "alias_of"
	RelationMemberOf Relation = "member_of"
	RelationArtistOf Relation = "artist_of"
	RelationVoiceOf  Relation = "voice_of"
	RelationParent   Relation = "parent"
	RelationChild    Relation = "child"
	RelationSynonym  Relation = "synonym"
)

var relations = map[Relation]struct{}{RelationAliasOf: {}, RelationMemberOf: {}, RelationArtistOf: {}, RelationVoiceOf: {}, RelationParent: {}, RelationChild: {}, RelationSynonym: {}}

// AssignmentState: proposed assignments (for example from User Intelligence)
// are stored but never effective until a host accepts them.
type AssignmentState string

const (
	AssignmentActive   AssignmentState = "active"
	AssignmentProposed AssignmentState = "proposed"
)

// Node is one catalog record.
type Node struct {
	TaxonomyID     TaxonomyID `json:"taxonomy_id"`
	TenantID       string     `json:"tenant_id"`
	Kind           string     `json:"kind"`
	Slug           string     `json:"slug"`
	State          State      `json:"state"`
	SourceRevision int64      `json:"source_revision"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

// Name is one localized canonical name or alias of a node.
type Name struct {
	Language       string   `json:"language"`
	Kind           NameKind `json:"kind"`
	Name           string   `json:"name"`
	Normalized     string   `json:"normalized,omitempty"`
	SourceRevision int64    `json:"source_revision"`
}

// Edge is one typed directed relationship between two nodes of a tenant.
type Edge struct {
	From           TaxonomyID `json:"from_taxonomy_id"`
	Relation       Relation   `json:"relation"`
	To             TaxonomyID `json:"to_taxonomy_id"`
	SourceRevision int64      `json:"source_revision"`
}

// Assignment links host content (the work, or one version when
// ContentVersionID is set) to a node under a host-defined relation such as
// tag, artist, publisher, character, series, installment, voice_actor, seller.
// An empty Relation defaults to the node's kind.
type Assignment struct {
	contentref.ContentRef
	TaxonomyID     TaxonomyID      `json:"taxonomy_id"`
	Relation       string          `json:"relation"`
	State          AssignmentState `json:"state,omitempty"`
	SourceRevision int64           `json:"source_revision"`
}

// Scope tells whether an effective tag came from the work or the version.
type Scope string

const (
	ScopeContent Scope = "content"
	ScopeVersion Scope = "version"
)

// EffectiveTag is one node effective on a reference.
type EffectiveTag struct {
	TaxonomyID TaxonomyID `json:"taxonomy_id"`
	Kind       string     `json:"kind"`
	Slug       string     `json:"slug"`
	Relation   string     `json:"relation"`
	Scope      Scope      `json:"scope"`
}

// Count is the number of distinct works of one content kind with an eligible
// document in one language whose effective assignments include the node.
type Count struct {
	ContentKind string `json:"content_kind"`
	Language    string `json:"language"`
	Count       int    `json:"count"`
}

// Options configures one tenant's store.
type Options struct {
	Pool   *pgxpool.Pool
	Schema string
	Tenant string
	// Kinds are the node kinds this tenant registers (tag, artist, creator,
	// character, series, season, voice_actor, listing, ...). Required; a kind
	// must never collide with a host content kind.
	Kinds []string
	// Languages are the document languages typeahead documents are built in
	// (one document per node and language). Required.
	Languages []string
	// CountEligibility is the host's public visibility join used to derive
	// counts (see search.Eligibility). Without it every document counts.
	// Reserved arg names: tenant, ids, taxonomy_kinds.
	CountEligibility *search.Eligibility
	// Logger receives the admin API access log: each request at DEBUG, a 5xx
	// at ERROR, both with the cause the response withheld. nil -> slog.Default().
	Logger *slog.Logger
}

// Store is one tenant's catalog over the host schema.
type Store struct {
	pool      *pgxpool.Pool
	tx        pgx.Tx
	qs        string
	schema    string
	tenant    string
	kinds     map[string]struct{}
	kindList  []string
	languages []string
	countElig *search.Eligibility
	log       *slog.Logger
}

var identRE = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// New validates the options and returns the store.
func New(opts Options) (*Store, error) {
	if opts.Pool == nil {
		return nil, fmt.Errorf("%w: Pool is required", ErrInvalid)
	}
	qs, err := search.QuoteSchema(opts.Schema)
	if err != nil {
		return nil, fmt.Errorf("%w: schema: %v", ErrInvalid, err)
	}
	tenant := strings.TrimSpace(opts.Tenant)
	if tenant == "" {
		return nil, fmt.Errorf("%w: Tenant is required", ErrInvalid)
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	s := &Store{pool: opts.Pool, qs: qs, schema: strings.TrimSpace(opts.Schema), tenant: tenant, kinds: map[string]struct{}{}, countElig: opts.CountEligibility, log: log}
	for _, k := range opts.Kinds {
		k = strings.TrimSpace(k)
		if !identRE.MatchString(k) {
			return nil, fmt.Errorf("%w: kind %q must match %s", ErrInvalid, k, identRE)
		}
		if _, dup := s.kinds[k]; !dup {
			s.kinds[k] = struct{}{}
			s.kindList = append(s.kindList, k)
		}
	}
	if len(s.kindList) == 0 {
		return nil, fmt.Errorf("%w: Kinds is required", ErrInvalid)
	}
	for _, l := range opts.Languages {
		l, err := normalizeLanguage(l)
		if err != nil {
			return nil, err
		}
		s.languages = append(s.languages, l)
	}
	if len(s.languages) == 0 {
		return nil, fmt.Errorf("%w: Languages is required", ErrInvalid)
	}
	if s.countElig != nil && strings.TrimSpace(s.countElig.SQL) == "" {
		s.countElig = nil
	}
	return s, nil
}

// Tenant returns the tenant this store is scoped to.
func (s *Store) Tenant() string { return s.tenant }

// Kinds returns the registered node kinds.
func (s *Store) Kinds() []string { return append([]string{}, s.kindList...) }

// WithTx returns the store bound to the host's transaction so catalog writes
// commit with the content change that caused them.
func (s *Store) WithTx(tx pgx.Tx) *Store {
	c := *s
	c.tx = tx
	return &c
}

type querier interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

// run executes fn in the bound transaction or in a new one.
func (s *Store) run(ctx context.Context, fn func(q querier) error) error {
	if s.tx != nil {
		return mapPGError(fn(s.tx))
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := fn(tx); err != nil {
		return mapPGError(err)
	}
	return tx.Commit(ctx)
}

// read executes fn against the bound transaction or the pool.
func (s *Store) read(ctx context.Context, fn func(q querier) error) error {
	if s.tx != nil {
		return mapPGError(fn(s.tx))
	}
	return mapPGError(fn(s.pool))
}

func (s *Store) table(name string) string { return s.qs + "." + name }

// constraintError maps a Postgres integrity violation onto a sentinel while
// keeping the constraint name for logs; the HTTP layer never renders it.
type constraintError struct {
	sentinel   error
	sqlstate   string
	constraint string
}

func (e constraintError) Error() string {
	return fmt.Sprintf("%s: %s (sqlstate %s)", e.sentinel.Error(), e.constraint, e.sqlstate)
}

func (e constraintError) Unwrap() error { return e.sentinel }

func mapPGError(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return err
	}
	switch pgErr.Code {
	case "23505":
		return constraintError{ErrConflict, pgErr.Code, pgErr.ConstraintName}
	case "23503":
		return constraintError{ErrNotFound, pgErr.Code, pgErr.ConstraintName}
	case "23514":
		return constraintError{ErrInvalid, pgErr.Code, pgErr.ConstraintName}
	}
	return err
}

func (s *Store) requireKind(kind string) error {
	if _, ok := s.kinds[kind]; !ok {
		return fmt.Errorf("%w: kind %q is not registered for tenant %s", ErrInvalid, kind, s.tenant)
	}
	return nil
}

func (s *Store) isKind(kind string) bool {
	_, ok := s.kinds[kind]
	return ok
}

func validateID(id TaxonomyID) error {
	v := string(id)
	if v == "" || utf8.RuneCountInString(v) > 128 || strings.IndexFunc(v, unicode.IsSpace) >= 0 {
		return fmt.Errorf("%w: taxonomy_id %q must be 1-128 characters without whitespace", ErrInvalid, v)
	}
	return nil
}

func validateSlug(slug string) (string, error) {
	slug = strings.TrimSpace(slug)
	if slug == "" || utf8.RuneCountInString(slug) > 200 || strings.ContainsAny(slug, "/ \t\n") {
		return "", fmt.Errorf("%w: slug %q must be 1-200 characters without whitespace or '/'", ErrInvalid, slug)
	}
	return slug, nil
}

func normalizeLanguage(l string) (string, error) {
	l = strings.ToLower(strings.TrimSpace(l))
	if l == "" || len(l) > 16 || strings.IndexFunc(l, unicode.IsSpace) >= 0 {
		return "", fmt.Errorf("%w: language %q", ErrInvalid, l)
	}
	return l, nil
}

func validateRelationName(r string) (string, error) {
	r = strings.TrimSpace(r)
	if r != "" && !identRE.MatchString(r) {
		return "", fmt.Errorf("%w: relation %q must match %s", ErrInvalid, r, identRE)
	}
	return r, nil
}

func (s *Store) requireTenant(ref contentref.ContentRef) error {
	if err := ref.Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if ref.TenantID != s.tenant {
		return fmt.Errorf("%w: %s is outside tenant %s", ErrInvalid, ref, s.tenant)
	}
	if s.isKind(ref.ContentKind) {
		return fmt.Errorf("%w: %s uses taxonomy kind %q as a content kind", ErrInvalid, ref, ref.ContentKind)
	}
	return nil
}

func uniqueIDs(ids []TaxonomyID) ([]string, error) {
	seen := map[TaxonomyID]struct{}{}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if err := validateID(id); err != nil {
			return nil, err
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, string(id))
	}
	return out, nil
}
