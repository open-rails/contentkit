package discovery

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/kinds"
)

// FacetVisibilityOptions configure FacetVisibility.
type FacetVisibilityOptions struct {
	Pool   *pgxpool.Pool
	Schema string
	Tenant string
	Kinds  kinds.Config
	Gate   *access.Gate
	Now    func() time.Time
}

// FacetVisibility is the listing rule of engine spec §4 over content_facets
// for engines without the catalog's (#120), which replaces it: live,
// listed versions in the site's scope, past the reader's content filters,
// in the requested language, and deleted content the reader holds.
func FacetVisibility(o FacetVisibilityOptions) Visibility {
	if o.Now == nil {
		o.Now = time.Now
	}
	q := func(t string) string { return pgx.Identifier{o.Schema, t}.Sanitize() }
	return facetVisibility{o: o, groups: q("content_groups"), prefs: q("content_filter_prefs")}
}

type facetVisibility struct {
	o             FacetVisibilityOptions
	groups, prefs string
}

type facetViewer struct {
	filter         *access.Filter
	mode           access.Mode
	now            time.Time
	language       string
	exclude        []string
	require        []string
	requireMissing bool
	hidden         []string
	includeDeleted bool
}

func (v facetVisibility) Viewer(ctx context.Context, o ViewerOptions) (Viewer, error) {
	f, err := v.o.Gate.Filter(ctx, o.Actor)
	if err != nil && !errors.Is(err, access.ErrUnavailable) {
		return nil, err
	}
	host := o.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	site, _ := v.o.Kinds.SiteFor(strings.ToLower(host))
	fv := &facetViewer{filter: f, mode: o.Mode, now: v.o.Now(), language: o.Language, exclude: append([]string{}, site.ExcludeRestrictions...),
		require: []string{}, hidden: []string{}}
	if fv.mode == "" {
		fv.mode = access.Lock
	}
	refs := slices.Clone(site.RequireGroups)
	for _, cf := range v.o.Kinds.ContentFilters {
		refs = append(refs, cf.Group)
	}
	ids, err := v.groupIDs(ctx, refs)
	if err != nil {
		return nil, err
	}
	for _, r := range site.RequireGroups {
		if id, ok := ids[r]; ok {
			fv.require = append(fv.require, id)
		} else {
			fv.requireMissing = true
		}
	}
	hidden := map[string]bool{}
	for _, cf := range v.o.Kinds.ContentFilters {
		if id, ok := ids[cf.Group]; ok && !cf.DefaultVisible {
			hidden[id] = true
		}
	}
	if o.Actor.Person() {
		var raw []byte
		err := v.o.Pool.QueryRow(ctx, `SELECT overrides, include_deleted FROM `+v.prefs+` WHERE tenant_id = $1 AND user_id = $2`,
			v.o.Tenant, o.Actor.ID).Scan(&raw, &fv.includeDeleted)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
		var overrides map[string]string
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &overrides); err != nil {
				return nil, err
			}
		}
		for id, o := range overrides {
			hidden[id] = o == hide
		}
	}
	for id, h := range hidden {
		if h {
			fv.hidden = append(fv.hidden, id)
		}
	}
	slices.Sort(fv.hidden)
	return fv, nil
}

func (v facetVisibility) groupIDs(ctx context.Context, refs []kinds.GroupRef) (map[kinds.GroupRef]string, error) {
	out := map[kinds.GroupRef]string{}
	if len(refs) == 0 {
		return out, nil
	}
	ks, ss := make([]string, len(refs)), make([]string, len(refs))
	for i, r := range refs {
		ks[i], ss[i] = r.Kind, r.Slug
	}
	rows, err := v.o.Pool.Query(ctx, `SELECT g.kind, g.slug, g.id FROM `+v.groups+` g JOIN unnest($2::text[], $3::text[]) AS r(kind, slug)
 ON g.kind = r.kind AND g.slug = r.slug WHERE g.tenant_id = $1 AND g.state = 'active' AND g.scope_id = ''`, v.o.Tenant, ks, ss)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var r kinds.GroupRef
		var id string
		if err := rows.Scan(&r.Kind, &r.Slug, &id); err != nil {
			return nil, err
		}
		out[r] = id
	}
	return out, rows.Err()
}

func (v *facetViewer) Listed(alias string) (string, pgx.NamedArgs) {
	col := func(c string) string { return alias + "." + c }
	opens, held, err := v.filter.SQL(alias, access.Hide)
	if err != nil {
		return "false", pgx.NamedArgs{}
	}
	args := pgx.NamedArgs{"cv_now": v.now, "cv_exclude": v.exclude, "cv_require": v.require, "cv_hidden": v.hidden,
		"cv_language": v.language, "cv_include_deleted": v.includeDeleted}
	for k, a := range held {
		args[k] = a
	}
	scope := "NOT (" + col("restrictions") + " && @cv_exclude::text[]) AND " + col("group_ids") + " @> @cv_require::text[]"
	if v.requireMissing {
		scope = "false"
	}
	live := col("live_from") + " <= @cv_now::timestamptz"
	listed := "(" + col("listing") + " = 'listed' OR (" + col("listing") + " = 'members' AND " + opens + "))"
	if v.mode == access.Hide {
		listed += " AND " + opens
	}
	public := "(NOT " + col("deleted") + " AND " + live + " AND " + scope + " AND NOT (" + col("group_ids") + " && @cv_hidden::text[])" +
		" AND (@cv_language::text = '' OR @cv_language::text = ANY(" + col("languages") + ")) AND " + listed + ")"
	bought := "(" + col("deleted") + " AND @cv_include_deleted::boolean AND " + live + " AND " + col("content_id") + " = ANY(@access_contents::text[]))"
	return "(" + public + " OR " + bought + ")", args
}
