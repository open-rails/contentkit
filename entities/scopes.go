package entities

import (
	"context"
)

// ScopeProvisioner ensures the declared authorization groups exist. It is the
// only provider capability needed by explicit fixed-owner migrations.
type ScopeProvisioner interface {
	Provision(ctx context.Context, scopes []Scope) error
}

// Scopes keeps the AuthKit groups of auth_scope entity kinds. Provision and
// Retire are idempotent; runtime adds read/revocation behavior to provisioning.
type Scopes interface {
	ScopeProvisioner
	// Retire stops each scope resolving and granting.
	Retire(ctx context.Context, ids []string) error
	// Holding answers, per live scope of the kinds' personas user belongs
	// to, the kind's access.ChannelPermissions user holds there.
	Holding(ctx context.Context, user string, kinds []string) (map[string][]string, error)
}

// LocalScopes optionally narrows authorization lookups to scope IDs verified
// by this app's store. It lets a shared identity directory resolve grants
// without enumerating or trusting entities belonging to other apps.
type LocalScopes interface {
	HoldingScopes(ctx context.Context, user string, kinds, scopeIDs []string) (map[string][]string, error)
}

// AppBinding identifies the tenant/app an authorization scope provider serves.
// A scoped engine requires a matching binding before enabling auth_scope kinds.
type AppBinding interface {
	ContentKitScope() (tenantID, appID string)
}

// Scope is one entity's AuthKit scope: ContentKit keys it by the entity's id.
type Scope struct {
	// Restore permits deliberately restoring a previously retired group. Normal
	// provisioning (including configured fixed owners) preserves live groups
	// and refuses deleted groups rather than silently removing their grants.
	Restore bool
	ID      string
	Kind    string
	Owner   string
}

// Ref names one entity with its kind.
type Ref struct {
	Kind string
	ID   EntityID
}
