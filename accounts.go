package contentkit

import (
	"fmt"
	"strings"

	"github.com/open-rails/contentkit/media/layout"
)

// AccountsConfig selects the shared tenant account storage and avatar service.
// Catalogs remain app-owned. Schema holds account metadata and the
// tenant avatar journal, allocations and publication metadata. A scoped engine
// derives the account key or loads its immutable adopted key from the binding.
type AccountsConfig struct {
	// Namespace is legacy-only. Scoped engines use MigrationOptions.LegacyAccountKey
	// once during adoption instead of configuring an independent namespace.
	Namespace string         `json:"namespace" yaml:"namespace"`
	Schema    string         `json:"schema" yaml:"schema"`
	Avatars   *AvatarsConfig `json:"avatars,omitempty" yaml:"avatars,omitempty"`
}

// withAccounts adapts the shared account declaration to the established account
// and avatar implementations. It never mutates the caller's configuration.
func (c Config) withAccounts() (Config, error) {
	a := c.Accounts
	if a == nil {
		return c, nil
	}
	if !layout.ValidSegment(a.Namespace) || a.Namespace == strings.TrimSpace(c.Tenant) {
		return c, fmt.Errorf("contentkit: Accounts.Namespace must be a valid shared namespace distinct from Tenant")
	}
	if !schemaRE.MatchString(a.Schema) || len(a.Schema) > 63 {
		return c, fmt.Errorf("contentkit: Accounts.Schema must be a lower-case PostgreSQL identifier")
	}
	if c.Avatars != nil && a.Avatars != nil {
		return c, fmt.Errorf("contentkit: configure Accounts.Avatars or Avatars, not both")
	}
	accounts := *a
	c.Accounts = &accounts
	if a.Avatars != nil {
		c.Avatars = a.Avatars
		// Keep account storage coordinates without carrying a second avatar
		// declaration into later MediaKinds normalization. The caller's struct
		// remains untouched, and a genuinely duplicated input is refused above.
		accounts.Avatars = nil
	}
	if c.Avatars != nil {
		avatars := *c.Avatars
		kind, _ := avatars.kind()
		if (c.Media == nil || c.Media.Scope.IsZero() || c.Media.LegacyLayout) && avatars.Kind != nil && kind.Namespace != "" && kind.Namespace != a.Namespace {
			return c, fmt.Errorf("contentkit: avatar kind namespace must match Accounts.Namespace")
		}
		if c.Media != nil && !c.Media.Scope.IsZero() && !c.Media.LegacyLayout {
			kind.Namespace = ""
			kind.Account = true
		} else {
			kind.Namespace = a.Namespace
		}
		avatars.Kind = &kind
		c.Avatars = &avatars
	}
	return c, nil
}
