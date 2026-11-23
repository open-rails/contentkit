package entitystore

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/contentkit/internal/domainpolicy"
	"github.com/open-rails/contentkit/internal/gate"
	"github.com/open-rails/contentkit/internal/locationpolicy"
)

// ForHost binds public entity reads to a host's hard catalog boundaries.
// Reader toggles and staff management reads retain their existing semantics.
// The original store is immutable and can serve other hosts concurrently.
func (s *Store) ForHost(ctx context.Context, host string) (*Store, error) {
	scoped := *s
	scoped.boundary = nil
	domain, _ := s.cfg.DomainFor(host)
	var err error
	scoped.locations, err = locationpolicy.Resolve(ctx, s.pool, s.schema, s.tenant, locationpolicy.DomainName(domain.Name), gate.Actor(ctx).IP)
	if err != nil {
		return nil, err
	}
	if len(domain.Filters) == 0 && len(domain.RequireEntities) == 0 {
		return &scoped, nil
	}
	err = s.read(ctx, func(q Querier) error {
		policies, err := domainpolicy.Resolve(ctx, q, s.table("content_entities"), s.tenant, domain, s.cfg.ContentFilters.AppliesTo)
		if err != nil {
			return err
		}
		for kind, policy := range policies {
			if len(policy.Required) == 0 && len(policy.Blocked) == 0 && !policy.MissingRequired && !policy.MissingHard {
				delete(policies, kind)
			}
		}
		scoped.boundary = policies
		return nil
	})
	return &scoped, err
}

func (s *Store) publicBoundary(alias, prefix string) (string, pgx.NamedArgs) {
	scope, args := s.boundary.SQL(alias, prefix)
	location, more := s.locations.SQL(alias, prefix+"_location")
	for k, v := range more {
		args[k] = v
	}
	return "(" + scope + " AND " + location + ")", args
}
