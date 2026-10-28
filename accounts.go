package contentkit

import "fmt"

// AccountsConfig selects the shared tenant account storage and avatar service.
// Catalogs remain app-owned. Schema holds account metadata and the
// tenant avatar journal, allocations and publication metadata. The engine
// derives the account key from Scope.TenantID.
type AccountsConfig struct {
	Schema  string         `json:"schema" yaml:"schema"`
	Avatars *AvatarsConfig `json:"avatars,omitempty" yaml:"avatars,omitempty"`
}

// withAccounts adapts the shared account declaration to the established account
// and avatar implementations. It never mutates the caller's configuration.
func (c Config) withAccounts() (Config, error) {
	a := c.Accounts
	if a == nil {
		return c, nil
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
		kind.Namespace = ""
		kind.Account = true
		avatars.Kind = &kind
		c.Avatars = &avatars
	}
	return c, nil
}
