package discoverystore

import (
	"context"

	"github.com/open-rails/contentkit/access"
	entitlement "github.com/open-rails/contentkit/internal/entitlement"
)

// internalAccessEntities is used only to redact public purchase metadata after
// the gate evaluated the original rules and entitlements.
func (s *Service) internalAccessEntities(ctx context.Context, items []access.Item, decisions []entitlement.Decision) (map[string]bool, error) {
	var ids []string
	for _, item := range items {
		for _, release := range item.Rule.Releases {
			if release.ChannelID != "" {
				ids = append(ids, release.ChannelID)
			}
		}
		if item.Rule.ChannelID != "" {
			ids = append(ids, item.Rule.ChannelID)
		}
	}
	for _, d := range decisions {
		for _, key := range d.Sell {
			if key.Kind() == access.KeyChannel {
				ids = append(ids, key.Channel())
			}
		}
		if d.Requires != nil && d.Requires.Kind() == access.KeyChannel {
			ids = append(ids, d.Requires.Channel())
		}
	}
	internal := map[string]bool{}
	if len(ids) == 0 {
		return internal, nil
	}
	rows, err := s.o.Pool.Query(ctx, `SELECT id FROM `+s.t.entities+` WHERE tenant_id=$1 AND id=ANY($2::text[]) AND internal`, s.o.Tenant, ids)
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
