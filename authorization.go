package contentkit

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/authkit/auth"
	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/entities"
	"github.com/open-rails/contentkit/internal/gate"
)

// PermissionTarget identifies an operation in an engine's fixed tenant/app.
// An empty EntityID addresses app staff authority; a nonempty EntityID addresses
// a content entity's stored authorization scope. It is not an app selector.
type PermissionTarget struct {
	Permission string
	EntityID   string
}

func (e *Engine) validateEntityScopes(provider entities.Scopes) error {
	if e.scope == nil {
		return nil
	}
	for _, kind := range e.cfg.Content.EntityKinds {
		if !kind.AuthScope {
			continue
		}
		bound, ok := provider.(entities.AppBinding)
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
// Implementations must reject foreign issuers and credentials bound to another
// app. ResolveActor returns the stable source actor for audit. Implementations
// admitting cross-tenant operators must also implement TenantAccountAuthorization
// so a source actor never becomes an implicit target-tenant audience account.
// Ordinary accounts need no app registration or membership record.
//
// Authorize checks effective authority for the explicit target tenant/app,
// including the provider's registered root grants. A entity grant must belong
// to this app. Preserve credential ceilings, revocation and binding.
// Errors never authorize an operation. Implementations must be concurrent-safe.
type Authorization interface {
	ResolveActor(ctx context.Context, scope Scope, verified auth.Verified) (string, error)
	Authorize(ctx context.Context, scope Scope, verified auth.Verified, target PermissionTarget) (bool, error)
}

// PublishingAuthorization is an optional app policy for a durable publishing
// request. CapturePublishing accepts only the provider's verified credential
// and returns nonsecret delegation data. ResumePublishing rechecks its live
// authority; it never trusts an identity decoded from JSON or stores a bearer
// token. Providers without this capability still support preparing drafts.
type PublishingAuthorization interface {
	CapturePublishing(context.Context, Scope, auth.Verified) (json.RawMessage, error)
	ResumePublishing(context.Context, Scope, json.RawMessage) (auth.Verified, error)
}

type publishingAuthority struct{ e *Engine }

type publishingDelegation struct {
	App        string          `json:"app"`
	Host       bool            `json:"host,omitempty"`
	Credential json.RawMessage `json:"credential,omitempty"`
}

func (a publishingAuthority) Capture(ctx context.Context, actor access.Actor) (json.RawMessage, error) {
	d := publishingDelegation{App: a.scopeKey()}
	if gate.IsHost(actor) {
		d.Host = true
		return json.Marshal(d)
	}
	scoped, ok := a.e.auth.(scopeAuthenticator)
	if !ok || actor.Verified == nil {
		return nil, auth.ErrForbidden
	}
	p, ok := scoped.policy.(PublishingAuthorization)
	if !ok {
		return nil, auth.ErrForbidden
	}
	var err error
	d.Credential, err = p.CapturePublishing(ctx, scoped.scope, actor.Verified)
	if err != nil {
		return nil, err
	}
	if len(d.Credential) == 0 {
		return nil, auth.ErrForbidden
	}
	return json.Marshal(d)
}

func (a publishingAuthority) Resume(ctx context.Context, raw json.RawMessage) (access.Actor, error) {
	var d publishingDelegation
	if err := json.Unmarshal(raw, &d); err != nil || d.App != a.scopeKey() {
		return access.Actor{}, auth.ErrForbidden
	}
	if d.Host {
		if len(d.Credential) != 0 {
			return access.Actor{}, auth.ErrForbidden
		}
		return gate.Host(), nil
	}
	scoped, ok := a.e.auth.(scopeAuthenticator)
	if !ok || len(d.Credential) == 0 {
		return access.Actor{}, auth.ErrForbidden
	}
	p, ok := scoped.policy.(PublishingAuthorization)
	if !ok {
		return access.Actor{}, auth.ErrForbidden
	}
	v, err := p.ResumePublishing(ctx, scoped.scope, d.Credential)
	if err != nil {
		return access.Actor{}, err
	}
	if v == nil {
		return access.Actor{}, auth.ErrForbidden
	}
	id, err := scoped.ResolveActor(ctx, v)
	if err != nil {
		return access.Actor{}, err
	}
	identity := v.Identity()
	if id == "" || identity.SubjectKind != auth.SubjectApplication {
		return access.Actor{}, auth.ErrForbidden
	}
	return access.Actor{ID: id, Kind: identity.SubjectKind, Verified: v, Invoker: identity.Invoker}, nil
}

func (a publishingAuthority) scopeKey() string {
	if a.e.scope != nil {
		return a.e.scope.TenantID.String() + "/" + a.e.scope.AppID.String()
	}
	return "legacy:" + a.e.schema + "/" + a.e.cfg.Tenant
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
		err := a.engine.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM `+pgx.Identifier{a.engine.schema, "content_entities"}.Sanitize()+` WHERE tenant_id=$1 AND auth_scope=$2 AND state='active')`, a.engine.cfg.Tenant, entity).Scan(&local)
		if err != nil {
			return false, err
		}
		if !local {
			return false, nil
		}
	}
	return a.policy.Authorize(ctx, a.scope, v, PermissionTarget{Permission: permission, EntityID: entity})
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
