package authkit

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/open-rails/authkit/auth"
	"github.com/open-rails/authkit/iam"
	"github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/access"
)

// AccountResolver maps a provider-verified identity to an existing account in
// the requested tenant. It must reject unrelated issuers and tenants; matching
// email or subject text alone does not establish an association.
type AccountResolver func(context.Context, contentkit.TenantID, auth.Identity) (string, error)

// GroupResolver resolves only a group's authorization scope owned by this app.
// Return auth.ErrForbidden for a missing group or one belonging to another app.
type GroupResolver func(context.Context, contentkit.Scope, string) (auth.Scope, error)

// AuthorizationConfig binds one ContentKit app to its identity directory and
// AuthKit permission group. These values come from trusted provisioning, never
// request headers. It creates no groups, accounts or role assignments.
type AuthorizationConfig struct {
	Scope contentkit.Scope
	// Issuer identifies the trusted tenant account directory. For a resource
	// server it can differ from the authority that owns the permission groups.
	Issuer string
	// PermissionAuthority owns AppScope, TenantAdminScope and content group
	// scopes. Empty preserves the same-issuer behavior by using Issuer.
	PermissionAuthority string
	AppScope            auth.Scope
	// TenantAdminScope is optional, explicit tenant administration. AuthKit's
	// deployment-wide root role is never consulted implicitly.
	TenantAdminScope auth.Scope
	Accounts         AccountResolver
	// Groups overrides deterministic GroupScopeID resolution only for an
	// explicitly verified legacy binding. New apps use AppScopes and omit it.
	Groups GroupResolver
}

type authorization struct{ cfg AuthorizationConfig }

// AuthorizationGroups reads the AuthKit groups used by the binding. A
// *authkit.Client satisfies it; no credential or role mutation is needed.
type AuthorizationGroups interface {
	Group(context.Context, iam.GroupRef) (iam.Group, error)
	Scope(context.Context, iam.GroupRef) (auth.Scope, error)
}

// NewAuthorization returns an app-bound ContentKit policy. The authenticator
// must produce DirectPermissionChecker credentials (AuthKit CanDirect) so app
// grants cannot be inherited from a deployment-wide root role.
func NewAuthorization(ctx context.Context, groups AuthorizationGroups, cfg AuthorizationConfig) (contentkit.Authorization, error) {
	if err := cfg.Scope.Validate(); err != nil {
		return nil, err
	}
	if cfg.PermissionAuthority == "" {
		cfg.PermissionAuthority = cfg.Issuer
	}
	if cfg.Issuer == "" || strings.TrimSpace(cfg.Issuer) != cfg.Issuer || cfg.PermissionAuthority == "" || strings.TrimSpace(cfg.PermissionAuthority) != cfg.PermissionAuthority || cfg.AppScope.Authority != cfg.PermissionAuthority || cfg.AppScope.ID == "" {
		return nil, errors.New("contentkit/authkit: identity issuer and matching app permission authority are required")
	}
	if cfg.Accounts == nil {
		return nil, errors.New("contentkit/authkit: verified tenant account resolver is required")
	}
	if cfg.TenantAdminScope != (auth.Scope{}) && (cfg.TenantAdminScope.Authority != cfg.PermissionAuthority || cfg.TenantAdminScope.ID == "" || cfg.TenantAdminScope == cfg.AppScope) {
		return nil, errors.New("contentkit/authkit: tenant administration requires a distinct scope of the configured permission authority")
	}
	if groups == nil {
		return nil, errors.New("contentkit/authkit: authorization group reader is required")
	}
	if err := validateAuthorizationGroup(ctx, groups, cfg.AppScope, "contentkit"); err != nil {
		return nil, err
	}
	if cfg.TenantAdminScope != (auth.Scope{}) {
		if err := validateAuthorizationGroup(ctx, groups, cfg.TenantAdminScope, "contentkit-tenant"); err != nil {
			return nil, err
		}
	}
	return authorization{cfg}, nil
}

func validateAuthorizationGroup(ctx context.Context, groups AuthorizationGroups, scope auth.Scope, persona string) error {
	ref := iam.GroupByID(scope.ID)
	g, err := groups.Group(ctx, ref)
	if err != nil {
		return err
	}
	resolved, err := groups.Scope(ctx, ref)
	if err != nil {
		return err
	}
	if g.ID != scope.ID || g.DeletedAt != nil || g.Persona.String() != persona || resolved != scope {
		return errors.New("contentkit/authkit: authorization group does not match the expected live persona and issuer")
	}
	return nil
}

// IssuerAccounts binds one entire, dedicated issuer account directory to a
// tenant, retaining its existing subject IDs. Use a different resolver when
// one issuer contains accounts from multiple tenants or subject IDs differ.
func IssuerAccounts(tenant contentkit.TenantID, issuer string) AccountResolver {
	return func(_ context.Context, requested contentkit.TenantID, id auth.Identity) (string, error) {
		if tenant.IsZero() || requested != tenant || issuer == "" || id.Issuer != issuer || id.Subject == "" || id.SubjectKind != auth.SubjectUser {
			return "", auth.ErrForbidden
		}
		return id.Subject, nil
	}
}

// AppPermission and TenantPermission are the AuthKit persona permissions for
// a ContentKit staff operation. Declare the returned names in the respective
// app and tenant personas, not in AuthKit's root persona.
func AppPermission(p access.Permission) string { return staffPermission("contentkit", p.String()) }
func TenantPermission(p access.Permission) string {
	return staffPermission("contentkit-tenant", p.String())
}
func staffPermission(persona, permission string) string {
	for _, known := range access.Permissions() {
		if known.String() == permission {
			return persona + ":" + strings.TrimPrefix(permission, "root:")
		}
	}
	return ""
}

func (a authorization) ResolveActor(ctx context.Context, scope contentkit.Scope, v auth.Verified) (string, error) {
	if err := a.admit(scope, v); err != nil {
		return "", err
	}
	id := v.Identity()
	if id.SubjectKind == auth.SubjectApplication {
		return id.Subject, nil
	}
	account, err := a.cfg.Accounts(ctx, scope.TenantID, id)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(account) == "" {
		return "", auth.ErrForbidden
	}
	return account, nil
}

func (a authorization) admit(scope contentkit.Scope, v auth.Verified) error {
	if scope != a.cfg.Scope || v == nil {
		return auth.ErrForbidden
	}
	id := v.Identity()
	if id.Issuer != a.cfg.Issuer || id.Subject == "" {
		return auth.ErrForbidden
	}
	bound := auth.Scope{}
	if b, ok := v.(auth.Bound); ok {
		bound = b.BoundScope()
	}
	if bound != (auth.Scope{}) && bound != a.cfg.AppScope && bound != a.cfg.TenantAdminScope {
		return auth.ErrForbidden
	}
	if id.SubjectKind == auth.SubjectApplication && (bound == (auth.Scope{}) || bound != a.cfg.AppScope && bound != a.cfg.TenantAdminScope) {
		return auth.ErrForbidden
	}
	if id.SubjectKind != auth.SubjectUser && id.SubjectKind != auth.SubjectApplication {
		return auth.ErrForbidden
	}
	return nil
}

func (a authorization) Authorize(ctx context.Context, scope contentkit.Scope, v auth.Verified, target contentkit.PermissionTarget) (bool, error) {
	if _, err := a.ResolveActor(ctx, scope, v); err != nil {
		return false, err
	}
	checker, ok := v.(auth.DirectPermissionChecker)
	if !ok {
		return false, nil
	}
	if target.GroupID != "" {
		var group auth.Scope
		var err error
		if a.cfg.Groups != nil {
			group, err = a.cfg.Groups(ctx, scope, target.GroupID)
		} else {
			group.ID, err = GroupScopeID(scope, target.GroupID)
			group.Authority = a.cfg.PermissionAuthority
		}
		if err != nil {
			return false, err
		}
		if group.ID == "" || group.Authority != a.cfg.PermissionAuthority {
			return false, auth.ErrForbidden
		}
		return checker.CanDirect(ctx, group, target.Permission)
	}
	permission := staffPermission("contentkit", target.Permission)
	if permission == "" {
		return false, nil
	}
	var allowed bool
	var err error
	// Avatars belong to the tenant account directory. App moderation grants
	// must not become permission to edit shared profiles in every app.
	if target.Permission != access.AvatarsWrite.String() {
		allowed, err = checker.CanDirect(ctx, a.cfg.AppScope, permission)
	}
	if err != nil || allowed || a.cfg.TenantAdminScope == (auth.Scope{}) {
		return allowed, err
	}
	return checker.CanDirect(ctx, a.cfg.TenantAdminScope, staffPermission("contentkit-tenant", target.Permission))
}

// GroupScopeID is a deterministic AuthKit group identity for a ContentKit
// group in one tenant and app. Existing content IDs and stored auth_scope
// references stay unchanged; only the provider's authorization ID is derived.
func GroupScopeID(scope contentkit.Scope, groupID string) (string, error) {
	if err := scope.Validate(); err != nil {
		return "", err
	}
	if groupID == "" || strings.TrimSpace(groupID) != groupID {
		return "", errors.New("contentkit/authkit: group ID is required")
	}
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte("contentkit:group:"+scope.TenantID.String()+":"+scope.AppID.String()+":"+groupID)).String(), nil
}
