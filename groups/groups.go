// Package groups is how content is grouped: tags, artists, series,
// characters, channels, playlists and featured rails. Group kinds come from
// kinds.Config.GroupKinds. A group has localized names and aliases,
// per-language text, declared attributes, a cover, a parent, an optional
// scope and edges to other groups; content joins it through memberships, at
// content level or for one version where the kind allows. A version's
// effective groups (EffectiveGroups) feed content_facets.group_ids; counts
// derive from those facets.
package groups

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/kinds"
	"github.com/open-rails/contentkit/search"
)

// Sentinel errors; callers match them with errors.Is.
var (
	ErrInvalid  = errors.New("groups: invalid input")
	ErrNotFound = errors.New("groups: not found")
	ErrConflict = errors.New("groups: conflict")
)

// GroupID identifies one group within a tenant: a UUIDv7, kept across import.
type GroupID = contentref.GroupID

// State of a group. A merged group points at its survivor (MergedInto);
// deleted groups keep their memberships but are effective nowhere.
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

// Name is one localized canonical name or alias.
type Name struct {
	Language string   `json:"language"`
	Kind     NameKind `json:"kind"`
	Name     string   `json:"name"`
}

// Text is a group's description and sort key in one language.
type Text struct {
	Language    string `json:"language"`
	Description string `json:"description,omitempty"`
	SortKey     string `json:"sort_key,omitempty"`
}

// Cover is a group's picture: an image uploaded to the group's media folder
// ("i-{uuid}") or a member content's cover. Auto covers are the
// most-favorited live member, chosen by Recount when none is selected.
type Cover struct {
	Image     string `json:"image,omitempty"`
	ContentID string `json:"content_id,omitempty"`
	Auto      bool   `json:"auto,omitempty"`
}

// Group is one group record.
type Group struct {
	ID   GroupID `json:"id"`
	Kind string  `json:"kind"`
	Slug string  `json:"slug"`
	// ScopeID is the group the slug is unique within (a character's series).
	ScopeID     GroupID        `json:"scope_id,omitempty"`
	ParentID    GroupID        `json:"parent_id,omitempty"`
	State       State          `json:"state"`
	MergedInto  GroupID        `json:"merged_into,omitempty"`
	Attributes  map[string]any `json:"attributes"`
	Cover       *Cover         `json:"cover,omitempty"`
	OwnerUserID string         `json:"owner_user_id,omitempty"`
	AuthScope   string         `json:"auth_scope,omitempty"`
	CreatedAt   time.Time      `json:"created_at"`
	UpdatedAt   time.Time      `json:"updated_at"`
	Revision    int64          `json:"revision"`
}

// Querier is a pgx pool, connection or transaction.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Options configures one tenant's store.
type Options struct {
	Pool   *pgxpool.Pool
	Schema string
	Tenant string
	// Kinds is the site's content model; its GroupKinds govern every write.
	Kinds kinds.Config
	// Logger receives the HTTP API's failures. nil -> slog.Default().
	Logger *slog.Logger
}

// Store is one tenant's groups.
type Store struct {
	pool   *pgxpool.Pool
	tx     Querier
	qs     string
	schema string
	tenant string
	cfg    kinds.Config
	log    *slog.Logger
}

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
	if err := opts.Kinds.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Store{pool: opts.Pool, qs: qs, schema: strings.TrimSpace(opts.Schema), tenant: tenant, cfg: opts.Kinds, log: log}, nil
}

// Tenant returns the tenant this store is scoped to.
func (s *Store) Tenant() string { return s.tenant }

// WithTx returns the store bound to tx, so group writes commit with the
// engine change (an import batch) that caused them; the caller commits.
func (s *Store) WithTx(tx pgx.Tx) *Store {
	c := *s
	c.tx = tx
	return &c
}

// run executes fn in the bound transaction or in a new one.
func (s *Store) run(ctx context.Context, fn func(q Querier) error) error {
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
func (s *Store) read(ctx context.Context, fn func(q Querier) error) error {
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

func (s *Store) groupKind(kind string) (kinds.GroupKind, error) {
	gk, ok := s.cfg.GroupKinds[kind]
	if !ok {
		return gk, fmt.Errorf("%w: group kind %q is not configured", ErrInvalid, kind)
	}
	return gk, nil
}

func validateID(id GroupID) error {
	if err := contentref.ValidateID(string(id)); err != nil {
		return fmt.Errorf("%w: group id: %w", ErrInvalid, err)
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

func uniqueIDs(ids []GroupID) ([]string, error) {
	seen := map[GroupID]struct{}{}
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
