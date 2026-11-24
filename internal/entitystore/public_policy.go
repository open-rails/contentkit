package entitystore

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/contentkit/internal/domainpolicy"
	"github.com/open-rails/contentkit/internal/gate"
	"github.com/open-rails/contentkit/internal/locationpolicy"
)

// readScope is a public read's hard boundary: the domain rules of the
// request's host and the visitor's location rules. The zero scope applies
// none: management reads, and the engine's own outside any request.
type readScope struct {
	domain    domainpolicy.Policies
	locations locationpolicy.Block
}

func (b readScope) bounded() bool { return len(b.domain) > 0 || !b.locations.Empty() }

func (b readScope) sql(alias, prefix string) (string, pgx.NamedArgs) {
	scope, args := b.domain.SQL(alias, prefix)
	location, more := b.locations.SQL(alias, prefix+"_location")
	for k, v := range more {
		args[k] = v
	}
	return "(" + scope + " AND " + location + ")", args
}

// publicScope is the boundary of ctx's request: every public read a request
// makes, counts and automatic covers included, stays inside it. Reader
// toggles and staff management reads retain their own semantics.
func (s *Store) publicScope(ctx context.Context) (readScope, error) {
	if !gate.InRequest(ctx) {
		return readScope{}, nil
	}
	var b readScope
	domain, _ := s.cfg.DomainFor(gate.Domain(ctx))
	var err error
	b.locations, err = locationpolicy.Resolve(ctx, s.pool, s.schema, s.tenant, locationpolicy.DomainName(domain.Name), gate.Actor(ctx).IP)
	if err != nil || len(domain.Filters) == 0 && len(domain.RequireEntities) == 0 {
		return b, err
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
		b.domain = policies
		return nil
	})
	return b, err
}
