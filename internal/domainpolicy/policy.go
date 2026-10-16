// Package domainpolicy resolves and applies per-content-kind domain rules.
package domainpolicy

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/contentkit/kinds"
)

type Querier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

type Policy struct {
	Required        []string
	Blocked         []string
	Toggles         map[string]bool // entity ID to default visibility
	ToggleRefs      map[kinds.GroupRef]string
	MissingRequired bool
	MissingHard     bool
}
type Policies map[string]Policy

// Resolve includes internal entities for policy evaluation, but never puts
// them in Toggles. Missing fixed exclusions fail only their content kind closed.
func Resolve(ctx context.Context, q Querier, table, tenant string, d kinds.Domain) (Policies, error) {
	refs := map[kinds.GroupRef]bool{}
	for _, p := range d.Kinds {
		for _, r := range p.RequireGroups {
			refs[r] = true
		}
		for _, f := range p.Filters {
			refs[f.Group] = true
		}
	}
	type entity struct {
		id       string
		internal bool
		active   bool
	}
	found := map[kinds.GroupRef]entity{}
	inactiveInternal := map[kinds.GroupRef]bool{}
	if len(refs) > 0 {
		ks, ss := []string{}, []string{}
		for r := range refs {
			ks = append(ks, r.Kind)
			ss = append(ss, r.Slug)
		}
		rows, err := q.Query(ctx, `SELECT g.kind,g.slug,g.id,g.internal,g.state='active' FROM `+table+` g JOIN unnest($2::text[],$3::text[]) r(kind,slug) ON g.kind=r.kind AND g.slug=r.slug WHERE g.tenant_id=$1 AND g.scope_id='' ORDER BY (g.state='active') DESC,g.internal DESC`, tenant, ks, ss)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var r kinds.GroupRef
			var e entity
			if err := rows.Scan(&r.Kind, &r.Slug, &e.id, &e.internal, &e.active); err != nil {
				rows.Close()
				return nil, err
			}
			if !e.active && e.internal {
				inactiveInternal[r] = true
			}
			if old, ok := found[r]; !ok || !old.active && e.active {
				found[r] = e
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	out := Policies{}
	for kind, rule := range d.Kinds {
		p := Policy{Required: []string{}, Blocked: []string{}, Toggles: map[string]bool{}, ToggleRefs: map[kinds.GroupRef]string{}}
		for _, r := range rule.RequireGroups {
			if e, ok := found[r]; ok && e.active {
				p.Required = append(p.Required, e.id)
			} else {
				p.MissingRequired = true
			}
		}
		for _, f := range rule.Filters {
			if !f.Visible && inactiveInternal[f.Group] {
				p.MissingHard = true
				continue
			}
			e, ok := found[f.Group]
			if !ok || !e.active {
				if !f.Visible && (!f.Toggleable() || e.internal) {
					p.MissingHard = true
				}
				continue
			}
			if f.Toggleable() && !e.internal && !rule.Requires(f.Group) {
				p.Toggles[e.id] = f.Visible
				p.ToggleRefs[f.Group] = e.id
			} else if !f.Visible {
				p.Blocked = append(p.Blocked, e.id)
			}
		}
		out[kind] = p
	}
	return out, nil
}

func (p Policies) Allows(kind string, entities []string) bool {
	r, ok := p[kind]
	if !ok {
		return true
	}
	if r.MissingHard || r.MissingRequired {
		return false
	}
	for _, id := range r.Required {
		if !slices.Contains(entities, id) {
			return false
		}
	}
	for _, id := range r.Blocked {
		if slices.Contains(entities, id) {
			return false
		}
	}
	return true
}

// SQL is the hard boundary over a content facet alias. Unconfigured kinds pass.
func (p Policies) SQL(alias, prefix string) (string, pgx.NamedArgs) {
	args := pgx.NamedArgs{}
	if len(p) == 0 {
		return "true", args
	}
	names := make([]string, 0, len(p))
	for kind := range p {
		names = append(names, kind)
	}
	sort.Strings(names)
	args[prefix+"_kinds"] = names
	parts := []string{"NOT (" + alias + ".kind = ANY(@" + prefix + "_kinds::text[]))"}
	for i, kind := range names {
		rule := p[kind]
		key := fmt.Sprintf("%s_%d", prefix, i)
		args[key+"_kind"] = kind
		pred := "false"
		if !rule.MissingHard && !rule.MissingRequired {
			args[key+"_required"], args[key+"_blocked"] = rule.Required, rule.Blocked
			pred = alias + ".group_ids @> @" + key + "_required::text[] AND NOT (" + alias + ".group_ids && @" + key + "_blocked::text[])"
		}
		parts = append(parts, "("+alias+".kind = @"+key+"_kind AND ("+pred+"))")
	}
	return "(" + strings.Join(parts, " OR ") + ")", args
}

// HiddenSQL applies optional preferences to the same kind that owns them.
func HiddenSQL(hidden map[string][]string, alias, prefix string) (string, pgx.NamedArgs) {
	args := pgx.NamedArgs{}
	parts := []string{}
	names := make([]string, 0, len(hidden))
	for kind, ids := range hidden {
		if len(ids) > 0 {
			names = append(names, kind)
		}
	}
	sort.Strings(names)
	for i, kind := range names {
		key := fmt.Sprintf("%s_%d", prefix, i)
		args[key+"_kind"], args[key+"_ids"] = kind, hidden[kind]
		parts = append(parts, "("+alias+".kind <> @"+key+"_kind OR NOT ("+alias+".group_ids && @"+key+"_ids::text[]))")
	}
	if len(parts) == 0 {
		return "true", args
	}
	return "(" + strings.Join(parts, " AND ") + ")", args
}
