package catalog

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/contentkit/kinds"
)

var errFixedOwnerUnavailable = errors.New("configured content channel unavailable")

// fixedOwner applies a configured site-owned channel to every creation path.
// Ordinary creator-owned kinds are unchanged. Transactional callers retain
// the row lock until their content write commits.
func (s *Store) fixedOwner(ctx context.Context, q Querier, kind, supplied string) (string, error) {
	policy := s.cfg.Kinds[kind].Channel
	if policy == nil {
		if supplied != "" {
			return "", invalid("kind %s does not use channels", kind)
		}
		return "", nil
	}
	if policy.Fixed == "" {
		if policy.Required && supplied == "" {
			return "", invalid("kind %s requires a channel", kind)
		}
		return supplied, nil
	}
	ref := kinds.EntityRef{Kind: "channel", Slug: policy.Fixed}
	var id string
	err := q.QueryRow(ctx, `SELECT id FROM `+s.t.entities+`
 WHERE tenant_id=$1 AND kind=$2 AND slug=$3 AND state='active'
 AND scope_id = '' AND owner_user_id IS NULL AND NOT internal
 AND (NOT $4::boolean OR auth_scope IS NOT NULL)
 FOR SHARE`, s.tenant, ref.Kind, ref.Slug, s.cfg.EntityKinds[ref.Kind].AuthScope).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("%w: kind %s needs its configured site-owned entity %s/%s", errFixedOwnerUnavailable, kind, ref.Kind, ref.Slug)
	}
	if err != nil {
		return "", err
	}
	if supplied != "" && supplied != id {
		return "", invalid("kind %s has a fixed owner entity", kind)
	}
	return id, nil
}
