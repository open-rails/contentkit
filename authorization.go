package contentkit

import (
	"context"
	"errors"
	"net/http"
	"reflect"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/authkit/auth"
	"github.com/open-rails/contentkit/entities"
	"github.com/open-rails/contentkit/internal/gate"
)

// PermissionTarget identifies an operation in an engine's fixed tenant/app.
// An empty ChannelID addresses app staff authority; a nonempty ChannelID addresses
// a channel's stored authorization scope. It is not an app selector.
type PermissionTarget struct {
	Permission string
	ChannelID  string
}

func (e *Engine) validateEntityScopes(provider entities.Scopes) error {
	if e.scope == nil {
		return nil
	}
	if e.cfg.Content.Channels != nil {
		return e.validateScopeBinding(provider)
	}
	return nil
}

// validateScopeBinding is shared by runtime and explicit provisioning; neither
// may use another app's authorization-group identities.
func (e *Engine) validateScopeBinding(provider entities.ScopeProvisioner) error {
	if e.scope == nil {
		return nil
	}
	bound, ok := provider.(entities.AppBinding)
	if !ok {
		return errors.New("contentkit: scoped authorization groups require an app-bound Scopes provider")
	}
	tenant, app := bound.ContentKitScope()
	if tenant != e.scope.TenantID.String() || app != e.scope.AppID.String() {
		return errors.New("contentkit: Scopes provider belongs to another tenant or app")
	}
	return nil
}

// Authorization binds a verified credential to one tenant and app. AuthKit
// (or the host's identity provider) remains responsible for credentials and
// live sessions. This interface never receives an unverified request identity.
//
// Implementations must reject foreign issuers and credentials bound to another
// app. ResolveActor returns the stable source actor for audit. Implementations
// admitting cross-tenant operators must also implement TenantAccountAuthorization
// so a source actor never becomes an implicit target-tenant audience account.
// Ordinary accounts need no app registration or membership record.
//
// Authorize checks effective authority for the explicit target tenant/app,
// including the provider's registered root grants. A channel grant must belong
// to this app. Preserve credential ceilings, revocation and binding.
// Errors never authorize an operation. Implementations must be concurrent-safe.
type Authorization interface {
	ResolveActor(ctx context.Context, scope Scope, verified auth.Verified) (string, error)
	Authorize(ctx context.Context, scope Scope, verified auth.Verified, target PermissionTarget) (bool, error)
}

// TenantAccountAuthorization separates audience membership from administrative
// authority. A nonmember returns ("", false, nil); it may still hold a root
// permission to administer the target. The account must already exist and be
// owned by scope.TenantID. Errors fail closed. Existing custom Authorization
// implementations retain their trusted ResolveActor-as-account contract until
// they opt into cross-tenant actors through this interface.
type TenantAccountAuthorization interface {
	ResolveTenantAccount(context.Context, Scope, auth.Verified) (accountID string, member bool, err error)
}

// scopeAuthenticator installs app policy without changing the original
// verified identity or losing its permission, sender or session capabilities.
// The gate keeps the mapped account ID separately from Verified.Identity.
type scopeAuthenticator struct {
	auth.Authenticator
	scope  Scope
	policy Authorization
	engine *Engine
}

var _ gate.ScopedAuthorization = scopeAuthenticator{}

func (e *Engine) authorization(a auth.Authenticator, policy Authorization) (auth.Authenticator, error) {
	if err := gate.Check(a); err != nil {
		return nil, err
	}
	if e.scope == nil {
		if !nilAuthorization(policy) {
			return nil, errors.New("contentkit: Deps.Authorization requires Config.Scope")
		}
		return a, nil
	}
	if nilAuthorization(policy) {
		return nil, errors.New("contentkit: Config.Scope requires Deps.Authorization")
	}
	return scopeAuthenticator{Authenticator: a, scope: *e.scope, policy: policy, engine: e}, nil
}

func nilAuthorization(v Authorization) bool {
	if v == nil {
		return true
	}
	r := reflect.ValueOf(v)
	switch r.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return r.IsNil()
	}
	return false
}

func (a scopeAuthenticator) ResolveActor(ctx context.Context, v auth.Verified) (string, error) {
	return a.policy.ResolveActor(ctx, a.scope, v)
}

func (a scopeAuthenticator) ResolveTenantAccount(ctx context.Context, v auth.Verified, actorID string) (string, bool, error) {
	if policy, ok := a.policy.(TenantAccountAuthorization); ok {
		return policy.ResolveTenantAccount(ctx, a.scope, v)
	}
	return actorID, actorID != "", nil
}

func (a scopeAuthenticator) Authorize(ctx context.Context, v auth.Verified, entity, permission string) (bool, error) {
	if entity != "" {
		if a.engine == nil || a.engine.pool == nil {
			return false, auth.ErrUnavailable
		}
		var local bool
		err := a.engine.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM `+pgx.Identifier{a.engine.schema, "content_entities"}.Sanitize()+` WHERE tenant_id=$1 AND kind='channel' AND auth_scope=$2 AND state='active')`, a.engine.cfg.Tenant, entity).Scan(&local)
		if err != nil {
			return false, err
		}
		if !local {
			return false, nil
		}
	}
	return a.policy.Authorize(ctx, a.scope, v, PermissionTarget{Permission: permission, ChannelID: entity})
}

// Headers preserve the underlying provider's sender-proof and challenge
// contract. Providers without this optional capability use Authorization.
func (a scopeAuthenticator) AllowedHeaders() []string {
	if h, ok := a.Authenticator.(auth.Headers); ok {
		return h.AllowedHeaders()
	}
	return []string{"Authorization"}
}

func (a scopeAuthenticator) ExposedHeaders() []string {
	if h, ok := a.Authenticator.(auth.Headers); ok {
		return h.ExposedHeaders()
	}
	return []string{"WWW-Authenticate"}
}

// Authenticate deliberately delegates once. App admission happens in the
// gate after it has validated the provider's identity and caught its faults.
func (a scopeAuthenticator) Authenticate(r *http.Request) (auth.Verified, error) {
	return a.Authenticator.Authenticate(r)
}
