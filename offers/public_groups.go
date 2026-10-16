package offers

import (
	"context"

	"github.com/open-rails/contentkit/internal/gate"
	"github.com/open-rails/contentkit/kinds"
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

// libraryDomain resolves hard domain rules. Reader preferences do not change
// ownership; this boundary only stops a blocked item being disclosed here.
func (s *Service) libraryDomain(ctx context.Context) (exclude, required, blocked, requireKinds []string, missingRequired, missingHard bool, err error) {
	d, _ := s.kinds.DomainFor(gate.Domain(ctx))
	exclude, required, blocked, requireKinds = append([]string{}, d.ExcludeRestrictions...), []string{}, []string{}, append([]string{}, d.RequireKinds...)
	refs := append([]kinds.GroupRef{}, d.RequireGroups...)
	for _, f := range d.Filters {
		if !f.Visible {
			refs = append(refs, f.Group)
		}
	}
	if len(refs) == 0 {
		return
	}
	ks, slugs := make([]string, len(refs)), make([]string, len(refs))
	for i, r := range refs {
		ks[i], slugs[i] = r.Kind, r.Slug
	}
	rows, e := s.pool.Query(ctx, `SELECT g.kind,g.slug,g.id,g.internal FROM `+s.t.groups+` g JOIN unnest($2::text[],$3::text[]) r(kind,slug) ON g.kind=r.kind AND g.slug=r.slug WHERE g.tenant_id=$1 AND g.state='active' AND g.scope_id=''`, s.tenant, ks, slugs)
	if e != nil {
		err = e
		return
	}
	defer rows.Close()
	type entity struct {
		id       string
		internal bool
	}
	found := map[kinds.GroupRef]entity{}
	for rows.Next() {
		var r kinds.GroupRef
		var g entity
		if err = rows.Scan(&r.Kind, &r.Slug, &g.id, &g.internal); err != nil {
			return
		}
		found[r] = g
	}
	if err = rows.Err(); err != nil {
		return
	}
	for _, r := range d.RequireGroups {
		if g, ok := found[r]; ok {
			required = append(required, g.id)
		} else {
			missingRequired = true
		}
	}
	for _, f := range d.Filters {
		g, ok := found[f.Group]
		if !ok && !f.Visible && !f.Toggleable() {
			missingHard = true
		}
		if ok && !f.Visible && (!f.Toggleable() || g.internal) {
			blocked = append(blocked, g.id)
		}
	}
	return
}
