package discoverystore

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/internal/domainpolicy"
	"github.com/open-rails/contentkit/internal/entitystore"
	"github.com/open-rails/contentkit/kinds"
)

// RefreshEntityPopularRankings replaces each kind and domain's ranking from account favourites.
func (s *Service) RefreshEntityPopularRankings(ctx context.Context) error {
	domains := slices.Clone(s.o.Kinds.ContentFilters.Domains)
	if !slices.ContainsFunc(domains, func(d kinds.Domain) bool { return d.Name == "main" }) {
		domains = append(domains, kinds.Domain{})
	}
	var failures []error
	for _, kind := range slices.Sorted(maps.Keys(s.o.Kinds.EntityKinds)) {
		p := s.o.Kinds.EntityKinds[kind].Popular
		if p == nil {
			continue
		}
		for _, domain := range domains {
			err := pgx.BeginFunc(ctx, s.o.Pool, func(tx pgx.Tx) error {
				policies, err := domainpolicy.Resolve(ctx, tx, s.t.entities, s.o.Tenant, domain, s.o.Kinds.ContentFilters.AppliesTo)
				if err != nil {
					return err
				}
				boundary, args := policies.SQL("f", "entity_domain")
				args["tenant"], args["entity_kind"], args["content_kind"], args["domain"] = s.o.Tenant, kind, p.ContentKind, domain.Name
				if _, err := tx.Exec(ctx, `DELETE FROM `+s.t.entityRankings+` WHERE tenant_id=@tenant AND entity_kind=@entity_kind AND domain=@domain`, args); err != nil {
					return err
				}
				_, err = tx.Exec(ctx, `WITH favorites AS (
 SELECT user_id, content_id, exp(-ln(2)*extract(epoch FROM now()-max(created_at))::float8/5184000) AS weight
 FROM `+s.t.favorites+`
 WHERE tenant_id=@tenant AND content_kind=@content_kind AND value=1 AND user_id IS NOT NULL
 AND created_at >= now()-interval '180 days' AND created_at <= now()
 GROUP BY user_id,content_id),
members AS (
 SELECT DISTINCT f.content_id,m.entity_id
 FROM `+s.t.facets+` f CROSS JOIN LATERAL unnest(array_append(f.entity_ids,f.owner_entity_id)) m(entity_id)
 JOIN `+s.t.entities+` e ON e.tenant_id=f.tenant_id AND e.id=m.entity_id
 WHERE f.tenant_id=@tenant AND f.kind=@content_kind AND `+entitystore.LiveListed+` AND `+boundary+`
 AND e.kind=@entity_kind AND e.state='active' AND NOT e.internal AND coalesce(e.attributes->>'categorical','false') <> 'true'),
bests AS (
 SELECT m.entity_id,v.user_id,v.content_id,v.weight,
 lead(v.weight,1,0::float8) OVER account AS second,
 row_number() OVER account AS n
 FROM favorites v JOIN members m ON m.content_id=v.content_id
 WINDOW account AS (PARTITION BY m.entity_id,v.user_id ORDER BY v.weight DESC,v.content_id)),
losses AS (
 SELECT entity_id,content_id,sum(weight) AS support,sum(weight-second) AS loss
 FROM bests WHERE n=1 GROUP BY entity_id,content_id),
scores AS (
 SELECT entity_id,least(sum(support),2*(sum(support)-max(loss))) AS score
 FROM losses GROUP BY entity_id)
INSERT INTO `+s.t.entityRankings+` (tenant_id,entity_kind,domain,entity_id,place,computed_at)
SELECT @tenant,@entity_kind,@domain,entity_id,row_number() OVER (ORDER BY score DESC,entity_id),now()
FROM scores WHERE score >= 20`, args)
				return err
			})
			if err != nil {
				failures = append(failures, fmt.Errorf("discovery: store %s entity ranking for domain %q: %w", kind, domain.Name, err))
			}
		}
	}
	return errors.Join(failures...)
}
