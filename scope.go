package contentkit

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/access"
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

// withScope resolves public ownership identity to the established catalog key.
// The returned private config uses the legacy runtime fields; clearing the
// public declaration makes further normalization idempotent. The caller keeps
// the returned scope separately. No durable content or media key is rewritten.
func (c Config) withScope() (Config, *Scope, error) {
	if c.Scope == nil {
		if c.LegacyCatalogKey != "" {
			return c, nil, errors.New("contentkit: LegacyCatalogKey requires Scope")
		}
		return c, nil, nil
	}
	if c.Notifications != nil {
		return c, nil, errors.New("contentkit: scoped engines use Accounts rather than legacy Notifications configuration")
	}
	if c.Tenant != "" {
		return c, nil, errors.New("contentkit: configure Scope or legacy Tenant, not both; use LegacyCatalogKey with Scope")
	}
	scope := *c.Scope
	if err := scope.Validate(); err != nil {
		return c, nil, err
	}
	key := c.LegacyCatalogKey
	if key == "" {
		key = scope.AppID.String()
	}
	if strings.TrimSpace(key) != key || !access.ValidTenant(key) {
		return c, nil, errors.New("contentkit: LegacyCatalogKey must match [a-z0-9_-]{1,64}")
	}
	if c.Accounts == nil && c.Notifications == nil {
		c.Accounts = &AccountsConfig{Schema: "contentkit_accounts"}
	}
	if c.Accounts != nil {
		accounts := *c.Accounts
		if accounts.Namespace != "" {
			return c, nil, errors.New("contentkit: scoped Accounts.Namespace is derived; use MigrationOptions.LegacyAccountKey to adopt existing storage")
		}
		if accounts.Namespace == "" {
			accounts.Namespace = (layout.Scope{TenantID: scope.TenantID.String()}).Partition()
		}
		c.Accounts = &accounts
	}
	if c.Media != nil {
		mc := *c.Media
		if mc.Namespace != "" && mc.Namespace != key {
			return c, nil, errors.New("contentkit: legacy Media.Namespace must match LegacyCatalogKey during adoption")
		}
		mc.Namespace = ""
		wanted := layout.Scope{TenantID: scope.TenantID.String(), AppID: scope.AppID.String()}
		if !mc.Scope.IsZero() && mc.Scope != wanted {
			return c, nil, errors.New("contentkit: Media.Scope must be derived from Config.Scope")
		}
		mc.Scope = wanted
		c.Media = &mc
	}
	c.Tenant, c.Scope, c.LegacyCatalogKey = key, nil, ""
	return c, &scope, nil
}

// ErrScopeConflict means a catalog key or app is already bound differently,
// or an unscoped runtime is trying to open a catalog that has adopted Scope.
var ErrScopeConflict = errors.New("contentkit: app ownership binding conflicts with configured scope")

// Scope returns the immutable ownership identity, or false for a legacy-only
// runtime. A zero scope never means all tenants or all apps.
func (e *Engine) Scope() (Scope, bool) {
	if e.scope == nil {
		return Scope{}, false
	}
	return *e.scope, true
}

func (e *Engine) accountStorage() (schema, namespace string) {
	if n := e.cfg.Notifications; n != nil {
		return n.Schema, n.Namespace
	}
	return e.schema, e.cfg.Tenant
}

// bindScope serializes registration and checks within the catalog and account
// schemas. Both claims commit together; an account conflict cannot bind an app.
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
	if e.scope == nil {
		var bound bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM `+table+` WHERE catalog_key = $1)`, e.cfg.Tenant).Scan(&bound); err != nil {
			return err
		}
		if bound {
			return fmt.Errorf("%w: catalog requires Scope", ErrScopeConflict)
		}
	} else {
		var tenant, app, key string
		err = tx.QueryRow(ctx, `SELECT tenant_id::text, app_id::text, catalog_key FROM `+table+` WHERE app_id = $1 OR catalog_key = $2`,
			e.scope.AppID.String(), e.cfg.Tenant).Scan(&tenant, &app, &key)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			if !write {
				return errors.New("contentkit: app scope binding is missing")
			}
			_, err = tx.Exec(ctx, `INSERT INTO `+table+` (tenant_id, app_id, catalog_key) VALUES ($1, $2, $3)`,
				e.scope.TenantID.String(), e.scope.AppID.String(), e.cfg.Tenant)
		case err != nil:
			return err
		case tenant != e.scope.TenantID.String() || app != e.scope.AppID.String() || key != e.cfg.Tenant:
			return ErrScopeConflict
		}
		if err != nil {
			return err
		}
	}
	accountSchema, namespace := e.accountStorage()
	if write {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "contentkit:account-bindings:"+accountSchema); err != nil {
			return err
		}
	}
	accountTable := pgx.Identifier{accountSchema, "content_account_bindings"}.Sanitize()
	var tenant, storedNamespace string
	if e.scope == nil {
		err = tx.QueryRow(ctx, `SELECT tenant_id::text, account_namespace FROM `+accountTable+` WHERE account_namespace = $1`, namespace).Scan(&tenant, &storedNamespace)
	} else {
		err = tx.QueryRow(ctx, `SELECT tenant_id::text, account_namespace FROM `+accountTable+` WHERE account_namespace = $1 OR tenant_id = $2`,
			namespace, e.scope.TenantID.String()).Scan(&tenant, &storedNamespace)
	}
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		if e.scope != nil {
			if !write {
				return errors.New("contentkit: tenant account binding is missing")
			}
			_, err = tx.Exec(ctx, `INSERT INTO `+accountTable+` (account_namespace, tenant_id) VALUES ($1, $2)`, namespace, e.scope.TenantID.String())
		} else {
			err = nil
		}
	case err != nil:
		return err
	case e.scope == nil || tenant != e.scope.TenantID.String() || storedNamespace != namespace:
		return fmt.Errorf("%w: account storage belongs to a registered tenant", ErrScopeConflict)
	}
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// resolveAccountKey follows the immutable tenant binding before opening inbox
// stores. Missing bindings remain the explicit migrator's responsibility.
func (e *Engine) resolveAccountKey(ctx context.Context) error {
	if e.scope == nil {
		return nil
	}
	schema, _ := e.accountStorage()
	var key string
	err := e.pool.QueryRow(ctx, "SELECT account_namespace FROM "+pgx.Identifier{schema, "content_account_bindings"}.Sanitize()+" WHERE tenant_id=$1", e.scope.TenantID.String()).Scan(&key)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	e.cfg.Notifications.Namespace = key
	return nil
}
