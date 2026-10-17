package groups

import (
	"fmt"
	"github.com/open-rails/contentkit/kinds"
)

// policyBindingError keeps a mutation from silently changing the meaning of a
// configured hard domain rule. The existing conflict code is the public API.
type policyBindingError struct {
	domain string
	entity GroupID
}

func (e *policyBindingError) Error() string {
	return fmt.Sprintf("entity %s is used by the %s domain policy; update the policy first", e.entity, e.domain)
}

func (*policyBindingError) Unwrap() error { return ErrConflict }

// checkPolicyBinding allows presentation edits while keeping each hard rule's
// canonical reference and implicit internal classification intact. Inactive
// internal rows also retain evidence needed for fail-closed policy reads.
func (s *Store) checkPolicyBinding(before, after Group) error {
	if before.ScopeID != "" {
		return nil // Domain references resolve only unscoped canonical entities.
	}
	identityChanged := before.Kind != after.Kind || before.Slug != after.Slug || before.ScopeID != after.ScopeID
	deactivated := before.State == StateActive && after.State != StateActive
	unmarked := before.Internal && !after.Internal
	if !identityChanged && !deactivated && !unmarked {
		return nil
	}
	for _, domain := range s.cfg.ContentFilters.Domains {
		required := domain.Requires(kinds.GroupRef{Kind: before.Kind, Slug: before.Slug})
		if required && (identityChanged || deactivated) {
			return &policyBindingError{domain: domain.Name, entity: before.ID}
		}
		for _, filter := range domain.Filters {
			if filter.Group.Kind != before.Kind || filter.Group.Slug != before.Slug || filter.Visible {
				continue
			}
			fixed := !filter.Toggleable()
			if (fixed || before.Internal) && (identityChanged || deactivated) || before.Internal && !fixed && !required && unmarked {
				return &policyBindingError{domain: domain.Name, entity: before.ID}
			}
		}
	}
	return nil
}
