package entitlement

import (
	access "github.com/open-rails/contentkit/access"
)

func validTier(s string) bool {
	return len(s) <= 63 && s != "" && s[0] >= 'a' && s[0] <= 'z' && access.ValidTenant(s)
}
