package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/kinds"
)

// ViewerOptions describes one request's reader.
type ViewerOptions struct {
	Actor access.Actor
	// Host is the request's host; it picks the site (kinds.Config.SiteFor).
	Host string
	// Language, when set, lists only content with a live version serving it.
	Language string
	// Mode defaults to access.Lock.
	Mode access.Mode
	// EditorGroups are owner groups (channels) whose editors see their whole
	// content in lists; the viewer's editorship of each is checked.
	EditorGroups []string
}

// Viewer is the one visibility rule (ContentKit #117 spec §4) for one
// request: live content and versions, the listing rule, the site scope, the
// viewer's content filters and entitlements, and the staff, creators and
// owner-group editors who see everything. Listed renders it as SQL over
// content_facets for lists and searches; Store.Contents and Resolver decide
// items with it.
type Viewer struct {
	s              *Store
	actor          access.Actor
	site           kinds.Site
	language       string
	mode           access.Mode
	now            time.Time
	staff          bool
	filter         *access.Filter
	exclude        []string
	require        []string
	requireMissing bool
	hidden         []string
	includeDeleted bool
	editorGroups   []string
	// asked are owner groups whose editorship this request already checked.
	asked map[string]bool
}

// Viewer resolves o once per request: the site, staff rights, entitlements
// (one billing read, fail-closed when unavailable), the site's required groups
// and the viewer's content filters (config defaults merged with their own).
func (s *Store) Viewer(ctx context.Context, o ViewerOptions) (*Viewer, error) {
	v := &Viewer{s: s, actor: o.Actor, language: o.Language, mode: o.Mode, now: s.now()}
	if v.mode == "" {
		v.mode = access.Lock
	}
	if v.mode != access.Lock && v.mode != access.Hide {
		return nil, invalid("mode %q must be lock or hide", o.Mode)
	}
	if v.language != "" {
		if err := s.checkLanguage(v.language); err != nil {
			return nil, err
		}
	}
	host := o.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	v.site, _ = s.cfg.SiteFor(strings.ToLower(host))
	v.exclude = append([]string{}, v.site.ExcludeRestrictions...)
	rights, err := s.staffRights(ctx, o.Actor)
	if err != nil {
		// A read shows what the rule shows anyone; staff extras wait.
		s.log.WarnContext(ctx, "catalog: staff check failed; reading as a non-staff viewer", "err", err)
	}
	v.staff = rights.view || rights.write
	f, err := s.accessG.Filter(ctx, o.Actor)
	if err != nil && !errors.Is(err, access.ErrUnavailable) {
		return nil, err
	}
	v.filter = f
	refs := append([]kinds.GroupRef{}, v.site.RequireGroups...)
	for _, f := range s.cfg.ContentFilters {
		refs = append(refs, f.Group)
	}
	ids, err := s.groupIDs(ctx, refs)
	if err != nil {
		return nil, err
	}
	v.require = []string{}
	for _, r := range v.site.RequireGroups {
		id, ok := ids[r]
		if !ok {
			v.requireMissing = true
			continue
		}
		v.require = append(v.require, id)
	}
	hidden := map[string]bool{}
	for _, f := range s.cfg.ContentFilters {
		if id, ok := ids[f.Group]; ok && !f.DefaultVisible {
			hidden[id] = true
		}
	}
	if o.Actor.Person() {
		var raw []byte
		err := s.pool.QueryRow(ctx, `SELECT overrides, include_deleted FROM `+s.t.prefs+` WHERE tenant_id = $1 AND user_id = $2`, s.tenant, o.Actor.ID).Scan(&raw, &v.includeDeleted)
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
			hidden[id] = o == "hide"
		}
	}
	v.hidden = []string{}
	for id, h := range hidden {
		if h {
			v.hidden = append(v.hidden, id)
		}
	}
	slices.Sort(v.hidden)
	v.editorGroups = []string{}
	if len(o.EditorGroups) > 0 && !v.staff {
		got := s.readEditors(ctx, o.Actor, o.EditorGroups)
		v.asked = map[string]bool{}
		for _, g := range o.EditorGroups {
			if v.asked[g] = got[g]; got[g] {
				v.editorGroups = append(v.editorGroups, g)
			}
		}
		slices.Sort(v.editorGroups)
	}
	return v, nil
}

// groupIDs resolves active groups by kind and slug (unscoped ones).
func (s *Store) groupIDs(ctx context.Context, refs []kinds.GroupRef) (map[kinds.GroupRef]string, error) {
	out := map[kinds.GroupRef]string{}
	if len(refs) == 0 {
		return out, nil
	}
	ks, ss := make([]string, len(refs)), make([]string, len(refs))
	for i, r := range refs {
		ks[i], ss[i] = r.Kind, r.Slug
	}
	rows, err := s.pool.Query(ctx, `SELECT g.kind, g.slug, g.id FROM `+s.t.groups+` g JOIN unnest($2::text[], $3::text[]) AS r(kind, slug)
 ON g.kind = r.kind AND g.slug = r.slug WHERE g.tenant_id = $1 AND g.state = 'active' AND g.scope_id = ''`, s.tenant, ks, ss)
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

// editable reports an owner group the request already knows the viewer
// edits (ViewerOptions.EditorGroups).
func (v *Viewer) editable(group string) bool { return slices.Contains(v.editorGroups, group) }

// Editors reports, per owner group, whether the viewer edits its content and
// so opens it: staff edit every group's; anyone else is asked once per group
// per request, and a failed check reads as not an editor.
func (v *Viewer) Editors(ctx context.Context, groups []string) map[string]bool {
	out := map[string]bool{}
	var ask []string
	for _, g := range groups {
		switch {
		case g == "":
		case v.staff || v.editable(g):
			out[g] = true
		case v.asked != nil && v.asked[g]:
			out[g] = true
		case v.asked != nil && hasKey(v.asked, g):
		case !slices.Contains(ask, g):
			ask = append(ask, g)
		}
	}
	if len(ask) == 0 || v.actor.Anonymous || v.actor.ID == "" {
		return out
	}
	got := v.s.readEditors(ctx, v.actor, ask)
	if v.asked == nil {
		v.asked = map[string]bool{}
	}
	for _, g := range ask {
		v.asked[g], out[g] = got[g], got[g]
	}
	return out
}

func hasKey(m map[string]bool, k string) bool {
	_, ok := m[k]
	return ok
}

// Staff reports a viewer holding root:content:view-restricted or
// root:content:write: they see everything.
func (v *Viewer) Staff() bool { return v.staff }

// Site is the site the viewer is on.
func (v *Viewer) Site() kinds.Site { return v.site }

// Actor is the viewer's actor.
func (v *Viewer) Actor() access.Actor { return v.actor }

// Listed is a boolean SQL predicate over the content_facets row aliased
// alias: the versions the viewer's listing shows. A list shows one card per
// content item among its rows. In Hide mode, and for members-listed rows,
// rows a truncated keyspace may unlock are admitted as candidates: decide
// them with Contents (or access.Gate.Settle) before showing. Args are pgx
// named args prefixed cv_ and access_.
func (v *Viewer) Listed(alias string) (string, pgx.NamedArgs) {
	f := func(col string) string { return alias + "." + col }
	opens, accessArgs, err := v.filter.SQL(alias, access.Hide)
	if err != nil {
		// alias is the caller's constant; a bad one lists nothing.
		opens, accessArgs = "false", nil
	}
	versions := slices.Sorted(maps.Keys(v.filter.Versions()))
	args := pgx.NamedArgs{"cv_now": v.now, "cv_exclude": v.exclude, "cv_require": v.require, "cv_hidden": v.hidden,
		"cv_language": v.language, "cv_owned": v.filter.Contents(), "cv_owned_versions": versions,
		"cv_owned_candidates": !v.filter.Complete() && !v.filter.Unknown(), "cv_user": viewerID(v.actor), "cv_editors": v.editorGroups,
		"cv_include_deleted": v.includeDeleted}
	for k, a := range accessArgs {
		args[k] = a
	}
	if v.staff {
		return "true", args
	}
	live := f("live_from") + " <= @cv_now::timestamptz"
	scope := "NOT (" + f("restrictions") + " && @cv_exclude::text[]) AND " + f("group_ids") + " @> @cv_require::text[]"
	if v.requireMissing {
		scope = "false"
	}
	listed := "(" + f("listing") + " = 'listed' OR (" + f("listing") + " = 'members' AND " + opens + "))"
	if v.mode == access.Hide {
		listed += " AND " + opens
	}
	public := "(NOT " + f("deleted") + " AND " + live + " AND " + scope + " AND NOT (" + f("group_ids") + " && @cv_hidden::text[])" +
		" AND (@cv_language = '' OR @cv_language = ANY(" + f("languages") + ")) AND " + listed + ")"
	held := "(" + f("deleted") + " AND @cv_include_deleted::boolean AND " + live + " AND (" + f("content_id") + " = ANY(@cv_owned::text[]) OR " +
		f("version_id") + " = ANY(@cv_owned_versions::text[]) OR @cv_owned_candidates::boolean))"
	privileged := "(" + f("owner_group_id") + " = ANY(@cv_editors::text[]) OR (@cv_user <> '' AND EXISTS (SELECT 1 FROM " + v.s.t.creators +
		" cc WHERE cc.tenant_id = " + f("tenant_id") + " AND cc.content_id = " + f("content_id") + " AND cc.user_id = @cv_user)))"
	return "(" + public + " OR " + held + " OR " + privileged + ")", args
}

// facetRow is one version as the visibility rule reads it.
type facetRow struct {
	contentID, versionID, kind string
	liveFrom                   *time.Time
	// deleted is the version's own deletion; contentDeleted its content's.
	deleted, contentDeleted bool
	// held: the content's latest edit awaits moderation.
	held                   bool
	listing                string
	languages              []string
	rule                   access.Rule
	restrictions, groupIDs []string
	ownerGroup             string
	position               int
}

func (v *Viewer) live(r facetRow) bool { return r.liveFrom != nil && !r.liveFrom.After(v.now) }

func (v *Viewer) inScope(r facetRow) bool {
	if v.requireMissing {
		return false
	}
	for _, x := range r.restrictions {
		if slices.Contains(v.exclude, x) {
			return false
		}
	}
	for _, g := range v.require {
		if !slices.Contains(r.groupIDs, g) {
			return false
		}
	}
	return true
}

// itemView is the rule's verdict on one content item for the viewer.
type itemView struct {
	found, visible, editor bool
	// versions are the visible versions in position order, each with its
	// access decision.
	versions  []facetRow
	decisions map[string]Decision
}

// decide applies the rule to content items for direct reads: an editor (staff,
// creator, owner-group editor) sees everything; anyone else sees the live
// versions of live content in the site's scope, and a deleted one that was
// live if they hold its content or version key. Listing and content filters
// do not apply to direct reads. With every, each item's owner group is asked
// for editorship (a detail page); else only those of items the rule does not
// show whole or open.
func (v *Viewer) decide(ctx context.Context, ids []string, every bool) (map[string]*itemView, error) {
	s := v.s
	out := map[string]*itemView{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT f.content_id, f.version_id, f.kind, f.live_from, c.state = 'deleted', ver.state = 'deleted', c.moderation <> 'approved',
 f.listing, f.languages, c.access, ver.access, f.restrictions, f.group_ids, coalesce(f.owner_group_id, ''), f.position,
 EXISTS (SELECT 1 FROM `+s.t.creators+` cc WHERE cc.tenant_id = f.tenant_id AND cc.content_id = f.content_id AND cc.user_id = $3 AND $3 <> '')
 FROM `+s.t.facets+` f JOIN `+s.t.items+` c ON c.tenant_id = f.tenant_id AND c.id = f.content_id
 JOIN `+s.t.versions+` ver ON ver.tenant_id = f.tenant_id AND ver.id = f.version_id
 WHERE f.tenant_id = $1 AND f.content_id = ANY($2) ORDER BY f.content_id, f.position, f.version_id`, s.tenant, ids, viewerID(v.actor))
	if err != nil {
		return nil, err
	}
	byID := map[string][]facetRow{}
	creator := map[string]bool{}
	for rows.Next() {
		var r facetRow
		var isCreator bool
		var crule, vrule []byte
		if err := rows.Scan(&r.contentID, &r.versionID, &r.kind, &r.liveFrom, &r.contentDeleted, &r.deleted, &r.held, &r.listing, &r.languages,
			&crule, &vrule, &r.restrictions, &r.groupIDs, &r.ownerGroup, &r.position, &isCreator); err != nil {
			rows.Close()
			return nil, err
		}
		if r.rule, err = effectiveRule(crule, vrule); err != nil {
			rows.Close()
			return nil, fmt.Errorf("catalog: version %s: %w", r.versionID, err)
		}
		byID[r.contentID] = append(byID[r.contentID], r)
		creator[r.contentID] = creator[r.contentID] || isCreator
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Live versions are decided by their rule; deleted ones that were live
	// open only to holders of their own content or version key.
	known := func(id string) bool { return v.staff || creator[id] || v.editable(byID[id][0].ownerGroup) }
	var live, held []facetRow
	for id, rs := range byID {
		if known(id) {
			continue
		}
		for _, r := range rs {
			switch {
			case !v.live(r) || !v.inScope(r):
			case r.deleted || r.contentDeleted:
				held = append(held, r)
			default:
				live = append(live, r)
			}
		}
	}
	items := make([]access.Item, 0, len(live)+len(held))
	for _, r := range live {
		items = append(items, access.Item{Content: r.contentID, Version: r.versionID, Rule: r.rule, Groups: r.groupIDs})
	}
	for _, r := range held {
		items = append(items, access.Item{Content: r.contentID, Version: r.versionID, Rule: access.Rule{Level: access.PPV}})
	}
	ds, err := v.gateDecide(ctx, items)
	if err != nil {
		return nil, err
	}
	shown := map[string][]facetRow{}
	decisions := map[string]Decision{}
	locked := map[string]bool{}
	for i, r := range slices.Concat(live, held) {
		if !ds[i].Allowed {
			locked[r.contentID] = true
		}
		if i >= len(live) && !ds[i].Allowed {
			continue
		}
		shown[r.contentID] = append(shown[r.contentID], r)
		decisions[r.versionID] = decision(ds[i])
	}
	// Owner-group editorship is asked once per owner group: unless every,
	// only of items the rule does not show whole (drafts, scheduled, deleted,
	// held, out of the site's scope) or leaves a version locked, so an editor
	// is never offered a version of their group's.
	var groups []string
	for id, rs := range byID {
		g := rs[0].ownerGroup
		if g == "" || known(id) || slices.Contains(groups, g) {
			continue
		}
		if every || locked[id] || len(shown[id]) == 0 || slices.ContainsFunc(rs, func(r facetRow) bool { return !v.live(r) || !v.inScope(r) || r.deleted || r.contentDeleted || r.held }) {
			groups = append(groups, g)
		}
	}
	editors := v.Editors(ctx, groups)
	for id, rs := range byID {
		iv := &itemView{found: true, decisions: map[string]Decision{}}
		out[id] = iv
		if iv.editor = known(id) || editors[rs[0].ownerGroup]; iv.editor {
			iv.versions = rs
			for _, r := range rs {
				iv.decisions[r.versionID] = Decision{Allowed: true}
			}
			continue
		}
		iv.versions = shown[id]
		for _, r := range iv.versions {
			iv.decisions[r.versionID] = decisions[r.versionID]
		}
	}
	for _, iv := range out {
		iv.visible = iv.editor || len(iv.versions) > 0
		slices.SortFunc(iv.versions, func(a, b facetRow) int {
			if a.position != b.position {
				return a.position - b.position
			}
			return strings.Compare(a.versionID, b.versionID)
		})
	}
	return out, nil
}

// gateDecide decides items through the Gate. A failed billing read denies
// (Unknown) without failing the read.
func (v *Viewer) gateDecide(ctx context.Context, items []access.Item) ([]access.Decision, error) {
	if len(items) == 0 {
		return nil, nil
	}
	ds, err := v.s.accessG.Decide(ctx, v.actor, items)
	if err != nil && ds != nil && errors.Is(err, access.ErrUnavailable) {
		return ds, nil
	}
	return ds, err
}

func decision(d access.Decision) Decision {
	out := Decision{Allowed: d.Allowed, Unknown: d.Unknown, Preview: d.Preview, FreeAfter: d.FreeAfter}
	for _, k := range d.Sell {
		out.Sell = append(out.Sell, k.String())
	}
	if d.Requires != nil {
		out.Requires = d.Requires.String()
	}
	return out
}

// directViews decides content items for actor without a site (media and
// interactions know none).
func (s *Store) directViews(ctx context.Context, actor access.Actor, ids []string) (map[string]*itemView, error) {
	if len(ids) == 0 {
		return map[string]*itemView{}, nil
	}
	v, err := s.Viewer(ctx, ViewerOptions{Actor: actor})
	if err != nil {
		return nil, err
	}
	v.exclude, v.require, v.requireMissing = nil, nil, false
	return v.decide(ctx, ids, false)
}

// Resolver is the access.ContentResolver of content items for media
// (media/versions) and interactions: a content reference (kind, content) or
// one of its versions is visible by the rule for direct reads (live, deleted
// and held by its own keys, or edited by the viewer), accessible when the
// viewer opens it (a content reference: any of its visible versions), with
// staff, creators and owner-group editors as Editor and the first creator as
// Owner. A reference it omits is unseen.
func (s *Store) Resolver() access.ContentResolver { return resolver{s} }

type resolver struct{ s *Store }

func (r resolver) Resolve(ctx context.Context, refs []contentref.ContentRef, actor access.Actor) (map[contentref.ContentKey]access.Resolution, error) {
	s := r.s
	out := map[contentref.ContentKey]access.Resolution{}
	var ids []string
	for _, ref := range refs {
		if _, ok := s.cfg.Kinds[ref.ContentKind]; ok && ref.TenantID == s.tenant && !slices.Contains(ids, ref.ContentID) {
			ids = append(ids, ref.ContentID)
		}
	}
	views, err := s.directViews(ctx, actor, ids)
	if err != nil || len(views) == 0 {
		return out, err
	}
	owners, err := s.firstCreators(ctx, ids)
	if err != nil {
		return nil, err
	}
	for _, ref := range refs {
		iv := views[ref.ContentID]
		if ref.TenantID != s.tenant || iv == nil || !iv.visible || len(iv.versions) == 0 || iv.versions[0].kind != ref.ContentKind {
			continue
		}
		res := access.Resolution{Ref: ref, Editor: iv.editor, Owner: owners[ref.ContentID]}
		for _, ver := range iv.versions {
			if want := ref.Version(); want == "" || ver.versionID == want {
				res.Visible = true
				res.Accessible = res.Accessible || iv.decisions[ver.versionID].Allowed
			}
		}
		if res.Visible {
			out[ref.Key()] = res
		}
	}
	return out, nil
}

func (s *Store) firstCreators(ctx context.Context, ids []string) (map[string]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT ON (content_id) content_id, user_id FROM `+s.t.creators+`
 WHERE tenant_id = $1 AND content_id = ANY($2) ORDER BY content_id, position, user_id`, s.tenant, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, user string
		if err := rows.Scan(&id, &user); err != nil {
			return nil, err
		}
		out[id] = user
	}
	return out, rows.Err()
}
