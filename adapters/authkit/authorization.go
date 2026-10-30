package authkit

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/open-rails/authkit/auth"
	"github.com/open-rails/authkit/iam"
	"github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/access"
)

// AccountResolver is the legacy dedicated-issuer mapping to an existing
// account in the requested tenant. Shared multi-tenant issuers use
// TenantActorResolver instead. Matching email or subject text alone does not
// establish an association.
type AccountResolver func(context.Context, contentkit.TenantID, auth.Identity) (string, error)

// TenantActorResolver returns live source-account ownership from a verified
// credential. AuthKit's Client implements it; it never grants target access.
type TenantActorResolver interface {
	ResolveTenantActor(context.Context, auth.Verified) (iam.TenantActor, error)
}

// ErrScopeMigrationRequired means persisted authorization groups still use
// the retired library-specific personas. Migrate them explicitly through
// AuthKit before adopting this adapter; startup never rewrites grants or keys.
var ErrScopeMigrationRequired = errors.New("contentkit/authkit: explicit authorization-scope migration required")

// AuthorizationConfig binds one ContentKit app to its identity directory and
// AuthKit permission group. These values come from trusted provisioning, never
// request headers. It creates no groups, accounts or role assignments.
type AuthorizationConfig struct {
	Scope contentkit.Scope
	// Issuer identifies the trusted tenant account directory. For a resource
	// server it can differ from the authority that owns the permission entities.
	Issuer string
	// PermissionAuthority owns AppScope, TenantAdminScope and content group
	// scopes. Empty preserves the same-issuer behavior by using Issuer.
	PermissionAuthority string
	AppScope            auth.Scope
	// TenantAdminScope is optional, explicit tenant administration. Native
	// effective root authority also applies, within the credential's limits.
	TenantAdminScope auth.Scope
	// TenantActors defaults to groups when it implements TenantActorResolver.
	// New multi-tenant hosts use this verified, live ownership boundary.
	TenantActors TenantActorResolver
	// Accounts is the legacy dedicated-issuer contract. Do not use it when
	// one issuer contains accounts from more than one tenant.
	Accounts AccountResolver
}

type authorization struct {
	cfg    AuthorizationConfig
	root   auth.Scope
	groups AuthorizationGroups
}

// AuthorizationGroups reads the AuthKit groups used by the binding. A
// *authkit.Client satisfies it; no credential or role mutation is needed.
type AuthorizationGroups interface {
	Group(context.Context, iam.GroupRef) (iam.Group, error)
	Scope(context.Context, iam.GroupRef) (auth.Scope, error)
}

// NewAuthorization returns an app-bound ContentKit policy. The authenticator
// must produce PermissionChecker credentials. AuthKit's native effective Can
// preserves global root grants, live membership and credential restrictions.
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
	if cfg.Accounts != nil && cfg.TenantActors != nil {
		return nil, errors.New("contentkit/authkit: configure native tenant actors or legacy accounts, not both")
	}
	if cfg.Accounts == nil && cfg.TenantActors == nil {
		cfg.TenantActors, _ = groups.(TenantActorResolver)
	}
	if cfg.Accounts == nil && cfg.TenantActors == nil {
		return nil, errors.New("contentkit/authkit: verified tenant account resolver is required")
	}
	if cfg.TenantAdminScope != (auth.Scope{}) && (cfg.TenantAdminScope.Authority != cfg.PermissionAuthority || cfg.TenantAdminScope.ID == "" || cfg.TenantAdminScope == cfg.AppScope) {
		return nil, errors.New("contentkit/authkit: tenant administration requires a distinct scope of the configured permission authority")
	}
	if groups == nil {
		return nil, errors.New("contentkit/authkit: authorization group reader is required")
	}
	if cfg.AppScope.ID != cfg.Scope.AppID.String() {
		return nil, errors.New("contentkit/authkit: app group ID must equal the canonical ContentKit app ID")
	}
	if err := validateAuthorizationGroup(ctx, groups, cfg.AppScope, "app", cfg.Scope.TenantID); err != nil {
		return nil, err
	}
	if cfg.TenantAdminScope != (auth.Scope{}) {
		if cfg.TenantAdminScope.ID != cfg.Scope.TenantID.String() {
			return nil, errors.New("contentkit/authkit: tenant group ID must equal the canonical ContentKit tenant ID")
		}
		if err := validateAuthorizationGroup(ctx, groups, cfg.TenantAdminScope, "tenant", cfg.Scope.TenantID); err != nil {
			return nil, err
		}
	}
	root, err := groups.Scope(ctx, iam.RootGroup())
	if err != nil {
		return nil, err
	}
	if root.ID == "" || root.Authority != cfg.PermissionAuthority {
		return nil, errors.New("contentkit/authkit: root scope belongs to another permission authority")
	}
	return authorization{cfg: cfg, root: root, groups: groups}, nil
}

// currentGroup prevents a deleted/purged UUID reused by another tenant from
// retargeting a running ContentKit engine. Reads are live, never cross-request
// cached. An outage is unavailable, not a reason to discard a valid login.
func (a authorization) currentGroup(ctx context.Context, scope auth.Scope, persona string) error {
	g, err := a.groups.Group(ctx, iam.GroupByID(scope.ID))
	if errors.Is(err, iam.ErrGroupNotFound) {
		return auth.ErrForbidden
	}
	if err != nil {
		return errors.Join(auth.ErrUnavailable, err)
	}
	if g.ID != scope.ID || g.DeletedAt != nil || g.TenantID.String() != a.cfg.Scope.TenantID.String() || persona != "" && g.Persona.String() != persona {
		return auth.ErrForbidden
	}
	return nil
}

func validateAuthorizationGroup(ctx context.Context, groups AuthorizationGroups, scope auth.Scope, persona string, tenant contentkit.TenantID) error {
	ref := iam.GroupByID(scope.ID)
	g, err := groups.Group(ctx, ref)
	if err != nil {
		return err
	}
	if g.Persona.String() == "contentkit" || g.Persona.String() == "contentkit-tenant" {
		return fmt.Errorf("%w: migrate group %s from persona %s before startup", ErrScopeMigrationRequired, g.ID, g.Persona)
	}
	resolved, err := groups.Scope(ctx, ref)
	if err != nil {
		return err
	}
	if g.ID != scope.ID || g.DeletedAt != nil || g.Persona.String() != persona || resolved != scope || g.TenantID.String() != tenant.String() {
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

// AppPermission is the AuthKit app permission for a ContentKit staff operation.
// A limited tenant role may include that same permission; AuthKit scopes its
// inheritance to the tenant's apps.
func AppPermission(p access.Permission) string { return staffPermission("app", p.String()) }

// TenantPermission names shared account operations. It is empty for app-only
// operations: tenant roles include AppPermission grants instead of defining a
// duplicate tenant permission. Currently only avatar administration is shared.
func TenantPermission(p access.Permission) string {
	if p != access.AvatarsWrite {
		return ""
	}
	return staffPermission("tenant", p.String())
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
	if err := a.currentGroup(ctx, a.cfg.AppScope, "app"); err != nil {
		return "", err
	}
	id := v.Identity()
	if id.SubjectKind == auth.SubjectApplication {
		return id.Subject, nil
	}
	if a.cfg.TenantActors != nil {
		actor, err := a.tenantActor(ctx, v)
		return actor.AccountID, err
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

// ResolveTenantAccount identifies a real audience account in this target
// tenant. A foreign source actor remains attributable without becoming a
// member; its staff rights are checked separately by Authorize.
func (a authorization) ResolveTenantAccount(ctx context.Context, scope contentkit.Scope, v auth.Verified) (string, bool, error) {
	if err := a.admit(scope, v); err != nil {
		return "", false, err
	}
	if err := a.currentGroup(ctx, a.cfg.AppScope, "app"); err != nil {
		return "", false, err
	}
	if v.Identity().SubjectKind != auth.SubjectUser {
		return "", false, nil
	}
	if a.cfg.TenantActors != nil {
		actor, err := a.tenantActor(ctx, v)
		if err != nil || actor.TenantID.String() != scope.TenantID.String() {
			return "", false, err
		}
		return actor.AccountID, true, nil
	}
	account, err := a.ResolveActor(ctx, scope, v)
	return account, err == nil && account != "", err
}

func (a authorization) tenantActor(ctx context.Context, v auth.Verified) (iam.TenantActor, error) {
	actor, err := a.cfg.TenantActors.ResolveTenantActor(ctx, v)
	if err != nil {
		if errors.Is(err, auth.ErrForbidden) || errors.Is(err, auth.ErrUnauthenticated) || errors.Is(err, auth.ErrUnavailable) {
			return actor, err
		}
		return actor, errors.Join(auth.ErrUnavailable, err)
	}
	id := v.Identity()
	if actor.AccountID == "" || actor.AccountID != id.Subject || actor.TenantID.IsZero() || id.TenantID != "" && id.TenantID != actor.TenantID.String() {
		return iam.TenantActor{}, auth.ErrForbidden
	}
	return actor, nil
}

func (a authorization) admit(scope contentkit.Scope, v auth.Verified) error {
	if scope != a.cfg.Scope || v == nil {
		return auth.ErrForbidden
	}
	id := v.Identity()
	if id.Issuer != a.cfg.Issuer || id.Subject == "" {
		return auth.ErrForbidden
	}
	// A legacy dedicated-issuer mapping cannot override an explicit verified
	// tenant claim. Cross-tenant operators use the live TenantActors resolver.
	if a.cfg.TenantActors == nil && id.SubjectKind == auth.SubjectUser && id.TenantID != "" && id.TenantID != scope.TenantID.String() {
		return auth.ErrForbidden
	}
	bound := auth.Scope{}
	if b, ok := v.(auth.Bound); ok {
		bound = b.BoundScope()
	}
	if bound != (auth.Scope{}) && bound != a.cfg.AppScope && bound != a.cfg.TenantAdminScope && bound != a.root {
		return auth.ErrForbidden
	}
	if id.SubjectKind == auth.SubjectApplication && bound == (auth.Scope{}) {
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
	checker, ok := v.(auth.PermissionChecker)
	if !ok {
		return false, nil
	}
	if target.ChannelID != "" {
		id, err := ChannelScopeID(scope, target.ChannelID)
		if err != nil {
			return false, err
		}
		group := auth.Scope{ID: id, Authority: a.cfg.PermissionAuthority}
		if err := a.currentGroup(ctx, group, "channel"); err != nil {
			return false, err
		}
		return checker.Can(ctx, group, target.Permission)
	}
	// Avatars belong to the tenant account directory. App moderation grants
	// must not become permission to edit shared profiles in every app.
	if target.Permission == access.AvatarsWrite.String() {
		if a.cfg.TenantAdminScope == (auth.Scope{}) {
			return false, nil
		}
		if err := a.currentGroup(ctx, a.cfg.TenantAdminScope, "tenant"); err != nil {
			return false, err
		}
		return checker.Can(ctx, a.cfg.TenantAdminScope, TenantPermission(access.AvatarsWrite))
	}
	permission := staffPermission("app", target.Permission)
	if permission == "" {
		return false, nil
	}
	return checker.Can(ctx, a.cfg.AppScope, permission)
}

// ChannelScopeID is a deterministic AuthKit group identity for a ContentKit
// channel in one tenant and app. Existing content IDs and stored auth_scope
// references stay unchanged; only the provider's authorization ID is derived.
func ChannelScopeID(scope contentkit.Scope, groupID string) (string, error) {
	if err := scope.Validate(); err != nil {
		return "", err
	}
	if groupID == "" || strings.TrimSpace(groupID) != groupID {
		return "", errors.New("contentkit/authkit: group ID is required")
	}
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte("contentkit:group:"+scope.TenantID.String()+":"+scope.AppID.String()+":"+groupID)).String(), nil
}
