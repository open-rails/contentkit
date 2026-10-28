package catalogstore

import (
	"context"
	"slices"

	"github.com/open-rails/contentkit/access"
	catalog "github.com/open-rails/contentkit/catalog"
)

// redactInternalEntities removes operator-only classification and offer keys
// from public DTOs after access has been decided. It never changes Allowed.
func (s *Store) redactInternalEntities(ctx context.Context, items map[string]*catalog.Content) error {
	var ids []string
	add := func(id string) {
		if id != "" {
			ids = append(ids, id)
		}
	}
	collect := func(d catalog.Decision) {
		for _, release := range d.Releases {
			add(release.ChannelID)
		}
		for _, raw := range append(append([]string{}, d.Sell...), d.Requires) {
			if key, err := access.ParseKey(raw); err == nil && key.Kind() == access.KeyChannel {
				add(key.Channel())
			}
		}
	}
	for _, c := range items {
		add(c.ChannelID)
		add(c.Access.ChannelID)
		for _, release := range c.Access.Releases {
			add(release.ChannelID)
		}
		collect(c.Decision)
		for _, v := range c.Versions {
			if v.Access != nil {
				add(v.Access.ChannelID)
				for _, release := range v.Access.Releases {
					add(release.ChannelID)
				}
			}
			collect(v.Decision)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	rows, err := s.pool.Query(ctx, `SELECT id FROM `+s.t.entities+` WHERE tenant_id=$1 AND id=ANY($2::text[]) AND internal`, s.tenant, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	internal := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		internal[id] = true
	}
	if err := rows.Err(); err != nil {
		return err
	}
	privateKey := func(raw string) bool {
		key, err := access.ParseKey(raw)
		return err == nil && key.Kind() == access.KeyChannel && internal[key.Channel()]
	}
	redact := func(d *catalog.Decision) {
		d.Releases = slices.DeleteFunc(slices.Clone(d.Releases), func(r access.Release) bool { return internal[r.ChannelID] })
		if privateKey(d.Requires) {
			// No offered purchase may skip this required membership.
			d.Sell, d.Requires = nil, ""
			return
		}
		d.Sell = slices.DeleteFunc(slices.Clone(d.Sell), privateKey)
	}
	rule := func(r *access.Rule) {
		if r == nil {
			return
		}
		r.Releases = slices.DeleteFunc(slices.Clone(r.Releases), func(r access.Release) bool { return internal[r.ChannelID] })
		if !internal[r.ChannelID] {
			return
		}
		// The public shape says only that access is locked. The original entity
		// rule remains stored and was already evaluated by the access gate.
		*r = access.Rule{Level: access.PPV, Preview: r.Preview, Releases: r.Releases}
	}
	for _, c := range items {
		if internal[c.ChannelID] {
			c.ChannelID, c.Channel = "", nil
		}
		rule(&c.Access)
		redact(&c.Decision)
		for i := range c.Versions {
			v := &c.Versions[i]
			rule(v.Access)
			redact(&v.Decision)
		}
	}
	return nil
}
