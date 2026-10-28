package entitystore

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
	entities "github.com/open-rails/contentkit/entities"
	entitlement "github.com/open-rails/contentkit/internal/entitlement"
	"github.com/open-rails/contentkit/internal/search"
	"github.com/open-rails/contentkit/kinds"
)

var (
	ErrNotFound = errors.New("entities: not found")
)

var (
	ErrConflict = errors.New("entities: conflict")
)

var (
	ErrForbidden = errors.New("entities: forbidden")
)

var (
	// ErrLimit is a person owning as many entities of a kind as it allows
	// (kinds.EntityKind.MaxPerOwner).
	ErrLimit = errors.New("entities: limit reached")
)

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
	// Kinds is the site's content model; its EntityKinds govern every write.
	Kinds kinds.Config
	// ContentChanged runs whenever the effective entities of contents may have
	// changed (their facets' entity_ids are already re-derived), so the engine
	// can rebuild what else depends on them, such as search documents: in the
	// writing transaction, or for a large reach in the refresh job's batch
	// transactions. Optional.
	ContentChanged func(ctx context.Context, q Querier, contentIDs []string) error
	// NamesChanged runs in the writing transaction after the names or slugs
	// of entities changed, so the engine can requeue the documents of content
	// that shows them, such as search documents keyed by entity names. Their
	// facets are unchanged; it must not lock content. Optional.
	NamesChanged func(ctx context.Context, q Querier, entityIDs []string) error
	// DocumentsChanged runs in the writing transaction after a write queued
	// search documents, so the engine can sync them at once. Optional.
	DocumentsChanged func(ctx context.Context, q Querier) error
	// EntitiesChanged records entity events in the writing transaction: the
	// catalog event log. Optional.
	EntitiesChanged func(ctx context.Context, q Querier, changes []entities.Change) error
	// Covers fills covers' and avatars' renditions on reads. Optional.
	Covers entities.CoverResolver
	// Gate reads the viewer's entitlement-backed entity membership.
	Gate *entitlement.Gate
	// Scopes provisions and retires the AuthKit scopes of auth_scope kinds,
	// and lists a person's. Optional: without it auth_scope is whatever staff
	// set.
	Scopes entities.Scopes
	// Folders retires a user-owned entity's media after DeletedRetention and
	// confirms physical erasure before marking it purged. Irreversible claims
	// refuse restoration: media/versions.Service.EntityFolders.
	// Optional.
	Folders Folders
	// DeletedRetention is how long a deleted user-owned entity keeps its media
	// for a restore; zero is DefaultDeletedRetention.
	DeletedRetention time.Duration
	// Owned deletes the content a deleted user-owned entity owns
	// (owner_entity) and restores it with the entity: the catalog. Optional.
	Owned entities.OwnedContent
	// Logger receives the HTTP API's failures. nil -> slog.Default().
	Logger *slog.Logger
}

// Store is one tenant's entities.
type Store struct {
	channels  bool // private reserved-channel storage handle
	pool      *pgxpool.Pool
	tx        pgx.Tx
	qs        string
	schema    string
	tenant    string
	cfg       kinds.Config
	changed   func(ctx context.Context, q Querier, contentIDs []string) error
	renamed   func(ctx context.Context, q Querier, entityIDs []string) error
	documents func(ctx context.Context, q Querier) error
	recorded  func(ctx context.Context, q Querier, changes []entities.Change) error
	covers    entities.CoverResolver
	gate      *entitlement.Gate
	scopes    entities.Scopes
	folders   Folders
	keep      time.Duration
	owned     entities.OwnedContent
	log       *slog.Logger
	as        *writer
}

// New validates the options and returns the store.
func New(opts Options) (*Store, error) {
	if opts.Pool == nil {
		return nil, fmt.Errorf("%w: Pool is required", entities.ErrInvalid)
	}
	qs, err := search.QuoteSchema(opts.Schema)
	if err != nil {
		return nil, fmt.Errorf("%w: schema: %v", entities.ErrInvalid, err)
	}
	tenant := strings.TrimSpace(opts.Tenant)
	if tenant == "" {
		return nil, fmt.Errorf("%w: Tenant is required", entities.ErrInvalid)
	}
	if opts.Gate != nil && opts.Gate.Tenant() != tenant {
		return nil, fmt.Errorf("%w: Gate tenant differs from entity tenant", entities.ErrInvalid)
	}
	if err := opts.Kinds.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", entities.ErrInvalid, err)
	}
	opts.Kinds = opts.Kinds.StorageModel()
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	keep := opts.DeletedRetention
	if keep <= 0 {
		keep = entities.DefaultDeletedRetention
	}
	return &Store{pool: opts.Pool, qs: qs, schema: strings.TrimSpace(opts.Schema), tenant: tenant, cfg: opts.Kinds, changed: opts.ContentChanged, renamed: opts.NamesChanged, documents: opts.DocumentsChanged, recorded: opts.EntitiesChanged,
		covers: opts.Covers, gate: opts.Gate, scopes: opts.Scopes, folders: opts.Folders, keep: keep, owned: opts.Owned, log: log}, nil
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
		return constraintError{entities.ErrInvalid, pgErr.Code, pgErr.ConstraintName}
	}
	return err
}

func (s *Store) entityKind(kind string) (kinds.EntityKind, error) {
	gk, ok := s.cfg.EntityKinds[kind]
	if !ok || (kind == "channel") != s.channels {
		return gk, fmt.Errorf("%w: group kind %q is not configured", entities.ErrInvalid, kind)
	}
	return gk, nil
}

func validateID(id entities.EntityID) error {
	if err := contentref.ValidateID(string(id)); err != nil {
		return fmt.Errorf("%w: group id: %w", entities.ErrInvalid, err)
	}
	return nil
}

func validateSlug(slug string) (string, error) {
	slug = strings.TrimSpace(slug)
	if slug == "" || utf8.RuneCountInString(slug) > 200 || strings.ContainsAny(slug, "/ \t\n") {
		return "", fmt.Errorf("%w: slug %q must be 1-200 characters without whitespace or '/'", entities.ErrInvalid, slug)
	}
	return slug, nil
}

func normalizeLanguage(l string) (string, error) {
	l = strings.ToLower(strings.TrimSpace(l))
	if l == "" || len(l) > 16 || strings.IndexFunc(l, unicode.IsSpace) >= 0 {
		return "", fmt.Errorf("%w: language %q", entities.ErrInvalid, l)
	}
	return l, nil
}

func uniqueIDs(ids []entities.EntityID) ([]string, error) {
	seen := map[entities.EntityID]struct{}{}
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
