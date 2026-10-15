package contentkit

import (
	"context"
	"errors"
	"net/http"
	"reflect"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/authkit/auth"
	"github.com/open-rails/contentkit/groups"
	"github.com/open-rails/contentkit/internal/gate"
)

// PermissionTarget identifies an operation in an engine's fixed tenant/app.
// An empty GroupID addresses app staff authority; a nonempty GroupID addresses
// a content group's stored authorization scope. It is not an app selector.
type PermissionTarget struct {
	Permission string
	GroupID    string
}

func (e *Engine) validateGroupScopes(provider groups.Scopes) error {
	if e.scope == nil {
		return nil
	}
	for _, kind := range e.cfg.Content.GroupKinds {
		if !kind.AuthScope {
			continue
		}
		bound, ok := provider.(groups.AppBinding)
		if !ok {
			return errors.New("contentkit: scoped authorization groups require an app-bound Scopes provider")
		}
		tenant, app := bound.ContentKitScope()
		if tenant != e.scope.TenantID.String() || app != e.scope.AppID.String() {
			return errors.New("contentkit: Scopes provider belongs to another tenant or app")
		}
	}
	return nil
}

// Authorization binds a verified credential to one tenant and app. AuthKit
// (or the host's identity provider) remains responsible for credentials and
// live sessions. This interface never receives an unverified request identity.
//
// Implementations must reject foreign issuers, unrelated tenant accounts and
// credentials bound to another app. ResolveActor returns the stable tenant
// account ID for a person, or the scoped service identity for an application.
// Ordinary accounts need no app registration or membership record.
//
// Authorize checks an explicit app grant, or an explicitly configured tenant
// administrator grant covering that app. A group grant must belong to this
// app. Preserve the verified credential's ceilings, revocation and binding;
// a broad identity-provider root role is not an implicit tenant administrator.
// Errors never authorize an operation. Implementations must be concurrent-safe.
type Authorization interface {
	ResolveActor(ctx context.Context, scope Scope, verified auth.Verified) (string, error)
	Authorize(ctx context.Context, scope Scope, verified auth.Verified, target PermissionTarget) (bool, error)
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

func (a scopeAuthenticator) Authorize(ctx context.Context, v auth.Verified, group, permission string) (bool, error) {
	if group != "" {
		if a.engine == nil || a.engine.pool == nil {
			return false, auth.ErrUnavailable
		}
		var local bool
		err := a.engine.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM `+pgx.Identifier{a.engine.schema, "content_groups"}.Sanitize()+` WHERE tenant_id=$1 AND auth_scope=$2 AND state='active')`, a.engine.cfg.Tenant, group).Scan(&local)
		if err != nil {
			return false, err
		}
		if !local {
			return false, nil
		}
	}
	return a.policy.Authorize(ctx, a.scope, v, PermissionTarget{Permission: permission, GroupID: group})
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
