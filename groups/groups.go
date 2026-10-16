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
	ErrInvalid   = errors.New("groups: invalid input")
	ErrNotFound  = errors.New("groups: not found")
	ErrConflict  = errors.New("groups: conflict")
	ErrForbidden = errors.New("groups: forbidden")
	// ErrLimit is a person owning as many groups of a kind as it allows
	// (kinds.GroupKind.MaxPerOwner).
	ErrLimit = errors.New("groups: limit reached")
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
// ("i-{uuid}") or a member content's cover, the cover of the edition a
// reader's language opens by default. Auto covers are the most-favorited live
// member, chosen by Recount when none is selected.
type Cover struct {
	Image     string `json:"image,omitempty"`
	ContentID string `json:"content_id,omitempty"`
	Auto      bool   `json:"auto,omitempty"`
	// Renditions are its public images, filled on reads (Options.Covers).
	Renditions []Rendition `json:"renditions,omitempty"`
}

// Avatar is a user-owned group's square picture: an image uploaded to the
// group's media folder ("avatar/i-{uuid}"), selected by name. Without a
// selection a read shows the folder's first avatar, else the kind's default.
type Avatar struct {
	Image string `json:"image,omitempty"`
	// Renditions are its public images, filled on reads (Options.Covers).
	Renditions []Rendition `json:"renditions,omitempty"`
}

// Rendition is one public image of a cover, as media.PublicRendition.
type Rendition struct {
	URL string `json:"url"`
	W   int    `json:"w"`
	H   int    `json:"h"`
}

// CoverResolver turns covers and avatars into their public renditions, per
// picture in order, one call per page: media/versions.Service.GroupCovers.
type CoverResolver interface {
	Covers(ctx context.Context, covers []CoverOf) ([][]Rendition, error)
}

// CoverOf is one group's picture to resolve: its cover, or with Preset
// "avatar" its avatar (Cover.Image the selected one, if any). A content's
// cover is its default edition's in Language.
type CoverOf struct {
	Kind     string
	ID       GroupID
	Cover    Cover
	Language string
	Preset   string
}

// Group is one group record.
type Group struct {
	ID   GroupID `json:"id"`
	Kind string  `json:"kind"`
	Slug string  `json:"slug"`
	// Internal marks a classification used by policy rather than public metadata.
	Internal bool `json:"internal"`
	// ScopeID is the group the slug is unique within (a character's series).
	ScopeID     GroupID        `json:"scope_id,omitempty"`
	ParentID    GroupID        `json:"parent_id,omitempty"`
	State       State          `json:"state"`
	MergedInto  GroupID        `json:"merged_into,omitempty"`
	Attributes  map[string]any `json:"attributes"`
	Cover       *Cover         `json:"cover,omitempty"`
	Avatar      *Avatar        `json:"avatar,omitempty"`
	OwnerUserID string         `json:"owner_user_id,omitempty"`
	// AuthScope is the AuthKit group whose holders of
	// access.GroupSettingsManage edit it; ContentKit provisions it for a
	// kind with auth_scope (Options.Scopes).
	AuthScope string    `json:"auth_scope,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Revision  int64     `json:"revision"`
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
	// ContentChanged runs in the writing transaction whenever the effective
	// groups of contents may have changed (their facets' group_ids are
	// already re-derived), so the engine can rebuild what else depends on
	// them, such as search documents. Optional.
	ContentChanged func(ctx context.Context, q Querier, contentIDs []string) error
	// DocumentsChanged runs in the writing transaction after a write queued
	// search documents, so the engine can sync them at once. Optional.
	DocumentsChanged func(ctx context.Context, q Querier) error
	// Covers fills covers' and avatars' renditions on reads. Optional.
	Covers CoverResolver
	// Sales fills sellable groups' offers and the viewer's standing on
	// reads: offers.Service. Optional.
	Sales Sales
	// Scopes provisions and retires the AuthKit scopes of auth_scope kinds,
	// and lists a person's. Optional: without it auth_scope is whatever staff
	// set.
	Scopes Scopes
	// Folders hides a deleted group's media and shows it again on restore,
	// in the writing transaction, and purges a user-owned group's once it
	// stayed deleted for DeletedRetention: media/versions.Service.GroupFolders.
	// Optional.
	Folders Folders
	// DeletedRetention is how long a deleted user-owned group keeps its media
	// for a restore; zero is DefaultDeletedRetention.
	DeletedRetention time.Duration
	// Owned deletes the content a deleted user-owned group owns
	// (owner_group) and restores it with the group: the catalog. Optional.
	Owned OwnedContent
	// Queue is the River queue of group syncs; contentkit_groups when empty.
	Queue string
	// Logger receives the HTTP API's failures. nil -> slog.Default().
	Logger *slog.Logger
}

// DefaultDeletedRetention is how long a deleted user-owned group keeps its
// media folder, so a restore brings it back.
const DefaultDeletedRetention = 30 * 24 * time.Hour

// OwnedContent follows user-owned groups' deletion and restore with the
// content they own (owner_group): the catalog. A nil groups is every group.
type OwnedContent interface {
	// DeleteOwned deletes the live content of the deleted ones among groups,
	// recording each group as its content's cause; bought content stays
	// readable to its buyers.
	DeleteOwned(ctx context.Context, groups []string) error
	// RestoreOwned restores the content the active ones among groups
	// deleted when they were, unless it was purged since; content deleted
	// for another reason stays deleted.
	RestoreOwned(ctx context.Context, groups []string) error
}

// Store is one tenant's groups.
type Store struct {
	pool      *pgxpool.Pool
	tx        pgx.Tx
	qs        string
	schema    string
	tenant    string
	cfg       kinds.Config
	changed   func(ctx context.Context, q Querier, contentIDs []string) error
	documents func(ctx context.Context, q Querier) error
	covers    CoverResolver
	sales     Sales
	scopes    Scopes
	folders   Folders
	keep      time.Duration
	owned     OwnedContent
	queue     string
	log       *slog.Logger
	as        *writer
	river     *riverBinding
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
	queue := opts.Queue
	if queue == "" {
		queue = "contentkit_groups"
	}
	keep := opts.DeletedRetention
	if keep <= 0 {
		keep = DefaultDeletedRetention
	}
	return &Store{pool: opts.Pool, qs: qs, schema: strings.TrimSpace(opts.Schema), tenant: tenant, cfg: opts.Kinds, changed: opts.ContentChanged, documents: opts.DocumentsChanged,
		covers: opts.Covers, sales: opts.Sales, scopes: opts.Scopes, folders: opts.Folders, keep: keep, owned: opts.Owned, queue: queue, log: log,
		river: &riverBinding{}}, nil
}

// Tenant returns the tenant this store is scoped to.
func (s *Store) Tenant() string { return s.tenant }

// run executes fn in the bound transaction (an import's, bound) or in a new
// one.
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
