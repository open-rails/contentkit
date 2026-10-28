package entitlement

import (
	access "github.com/open-rails/contentkit/access"
)

// Effective is a version's rule: its override, else its content's.
func Effective(content access.Rule, override *access.Rule) access.Rule {
	if override != nil {
		return *override
	}
	return content
}
