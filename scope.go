package contentkit

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/media/layout"
)

// TenantID identifies the business owning an account directory and its apps.
// It is distinct from an OpenRails merchant ID and from any app ID.
type TenantID uuid.UUID

// AppID identifies one tenant-owned catalog and its app-specific activity.
type AppID uuid.UUID

// NewTenantID and NewAppID generate IDs for provisioning. Generate once and
// persist them; runtime startup must not generate new ownership identities.
func NewTenantID() TenantID { return TenantID(uuid.New()) }
func NewAppID() AppID       { return AppID(uuid.New()) }

func (id TenantID) String() string { return uuid.UUID(id).String() }
func (id AppID) String() string    { return uuid.UUID(id).String() }
func (id TenantID) IsZero() bool   { return uuid.UUID(id) == uuid.Nil }
func (id AppID) IsZero() bool      { return uuid.UUID(id) == uuid.Nil }

// ParseTenantID accepts a nonzero canonical lowercase UUID.
func ParseTenantID(s string) (TenantID, error) {
	id, err := parseScopeID(s, "tenant")
	return TenantID(id), err
}

// ParseAppID accepts a nonzero canonical lowercase UUID.
func ParseAppID(s string) (AppID, error) {
	id, err := parseScopeID(s, "app")
	return AppID(id), err
}

func parseScopeID(s, kind string) (uuid.UUID, error) {
	id, err := uuid.Parse(s)
	if err != nil || id == uuid.Nil || id.String() != s {
		return uuid.Nil, fmt.Errorf("contentkit: %s ID must be a nonzero canonical lowercase UUID", kind)
	}
	return id, nil
}

func (id TenantID) MarshalText() ([]byte, error) { return []byte(id.String()), nil }
func (id AppID) MarshalText() ([]byte, error)    { return []byte(id.String()), nil }

func (id *TenantID) UnmarshalText(text []byte) error {
	parsed, err := ParseTenantID(string(text))
	if err == nil {
		*id = parsed
	}
	return err
}

func (id *AppID) UnmarshalText(text []byte) error {
	parsed, err := ParseAppID(string(text))
	if err == nil {
		*id = parsed
	}
	return err
}

// Scope pins a runtime to a business and one of its apps. It identifies a
// trusted deployment context; it does not grant a request any permissions.
type Scope struct {
	TenantID TenantID `json:"tenant_id" yaml:"tenant_id"`
	AppID    AppID    `json:"app_id" yaml:"app_id"`
}

func (s Scope) Validate() error {
	if s.TenantID.IsZero() || s.AppID.IsZero() {
		return errors.New("contentkit: Scope requires nonzero TenantID and AppID")
	}
	return nil
}

func (c Config) withScope() (Config, *Scope, error) {
	if c.Scope == nil {
		return c, nil, errors.New("contentkit: Config.Scope is required")
	}
	scope := *c.Scope
	if err := scope.Validate(); err != nil {
		return c, nil, err
	}
	c.Scope = &scope
	if c.Accounts == nil {
		c.Accounts = &AccountsConfig{Schema: "contentkit_accounts"}
	}
	if c.Media != nil {
		mc := *c.Media
		if mc.Namespace != "" {
			return c, nil, errors.New("contentkit: scoped media derives its root; remove Media.Namespace")
		}
		wanted := layout.Scope{TenantID: scope.TenantID.String(), AppID: scope.AppID.String()}
		if !mc.Scope.IsZero() && mc.Scope != wanted {
			return c, nil, errors.New("contentkit: Media.Scope must be derived from Config.Scope")
		}
		mc.Scope = wanted
		c.Media = &mc
	}
	return c, &scope, nil
}

// ErrScopeConflict means a catalog key or app is already bound differently.
var ErrScopeConflict = errors.New("contentkit: app ownership binding conflicts with configured scope")

// Scope returns the immutable ownership identity.
// A zero scope never means all tenants or all apps.
func (e *Engine) Scope() (Scope, bool) {
	if e.scope == nil {
		return Scope{}, false
	}
	return *e.scope, true
}

func (e *Engine) accountStorage() (schema, namespace string) {
	return e.cfg.Accounts.Schema, e.accountMediaPartition()
}

// bindScope serializes app ownership registration and checks in the catalog
// schema, and initializes the tenant account event stream in the same transaction.
// Constraints also protect concurrent inserts outside this process. This is
// embedded deployment validation, not a hosted provisioning authorization API.
func (e *Engine) bindScope(ctx context.Context) error { return e.scopeBindings(ctx, true) }

func (e *Engine) checkScope(ctx context.Context) error { return e.scopeBindings(ctx, false) }

func (e *Engine) scopeBindings(ctx context.Context, write bool) error {
	options := pgx.TxOptions{}
	if !write {
		options.AccessMode = pgx.ReadOnly
	}
	tx, err := e.pool.BeginTx(ctx, options)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if write {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "contentkit:app-bindings:"+e.schema); err != nil {
			return err
		}
	}
	table := pgx.Identifier{e.schema, "content_app_bindings"}.Sanitize()
	var tenant, app, key string
	err = tx.QueryRow(ctx, `SELECT tenant_id::text, app_id::text, catalog_key FROM `+table+` WHERE app_id = $1 OR catalog_key = $2`,
		e.scope.AppID.String(), e.cfg.Scope.AppID.String()).Scan(&tenant, &app, &key)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		if !write {
			return errors.New("contentkit: app scope binding is missing")
		}
		_, err = tx.Exec(ctx, `INSERT INTO `+table+` (tenant_id, app_id, catalog_key) VALUES ($1, $2, $3)`,
			e.scope.TenantID.String(), e.scope.AppID.String(), e.cfg.Scope.AppID.String())
	case err != nil:
		return err
	case tenant != e.scope.TenantID.String() || app != e.scope.AppID.String() || key != e.cfg.Scope.AppID.String():
		return ErrScopeConflict
	}
	if err != nil {
		return err
	}
	if write {
		// Initialize the tenant account stream so reads before the first
		// processing event have a stable cursor.
		accountSchema, _ := e.accountStorage()
		if _, err := tx.Exec(ctx, `INSERT INTO `+pgx.Identifier{accountSchema, "eventlog_streams"}.Sanitize()+` (source,scope) VALUES('contentkit',$1) ON CONFLICT DO NOTHING`, e.accountMediaPartition()); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
