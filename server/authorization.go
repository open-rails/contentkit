package server

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/iam"
	contentkit "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/access"
	ckauthkit "github.com/open-rails/contentkit/adapters/authkit"
)

// AuthorizationConfig selects existing grants; startup never creates groups
// or assigns their roles. Issuer must identify one dedicated tenant account
// directory, not an issuer shared by unrelated tenants.
type AuthorizationConfig struct {
	Issuer        string `yaml:"issuer"`
	AppGroupID    string `yaml:"app_group_id"`
	TenantGroupID string `yaml:"tenant_group_id"`
}

func validateAuthorizationConfig(cfg Config, ak authkit.Config) error {
	if cfg.Engine.Scope == nil {
		if cfg.Authorization != nil {
			return errors.New("server: authorization requires engine.scope")
		}
		return nil
	}
	if err := cfg.Engine.Scope.Validate(); err != nil {
		return err
	}
	a := cfg.Authorization
	if a == nil || a.Issuer == "" || strings.TrimSpace(a.Issuer) != a.Issuer || a.AppGroupID == "" || strings.TrimSpace(a.AppGroupID) != a.AppGroupID {
		return errors.New("server: engine.scope requires authorization.issuer and authorization.app_group_id")
	}
	if ak.Token.Issuer == "" || strings.TrimSpace(ak.Token.Issuer) != ak.Token.Issuer {
		return errors.New("server: scoped authorization requires explicit auth.token.issuer permission authority")
	}
	if cfg.UsersIssuer != "" && cfg.UsersIssuer != a.Issuer {
		return errors.New("server: users_issuer must match authorization.issuer in scoped mode")
	}
	if strings.TrimSpace(a.TenantGroupID) != a.TenantGroupID {
		return errors.New("server: authorization.tenant_group_id must be an exact group ID")
	}
	for _, remote := range ak.RemoteApplications {
		if remote.Issuer == a.Issuer {
			return errors.New("server: scoped identity issuer must be provisioned in authorization.app_group_id; auth.remote_applications declares root applications")
		}
	}
	return nil
}

// serverRoles declares the permission catalog only. A pre-existing root role
// is never copied into either scoped persona.
func serverRoles(engine contentkit.Config) *authkit.Roles {
	r := authkit.NewRoles(authkit.RemoteApplications)
	// The engine's permission catalog still uses these stable operation names;
	// scoped policy maps them to app/tenant permissions instead of granting root.
	rootPermissions := r.Root.Declare(access.Names(access.Permissions()...)...)
	if engine.Scope == nil {
		var grants []iam.Grant
		for _, p := range rootPermissions {
			grants = append(grants, p)
		}
		r.Root.Role(StaffRole, grants...)
		return r
	}
	app := r.Persona("contentkit", authkit.RemoteApplications, authkit.APIKeys)
	tenant := r.Persona("contentkit-tenant", authkit.RemoteApplications, authkit.APIKeys)
	var appGrants, tenantGrants []iam.Grant
	for _, p := range access.Permissions() {
		appGrants = append(appGrants, app.Declare(ckauthkit.AppPermission(p))[0])
		tenantGrants = append(tenantGrants, tenant.Declare(ckauthkit.TenantPermission(p))[0])
	}
	app.Role(StaffRole, appGrants...)
	tenant.Role(StaffRole, tenantGrants...)
	for name, kind := range engine.Content.GroupKinds {
		if kind.AuthScope {
			r.Persona(name).Declare(access.GroupPermissions(name)...)
		}
	}
	return r
}

func (s *Server) scopedAuthorization(ctx context.Context, cfg Config, ak authkit.Config, deps *contentkit.Deps) error {
	a := cfg.Authorization
	app, err := s.auth.Scope(ctx, iam.GroupByID(a.AppGroupID))
	if err != nil {
		return fmt.Errorf("server: authorization app group must be provisioned before startup: %w", err)
	}
	policy := ckauthkit.AuthorizationConfig{
		Scope: *cfg.Engine.Scope, Issuer: a.Issuer, PermissionAuthority: ak.Token.Issuer,
		AppScope: app, Accounts: ckauthkit.IssuerAccounts(cfg.Engine.Scope.TenantID, a.Issuer),
	}
	if a.TenantGroupID != "" {
		if policy.TenantAdminScope, err = s.auth.Scope(ctx, iam.GroupByID(a.TenantGroupID)); err != nil {
			return fmt.Errorf("server: authorization tenant group must be provisioned before startup: %w", err)
		}
	}
	if a.Issuer == ak.Token.Issuer {
		deps.Users = s.auth.UserInfo()
	} else {
		remote, err := s.auth.RemoteApplication(ctx, iam.AppByIssuer(a.Issuer))
		if err != nil || !remote.Enabled || remote.GroupID != a.AppGroupID {
			return fmt.Errorf("server: identity issuer must have a live remote application in authorization.app_group_id: %w", errors.Join(err, errors.New("missing or mismatched remote application binding")))
		}
		deps.Users = s.auth.RemoteUserInfo(iam.GroupByID(a.AppGroupID), a.Issuer)
	}
	if deps.Authorization, err = ckauthkit.NewAuthorization(ctx, s.auth, policy); err != nil {
		return fmt.Errorf("server: scoped authorization: %w", err)
	}
	if deps.Scopes, err = ckauthkit.AppScopes(s.auth, *cfg.Engine.Scope); err != nil {
		return fmt.Errorf("server: scoped groups: %w", err)
	}
	return nil
}
