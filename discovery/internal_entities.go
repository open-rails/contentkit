package discovery

import (
	"context"

	"github.com/open-rails/contentkit/access"
)

// internalAccessGroups is used only to redact public purchase metadata after
// the gate evaluated the original rules and entitlements.
func (s *Service) internalAccessGroups(ctx context.Context, items []access.Item, decisions []access.Decision) (map[string]bool, error) {
	var ids []string
	for _, item := range items {
		for _, release := range item.Rule.Releases {
			if release.Group != "" {
				ids = append(ids, release.Group)
			}
		}
		if item.Rule.Group != "" {
			ids = append(ids, item.Rule.Group)
		}
	}
	for _, d := range decisions {
		for _, key := range d.Sell {
			if key.Kind() == access.KeyGroup {
				ids = append(ids, key.Group())
			}
		}
		if d.Requires != nil && d.Requires.Kind() == access.KeyGroup {
			ids = append(ids, d.Requires.Group())
		}
	}
	internal := map[string]bool{}
	if len(ids) == 0 {
		return internal, nil
	}
	rows, err := s.o.Pool.Query(ctx, `SELECT id FROM `+s.t.groups+` WHERE tenant_id=$1 AND id=ANY($2::text[]) AND internal`, s.o.Tenant, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		internal[id] = true
	}
	return internal, rows.Err()
}
