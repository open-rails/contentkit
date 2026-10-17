package offers

import (
	"context"

	"github.com/open-rails/contentkit/internal/domainpolicy"
	"github.com/open-rails/contentkit/internal/gate"
)

func (s *Service) publicGroups(ctx context.Context, ids []string) (map[string]bool, error) {
	out := map[string]bool{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT id FROM `+s.t.groups+` WHERE tenant_id=$1 AND id=ANY($2::text[]) AND state='active' AND NOT internal`, s.tenant, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// libraryDomain resolves only hard per-kind boundaries; ownership remains unchanged.
func (s *Service) libraryDomain(ctx context.Context) (domainpolicy.Policies, error) {
	d, _ := s.kinds.DomainFor(gate.Domain(ctx))
	return domainpolicy.Resolve(ctx, s.pool, s.t.groups, s.tenant, d, s.kinds.ContentFilters.AppliesTo)
}
