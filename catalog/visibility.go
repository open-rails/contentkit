package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/kinds"
	"github.com/open-rails/contentkit/search"
)

// Mode is how a listing treats what the viewer cannot open: Lock lists it
// (marked locked), Hide leaves it out (a purchased library).
type Mode string

const (
	Lock Mode = "lock"
	Hide Mode = "hide"
)

// ViewerOptions describes one request's reader.
type ViewerOptions struct {
	Actor access.Actor
	// Host is the request's host; it picks the site (kinds.Config.SiteFor).
	Host string
	// Language, when set, lists only content with a live version serving it.
	Language string
	// Mode defaults to Lock.
	Mode Mode
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
	mode           Mode
	now            time.Time
	staff          bool
	filter         *access.Filter
	exclude        []string
	require        []string
	requireMissing bool
	hidden         []string
	includeDeleted bool
	editorGroups   []string
}

// Viewer resolves o once per request: the site, staff rights, entitlements
// (one billing read, fail-closed when unavailable), the site's required groups
// and the viewer's content filters (config defaults merged with their own).
func (s *Store) Viewer(ctx context.Context, o ViewerOptions) (*Viewer, error) {
	v := &Viewer{s: s, actor: o.Actor, language: o.Language, mode: o.Mode, now: s.now()}
	if v.mode == "" {
		v.mode = Lock
	}
	if v.mode != Lock && v.mode != Hide {
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
		return nil, err
	}
	v.staff = rights.view || rights.write
	if s.accessG != nil {
		f, err := s.accessG.Filter(ctx, o.Actor)
		if err != nil && !errors.Is(err, access.ErrUnavailable) {
			return nil, err
		}
		v.filter = f
	}
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
	if len(o.EditorGroups) > 0 {
		editors, err := s.groupEditor(ctx, s.pool, o.Actor, o.EditorGroups)
		if err != nil {
			return nil, err
		}
		for g, ok := range editors {
			if ok {
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

// Staff reports a viewer holding root:content:view-restricted or
// root:content:write: they see everything.
func (v *Viewer) Staff() bool { return v.staff }

// Site is the site the viewer is on.
func (v *Viewer) Site() kinds.Site { return v.site }

// Actor is the viewer's actor.
func (v *Viewer) Actor() access.Actor { return v.actor }

// keys are the viewer's held content and group keys, from the Filter.
type keys struct {
	tiers, owned, members []string
	incomplete            bool
}

func (v *Viewer) keys() keys {
	k := keys{tiers: []string{}, owned: []string{}, members: []string{}}
	f := v.filter
	if f == nil || f.Anonymous() {
		return k
	}
	for _, t := range v.s.cfg.Tiers {
		if f.Tier(t) {
			k.tiers = append(k.tiers, t)
		}
	}
	for kind := range v.s.cfg.Kinds {
		k.owned = append(k.owned, f.Owned(kind)...)
		k.incomplete = k.incomplete || (!f.Unknown() && !f.Complete(kind))
	}
	for kind, gk := range v.s.cfg.GroupKinds {
		if gk.Sellable {
			k.members = append(k.members, f.Members(kind)...)
			k.incomplete = k.incomplete || (!f.Unknown() && !f.Complete(kind))
		}
	}
	slices.Sort(k.owned)
	slices.Sort(k.members)
	return k
}

// Listed is a boolean SQL predicate over the content_facets row aliased
// alias: the versions the viewer's listing shows. A list shows one card per
// content item among its rows. In Hide mode, and for members-listed rows,
// rows a truncated keyspace may unlock are admitted as candidates: decide
// them with Contents (or access.Gate.Settle) before showing. Args are pgx
// named args prefixed cv_.
func (v *Viewer) Listed(alias string) (string, pgx.NamedArgs) {
	f := func(col string) string { return alias + "." + col }
	k := v.keys()
	args := pgx.NamedArgs{"cv_now": v.now, "cv_exclude": v.exclude, "cv_require": v.require, "cv_hidden": v.hidden,
		"cv_language": v.language, "cv_tiers": k.tiers, "cv_owned": k.owned, "cv_members": k.members,
		"cv_candidates": k.incomplete, "cv_user": viewerID(v.actor), "cv_editors": v.editorGroups, "cv_include_deleted": v.includeDeleted}
	if v.staff {
		return "true", args
	}
	opens := "(" + f("access_level") + " = 'public' OR (" + f("access_level") + " = 'tier' AND " + f("access_tiers") + " && @cv_tiers::text[])" +
		" OR (" + f("access_level") + " = 'group' AND " + f("access_group_id") + " = ANY(@cv_members::text[]))" +
		" OR " + f("content_id") + " = ANY(@cv_owned::text[]) OR (" + f("access_level") + " <> 'public' AND @cv_candidates::boolean))"
	live := f("live_from") + " <= @cv_now::timestamptz"
	scope := "NOT (" + f("restrictions") + " && @cv_exclude::text[]) AND " + f("group_ids") + " @> @cv_require::text[]"
	if v.requireMissing {
		scope = "false"
	}
	listed := "(" + f("listing") + " = 'listed' OR (" + f("listing") + " = 'members' AND " + opens + "))"
	if v.mode == Hide {
		listed += " AND " + opens
	}
	public := "(NOT " + f("deleted") + " AND " + live + " AND " + scope + " AND NOT (" + f("group_ids") + " && @cv_hidden::text[])" +
		" AND (@cv_language = '' OR @cv_language = ANY(" + f("languages") + ")) AND " + listed + ")"
	held := "(" + f("deleted") + " AND @cv_include_deleted::boolean AND " + live + " AND " + f("content_id") + " = ANY(@cv_owned::text[]))"
	privileged := "(" + f("owner_group_id") + " = ANY(@cv_editors::text[]) OR (@cv_user <> '' AND EXISTS (SELECT 1 FROM " + v.s.t.creators +
		" cc WHERE cc.tenant_id = " + f("tenant_id") + " AND cc.content_id = " + f("content_id") + " AND cc.user_id = @cv_user)))"
	return "(" + public + " OR " + held + " OR " + privileged + ")", args
}

// Eligibility is the search join that keeps content documents (one per
// content item and language) the viewer's listing shows; documents of other
// kinds pass.
func (v *Viewer) Eligibility() *search.Eligibility {
	pred, args := v.Listed("f")
	kinds := make([]string, 0, len(v.s.cfg.Kinds))
	for k := range v.s.cfg.Kinds {
		kinds = append(kinds, k)
	}
	slices.Sort(kinds)
	args["cv_kinds"] = kinds
	return &search.Eligibility{SQL: `SELECT 0 AS priority WHERE sd.content_kind <> ALL(@cv_kinds::text[]) OR EXISTS (SELECT 1 FROM ` + v.s.t.facets +
		` f WHERE f.tenant_id = sd.tenant_id AND f.content_id = sd.content_id AND ` + pred + `)`, Args: args}
}

// facetRow is one version as the visibility rule reads it.
type facetRow struct {
	contentID, versionID, kind string
	liveFrom                   *time.Time
	// deleted is the version's own deletion; contentDeleted its content's.
	deleted, contentDeleted bool
	listing                 string
	languages               []string
	level                   string
	tiers                   []string
	groupID                 string
	restrictions, groupIDs  []string
	ownerGroup              string
	position                int
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
// creator, owner-group editor) sees everything; anyone else sees live content
// in the site's scope with its live versions, and deleted content they hold
// the key of. Listing and content filters do not apply to direct reads.
func (v *Viewer) decide(ctx context.Context, ids []string) (map[string]*itemView, error) {
	s := v.s
	out := map[string]*itemView{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT f.content_id, f.version_id, f.kind, f.live_from, c.state = 'deleted', ver.state = 'deleted', f.listing, f.languages,
 f.access_level, f.access_tiers, coalesce(f.access_group_id, ''), f.restrictions, f.group_ids, coalesce(f.owner_group_id, ''), f.position,
 EXISTS (SELECT 1 FROM `+s.t.creators+` cc WHERE cc.tenant_id = f.tenant_id AND cc.content_id = f.content_id AND cc.user_id = $3 AND $3 <> '')
 FROM `+s.t.facets+` f JOIN `+s.t.items+` c ON c.tenant_id = f.tenant_id AND c.id = f.content_id
 JOIN `+s.t.versions+` ver ON ver.tenant_id = f.tenant_id AND ver.id = f.version_id
 WHERE f.tenant_id = $1 AND f.content_id = ANY($2) ORDER BY f.content_id, f.position, f.version_id`, s.tenant, ids, viewerID(v.actor))
	if err != nil {
		return nil, err
	}
	byID := map[string][]facetRow{}
	creator := map[string]bool{}
	var groups []string
	for rows.Next() {
		var r facetRow
		var isCreator bool
		if err := rows.Scan(&r.contentID, &r.versionID, &r.kind, &r.liveFrom, &r.contentDeleted, &r.deleted, &r.listing, &r.languages, &r.level,
			&r.tiers, &r.groupID, &r.restrictions, &r.groupIDs, &r.ownerGroup, &r.position, &isCreator); err != nil {
			rows.Close()
			return nil, err
		}
		byID[r.contentID] = append(byID[r.contentID], r)
		creator[r.contentID] = creator[r.contentID] || isCreator
		if r.ownerGroup != "" && !slices.Contains(groups, r.ownerGroup) {
			groups = append(groups, r.ownerGroup)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	editors, err := s.groupEditor(ctx, s.pool, v.actor, groups)
	if err != nil {
		return nil, err
	}
	type pending struct {
		id, version string
		item        access.Item
	}
	var asks []pending
	var heldAsks []pending
	for id, rs := range byID {
		iv := &itemView{found: true, decisions: map[string]Decision{}}
		out[id] = iv
		iv.editor = v.staff || creator[id] || editors[rs[0].ownerGroup]
		if iv.editor {
			iv.visible, iv.versions = true, rs
			for _, r := range rs {
				iv.decisions[r.versionID] = Decision{Allowed: true}
			}
			continue
		}
		for _, r := range rs {
			if v.live(r) && !r.deleted && v.inScope(r) {
				iv.versions = append(iv.versions, r)
			}
		}
		if len(iv.versions) == 0 {
			continue
		}
		if rs[0].contentDeleted {
			// Deleted content: only holders of its own key read it.
			heldAsks = append(heldAsks, pending{id: id, item: access.Item{Ref: s.Ref(rs[0].kind, id), Rule: access.Rule{Level: access.PPV}}})
		}
		iv.visible = true
		for _, r := range iv.versions {
			asks = append(asks, pending{id: id, version: r.versionID})
		}
	}
	if len(heldAsks) > 0 {
		items := make([]access.Item, len(heldAsks))
		for i, p := range heldAsks {
			items[i] = p.item
		}
		ds, err := v.gateDecide(ctx, items)
		if err != nil {
			return nil, err
		}
		for i, p := range heldAsks {
			if !ds[i].Allowed {
				out[p.id].visible, out[p.id].versions = false, nil
			}
		}
	}
	var items []access.Item
	var at []pending
	groupKinds, err := v.groupKinds(ctx, byID)
	if err != nil {
		return nil, err
	}
	for _, p := range asks {
		iv := out[p.id]
		if !iv.visible {
			continue
		}
		for _, r := range iv.versions {
			if r.versionID != p.version {
				continue
			}
			rule, ok := accessRule(r, s.tenant, groupKinds)
			if !ok {
				iv.decisions[r.versionID] = Decision{}
				continue
			}
			items, at = append(items, access.Item{Ref: s.Ref(r.kind, r.contentID), Rule: rule}), append(at, p)
		}
	}
	if len(items) > 0 {
		ds, err := v.gateDecide(ctx, items)
		if err != nil {
			return nil, err
		}
		for i, p := range at {
			out[p.id].decisions[p.version] = decision(ds[i])
		}
	}
	return out, nil
}

// gateDecide decides items through the Gate; without one only public items
// open. A failed billing read denies (Unknown) without failing the read.
func (v *Viewer) gateDecide(ctx context.Context, items []access.Item) ([]access.Decision, error) {
	if v.s.accessG == nil {
		out := make([]access.Decision, len(items))
		for i, it := range items {
			out[i].Allowed = it.Rule.Level == access.Public
		}
		return out, nil
	}
	ds, err := v.s.accessG.Decide(ctx, v.actor, items)
	if err != nil && ds != nil && errors.Is(err, access.ErrUnavailable) {
		return ds, nil
	}
	return ds, err
}

func (v *Viewer) groupKinds(ctx context.Context, byID map[string][]facetRow) (map[string]string, error) {
	var ids []string
	for _, rs := range byID {
		for _, r := range rs {
			if r.groupID != "" && !slices.Contains(ids, r.groupID) {
				ids = append(ids, r.groupID)
			}
		}
	}
	out := map[string]string{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := v.s.pool.Query(ctx, `SELECT id, kind FROM `+v.s.t.groups+` WHERE tenant_id = $1 AND id = ANY($2)`, v.s.tenant, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, kind string
		if err := rows.Scan(&id, &kind); err != nil {
			return nil, err
		}
		out[id] = kind
	}
	return out, rows.Err()
}

// accessRule maps a facet's rule onto access.Rule; false for a rule naming a
// group that no longer exists (it opens to nobody).
func accessRule(r facetRow, tenant string, groupKinds map[string]string) (access.Rule, bool) {
	switch r.level {
	case LevelPublic:
		return access.Rule{Level: access.Public}, true
	case LevelTier:
		return access.Rule{Level: access.TierLevel, Tiers: r.tiers}, true
	case LevelPPV:
		return access.Rule{Level: access.PPV}, true
	case LevelGroup, LevelGroupPPV:
		gk, ok := groupKinds[r.groupID]
		if !ok {
			return access.Rule{}, false
		}
		level := access.MembersLevel
		if r.level == LevelGroupPPV {
			level = access.MembersPPV
		}
		return access.Rule{Level: level, Scope: contentref.New(tenant, gk, r.groupID)}, true
	}
	return access.Rule{}, false
}

func decision(d access.Decision) Decision {
	out := Decision{Allowed: d.Allowed, Unknown: d.Unknown}
	for _, k := range d.Sell {
		out.Sell = append(out.Sell, k.String())
	}
	if d.Requires != nil {
		out.Requires = d.Requires.String()
	}
	return out
}

// Resolver is the access.ContentResolver of content items for interactions
// and media: a content reference (kind, content) or one of its versions is
// visible as the rule says for a direct read (no site scope: none is known),
// accessible when the viewer opens it (a content reference: any of its
// visible versions), with editors as Editor and the first creator as Owner.
func (s *Store) Resolver() access.ContentResolver { return resolver{s} }

type resolver struct{ s *Store }

func (r resolver) Resolve(ctx context.Context, refs []contentref.ContentRef, actor access.Actor) (map[contentref.ContentKey]access.Resolution, error) {
	s := r.s
	out := map[contentref.ContentKey]access.Resolution{}
	var ids []string
	for _, ref := range refs {
		if ref.TenantID == s.tenant && s.cfg.Kinds[ref.ContentKind].Route != "" && !slices.Contains(ids, ref.ContentID) {
			ids = append(ids, ref.ContentID)
		}
	}
	if len(ids) == 0 {
		return out, nil
	}
	v, err := s.Viewer(ctx, ViewerOptions{Actor: actor})
	if err != nil {
		return nil, err
	}
	v.exclude, v.require, v.requireMissing = nil, nil, false
	views, err := v.decide(ctx, ids)
	if err != nil {
		return nil, err
	}
	owners, err := s.firstCreators(ctx, ids)
	if err != nil {
		return nil, err
	}
	for _, ref := range refs {
		iv, ok := views[ref.ContentID]
		if ref.TenantID != s.tenant || !ok || len(iv.versions) == 0 || iv.versions[0].kind != ref.ContentKind {
			continue
		}
		res := access.Resolution{Ref: ref, Visible: iv.visible, Editor: iv.editor, Owner: owners[ref.ContentID]}
		if want := ref.Version(); want != "" {
			res.Visible = false
			for _, ver := range iv.versions {
				if ver.versionID == want {
					res.Visible, res.Accessible = iv.visible, iv.decisions[want].Allowed
				}
			}
		} else {
			for _, ver := range iv.versions {
				res.Accessible = res.Accessible || iv.decisions[ver.versionID].Allowed
			}
		}
		out[ref.Key()] = res
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
