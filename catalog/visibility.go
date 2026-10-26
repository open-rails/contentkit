package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/domainpolicy"
	"github.com/open-rails/contentkit/internal/gate"
	"github.com/open-rails/contentkit/internal/readerprefs"
	"github.com/open-rails/contentkit/internal/resolve"
	"github.com/open-rails/contentkit/kinds"
)

// ViewerOptions describes one request's reader.
type ViewerOptions struct {
	Actor access.Actor
	// Host is the request's host; it picks the site (kinds.Config.DomainFor).
	Host string
	// Language, when set, lists only content with a live version serving it.
	Language string
	// Mode defaults to access.Lock.
	Mode access.Mode
	// EditorEntities are owner entities (channels) whose editors see their whole
	// content in lists; the viewer's editorship of each is checked.
	EditorEntities []string
	// Requested are entities a list requires (an entity's page): the viewer's
	// content filters do not hide them, so the page shows what the entity's
	// count counts.
	Requested []string
}

// Viewer is the one visibility rule (ContentKit #117 spec §4) for one
// request: live content and versions, the site scope, the
// viewer's content filters and entitlements, and the staff, authenticated authors and
// owner-entity editors who see everything. Listed renders it as SQL over
// content_facets for lists and searches; Store.Contents and Resolver decide
// items with it.
type Viewer struct {
	s              *Store
	actor          access.Actor
	domain         kinds.Domain
	language       string
	mode           access.Mode
	now            time.Time
	staff          bool
	staffWrite     bool
	ownRead        ownAccess
	ownWrite       ownAccess
	management     bool
	filter         *access.Filter
	policies       domainpolicy.Policies
	hidden         map[string][]string
	includeDeleted bool
	editorEntities []string
	// asked are owner entities whose editorship this request already checked.
	asked map[string]bool
}

// Viewer resolves o once per request: the site, staff rights, entitlements
// (one billing read, fail-closed when unavailable), the site's required entities
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
	v.domain, _ = s.cfg.DomainFor(o.Host)
	rights, err := s.staffRights(ctx, o.Actor)
	if err != nil {
		// A read shows what the rule shows anyone; staff extras wait.
		s.log.WarnContext(ctx, "catalog: staff check failed; reading as a non-staff viewer", "err", err)
	}
	v.staff = rights.view || rights.write
	v.staffWrite = rights.write
	if !v.staffWrite {
		v.ownRead, err = s.ownAccess(ctx, o.Actor, false)
		if err != nil {
			s.log.WarnContext(ctx, "catalog: own-read authority unavailable", "err", err)
			v.ownRead = ownAccess{}
		}
		v.ownWrite, err = s.ownAccess(ctx, o.Actor, true)
		if err != nil {
			s.log.WarnContext(ctx, "catalog: own-edit authority unavailable", "err", err)
			v.ownWrite = ownAccess{}
		}
	}
	f, err := s.accessG.Filter(ctx, o.Actor)
	if err != nil && !errors.Is(err, access.ErrUnavailable) {
		return nil, err
	}
	v.filter = f
	v.policies, err = domainpolicy.Resolve(ctx, s.pool, s.t.entities, s.tenant, v.domain, s.cfg.ContentFilters.AppliesTo)
	if err != nil {
		return nil, err
	}
	allowed := map[string]bool{}
	for _, p := range v.policies {
		for id := range p.Toggles {
			allowed[id] = true
		}
	}
	values := map[string]string{}
	if o.Actor.Member() {
		var raw []byte
		err := s.pool.QueryRow(ctx, `SELECT overrides,include_deleted FROM `+s.t.prefs+` WHERE tenant_id=$1 AND user_id=$2`, s.tenant, o.Actor.AccountID()).Scan(&raw, &v.includeDeleted)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &values); err != nil {
				return nil, err
			}
		}
	} else if o.Actor.Anonymous {
		choices, err := readerprefs.Overrides(ctx, allowed)
		if err != nil {
			return nil, invalid("%s", err)
		}
		for key, value := range choices {
			values[key] = value
		}
	}
	v.hidden = map[string][]string{}
	for kind, p := range v.policies {
		for id, defaultVisible := range p.Toggles {
			visible, overridden := readerprefs.Stored(values, id)
			if !overridden {
				visible = defaultVisible
			}
			if !visible && !slices.Contains(o.Requested, id) {
				v.hidden[kind] = append(v.hidden[kind], id)
			}
		}
	}
	v.editorEntities = []string{}
	if len(o.EditorEntities) > 0 && !v.staff {
		got := s.readEditors(ctx, o.Actor, o.EditorEntities)
		v.asked = map[string]bool{}
		for _, g := range o.EditorEntities {
			if v.asked[g] = got[g]; got[g] {
				v.editorEntities = append(v.editorEntities, g)
			}
		}
		slices.Sort(v.editorEntities)
	}
	return v, nil
}

// editable reports an owner entity the request already knows the viewer
// edits (ViewerOptions.EditorEntities).
func (v *Viewer) editable(entity string) bool { return slices.Contains(v.editorEntities, entity) }

// Editors reports, per owner entity, whether the viewer edits its content and
// so opens it: staff edit every entity's; anyone else is asked once per entity
// per request, and a failed check reads as not an editor.
func (v *Viewer) Editors(ctx context.Context, entities []string) map[string]bool {
	out := map[string]bool{}
	var ask []string
	for _, g := range entities {
		switch {
		case g == "":
		case v.staffWrite || v.editable(g):
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

// CanPostToChannel checks current destination posting authority. It grants no
// authority over an original referenced by a repost.
func (v *Viewer) CanPostToChannel(ctx context.Context, id string) (bool, error) {
	if v.staffWrite {
		return true, nil
	}
	got, err := v.s.entityAllowed(ctx, v.s.pool, v.actor, []string{id}, true)
	return got[id], err
}

// Staff reports a viewer holding root:content:view-restricted or
// root:content:write: they see everything inside the domain boundary.
func (v *Viewer) Staff() bool { return v.staff }

// Domain is the request entrypoint and its visibility policy.
func (v *Viewer) Domain() kinds.Domain { return v.domain }

// Actor is the viewer's actor.
func (v *Viewer) Actor() access.Actor { return v.actor }

// HiddenContentFilters copies the optional, public entity filters currently
// hidden by this viewer, by content kind. Required, fixed and internal rules
// are domain policy, never optional filters.
func (v *Viewer) HiddenContentFilters() map[string][]string {
	out := make(map[string][]string, len(v.hidden))
	for kind, ids := range v.hidden {
		out[kind] = slices.Clone(ids)
		slices.Sort(out[kind])
	}
	return out
}

// WithoutContentFilters returns a request-local viewer with only optional
// entity preferences relaxed. The domain, entitlements, language, access mode
// and captured clock remain the original request's. It never relaxes required,
// fixed or internal domain policy.
func (v *Viewer) WithoutContentFilters() *Viewer {
	out := *v
	out.hidden = nil
	out.asked = maps.Clone(v.asked)
	return &out
}

// Listed is a boolean SQL predicate over the content_facets row aliased
// alias: the versions the viewer's listing shows. A list shows one card per
// content item among its rows. In Hide mode,
// rows a truncated keyspace may unlock are admitted as candidates: decide
// them with Contents (or access.Gate.Settle) before showing. Args are pgx
// named args prefixed cv_ and access_.
func (v *Viewer) Listed(alias string) (string, pgx.NamedArgs) { return v.listed(alias, true) }

// Shown is Listed in every language: the versions a content's page offers
// the viewer, among which its language's default edition opens. Its args are
// Listed's.
func (v *Viewer) Shown(alias string) (string, pgx.NamedArgs) { return v.listed(alias, false) }

func (v *Viewer) listed(alias string, language bool) (string, pgx.NamedArgs) {
	f := func(col string) string { return alias + "." + col }
	opens, accessArgs, err := v.filter.SQL(alias, access.Hide)
	if err != nil {
		// alias is the caller's constant; a bad one lists nothing.
		opens, accessArgs = "false", nil
	}
	versions := slices.Sorted(maps.Keys(v.filter.Versions()))
	contents := make([]string, len(versions))
	for i, version := range versions {
		contents[i] = v.filter.Versions()[version]
	}
	args := pgx.NamedArgs{"cv_now": v.now,
		"cv_language": v.language, "cv_owned_contents": contents, "cv_owned_versions": versions,
		"cv_owned_candidates": !v.filter.Complete() && !v.filter.Unknown(), "cv_user": viewerID(v.actor), "cv_editors": v.editorEntities,
		"cv_include_deleted": v.includeDeleted, "cv_own_app": v.ownRead.app, "cv_own_channels": v.ownRead.entities}
	for k, a := range accessArgs {
		args[k] = a
	}
	live := f("live_from") + " <= @cv_now::timestamptz"
	scope, scopeArgs := v.policies.SQL(alias, "cv_domain")
	for k, a := range scopeArgs {
		args[k] = a
	}
	soft, softArgs := domainpolicy.HiddenSQL(v.hidden, alias, "cv_hidden")
	for k, a := range softArgs {
		args[k] = a
	}
	if v.staff {
		if v.management {
			return scope, args
		}
		return "(" + scope + " AND " + soft + " AND " + v.channelReady(alias) + ")", args
	}
	public := "(NOT " + f("deleted") + " AND " + live + " AND " + scope + " AND " + soft
	if v.mode == access.Hide {
		public += " AND " + opens
	}
	public += ")"
	if language {
		public = "(" + public + " AND (@cv_language = '' OR @cv_language = ANY(" + f("languages") + ")))"
	}
	held := "(" + f("deleted") + " AND @cv_include_deleted::boolean AND " + live + " AND (EXISTS (" +
		"SELECT 1 FROM unnest(@cv_owned_contents::text[], @cv_owned_versions::text[]) AS owned(content_id,version_id) WHERE owned.content_id=" +
		f("content_id") + " AND owned.version_id=" + f("version_id") + ") OR @cv_owned_candidates::boolean))"
	return "(" + scope + " AND " + v.channelReady(alias) + " AND (" + public + " OR " + held + " OR " + v.privileged(alias) + "))", args
}

// channelReady keeps incomplete or withdrawn authoring homes out of public
// readers and discovery. Deleted rows may still reach the existing exact-version
// purchase retention check; this predicate alone never grants access. Explicit
// staff management still inspects pending records.
func (v *Viewer) channelReady(alias string) string {
	return "(" + alias + ".owner_entity_id IS NULL OR EXISTS(SELECT 1 FROM " + v.s.t.entities + " source_channel WHERE source_channel.tenant_id=" + alias + ".tenant_id AND source_channel.id=" + alias + ".owner_entity_id AND source_channel.kind='channel' AND ((source_channel.state='active' AND source_channel.auth_scope IS NOT NULL) OR (source_channel.state='deleted' AND " + alias + ".deleted))))"
}

// privileged is the SQL test, over Listed's args, of a row whose channel the
// viewer edits or whose content they created.
func (v *Viewer) privileged(alias string) string {
	return "(" + alias + ".owner_entity_id = ANY(@cv_editors::text[]) OR (@cv_user <> '' AND EXISTS (SELECT 1 FROM " + v.s.t.items +
		" authored WHERE authored.tenant_id = " + alias + ".tenant_id AND authored.id = " + alias + ".content_id AND authored.created_by = @cv_user AND ((authored.owner_entity_id IS NULL AND @cv_own_app::boolean) OR authored.owner_entity_id=ANY(@cv_own_channels::text[])))))"
}

// Playable is a boolean SQL predicate over the content_facets row aliased
// alias, to AND with Listed (it uses Listed's args): versions that play, or
// that the viewer may see before they do as staff, channel editor or
// creator. Browsing applies it; a reader's own history and saved items do
// not.
func (v *Viewer) Playable(alias string) string {
	if v.staff {
		return "true"
	}
	return "(NOT " + alias + ".unplayable OR " + v.privileged(alias) + ")"
}

// Indexable is a boolean SQL predicate over the content_facets row aliased
// alias: the versions a site lists to anyone, as its sitemaps name them:
// live, not deleted, playable and in the viewer's site scope.
// Access, the language and a reader's content filters do not apply. Args are
// pgx named args prefixed ci_.
func (v *Viewer) Indexable(alias string) (string, pgx.NamedArgs) {
	scope, args := v.policies.SQL(alias, "ci_domain")
	args["ci_now"] = v.now
	return "(NOT " + alias + ".deleted AND " + alias + ".live_from <= @ci_now::timestamptz AND NOT " + alias +
		".unplayable AND " + scope + " AND " + v.channelReady(alias) + ")", args
}

// facetRow is one version as the visibility rule reads it.
type facetRow struct {
	contentID, versionID, kind string
	liveFrom                   *time.Time
	// deleted is the version's own deletion; contentDeleted its content's.
	deleted, contentDeleted bool
	languages               []string
	rule                    access.Rule
	restrictions, entityIDs []string
	ownerEntity             string
	position                int
}

func (v *Viewer) live(r facetRow) bool { return r.liveFrom != nil && !r.liveFrom.After(v.now) }

func (v *Viewer) inScope(r facetRow) bool { return v.policies.Allows(r.kind, r.entityIDs) }

// itemView is the rule's verdict on one content item for the viewer.
type itemView struct {
	found, visible, editor, readRestricted bool
	// versions are the visible versions in position order, each with its
	// access decision.
	versions  []facetRow
	decisions map[string]Decision
}

// decide applies the rule to content items for direct reads: an editor (staff,
// creator, owner-entity editor) sees everything; anyone else sees the live
// versions of live content in the site's scope, and a deleted one that was
// live if they hold its exact version key. Optional content filters
// do not apply to direct reads. With every, each item's owner entity is asked
// for editorship (a detail page); else only those of items the rule does not
// show whole or open.
func (v *Viewer) decide(ctx context.Context, ids []string, every bool) (map[string]*itemView, error) {
	s := v.s
	out := map[string]*itemView{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT f.content_id, f.version_id, f.kind, f.live_from, c.state = 'deleted', ver.state = 'deleted',
 f.languages, c.access, CASE WHEN ver.released THEN ver.released_access ELSE ver.access END, f.restrictions, f.entity_ids, coalesce(f.owner_entity_id, ''), f.position,
 (coalesce(c.created_by, '') = $3 AND $3 <> '')
 FROM `+s.t.facets+` f JOIN `+s.t.items+` c ON c.tenant_id = f.tenant_id AND c.id = f.content_id
 JOIN `+s.t.versions+` ver ON ver.tenant_id = f.tenant_id AND ver.id = f.version_id
 WHERE f.tenant_id = $1 AND f.content_id = ANY($2) AND ($4 OR `+v.channelReady("f")+`)
 ORDER BY f.content_id, f.position, f.version_id`, s.tenant, ids, viewerID(v.actor), v.management && v.staff)
	if err != nil {
		return nil, err
	}
	byID := map[string][]facetRow{}
	creator := map[string]bool{}
	for rows.Next() {
		var r facetRow
		var isCreator bool
		var crule, vrule []byte
		if err := rows.Scan(&r.contentID, &r.versionID, &r.kind, &r.liveFrom, &r.contentDeleted, &r.deleted, &r.languages,
			&crule, &vrule, &r.restrictions, &r.entityIDs, &r.ownerEntity, &r.position, &isCreator); err != nil {
			rows.Close()
			return nil, err
		}
		if r.rule, err = effectiveRule(crule, vrule); err != nil {
			rows.Close()
			return nil, fmt.Errorf("catalog: version %s: %w", r.versionID, err)
		}
		r.rule = r.rule.Previewed(s.cfg.Kinds[r.kind])
		byID[r.contentID] = append(byID[r.contentID], r)
		creator[r.contentID] = creator[r.contentID] || isCreator
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Live versions are decided by their rule; deleted ones that were live
	// open only to holders of their own exact version key.
	personal := func(id string, write bool) bool {
		if !creator[id] {
			return false
		}
		if write {
			return v.ownWrite.allows(byID[id][0].ownerEntity)
		}
		return v.ownRead.allows(byID[id][0].ownerEntity)
	}
	known := func(id string) bool { return v.staff || personal(id, false) || v.editable(byID[id][0].ownerEntity) }
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
		items = append(items, access.Item{Content: r.contentID, Version: r.versionID, Rule: r.rule})
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
	// Owner-entity editorship is asked once per owner entity: unless every,
	// only of items the rule does not show whole (drafts, scheduled, deleted,
	// held, out of the site's scope) or leaves a version locked, so an editor
	// is never offered a version of their entity's.
	var entities []string
	for id, rs := range byID {
		g := rs[0].ownerEntity
		if g == "" || known(id) || slices.Contains(entities, g) {
			continue
		}
		if every || locked[id] || len(shown[id]) == 0 || slices.ContainsFunc(rs, func(r facetRow) bool { return !v.live(r) || !v.inScope(r) || r.deleted || r.contentDeleted }) {
			entities = append(entities, g)
		}
	}
	editors := v.Editors(ctx, entities)
	for id, rs := range byID {
		iv := &itemView{found: true, decisions: map[string]Decision{}}
		out[id] = iv
		iv.editor = v.staffWrite || personal(id, true) || v.editable(rs[0].ownerEntity) || editors[rs[0].ownerEntity]
		iv.readRestricted = known(id) || editors[rs[0].ownerEntity]
		if iv.readRestricted {
			for _, r := range rs {
				if !v.management && !v.inScope(r) {
					continue
				}
				iv.versions = append(iv.versions, r)
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
		iv.visible = len(iv.versions) > 0
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
	out := Decision{Allowed: d.Allowed, Unknown: d.Unknown, Preview: d.Preview, Releases: d.Releases}
	for _, k := range d.Sell {
		out.Sell = append(out.Sell, k.String())
	}
	if d.Requires != nil {
		out.Requires = d.Requires.String()
	}
	return out
}

// directViews decides content items for actor on the request's site (the
// host the route gate bound to ctx; the first site outside a request).
func (s *Store) directViews(ctx context.Context, actor access.Actor, ids []string) (map[string]*itemView, error) {
	if len(ids) == 0 {
		return map[string]*itemView{}, nil
	}
	v, err := s.Viewer(ctx, ViewerOptions{Actor: actor, Host: gate.Domain(ctx)})
	if err != nil {
		return nil, err
	}
	v.management = resolve.EditorRead(ctx)
	return v.decide(ctx, ids, false)
}

// Resolver is the resolve.Resolver of content items for media
// (media/versions) and interactions: a content reference (kind, content) or
// one of its versions is visible by the rule for direct reads (live, deleted
// and held by its own keys, or edited by the viewer), accessible when the
// viewer opens it (a content reference: any of its visible versions), with
// staff, authenticated authors and owner-entity editors as Editor and the author as
// Owner. A reference it omits is unseen.
func (s *Store) Resolver() resolve.Resolver { return resolver{s} }

type resolver struct{ s *Store }

func (r resolver) Resolve(ctx context.Context, refs []contentref.ContentRef, actor access.Actor) (map[contentref.ContentKey]resolve.Resolution, error) {
	s := r.s
	out := map[contentref.ContentKey]resolve.Resolution{}
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
	owners, err := s.owners(ctx, ids)
	if err != nil {
		return nil, err
	}
	for _, ref := range refs {
		iv := views[ref.ContentID]
		if ref.TenantID != s.tenant || iv == nil || !iv.visible || len(iv.versions) == 0 || iv.versions[0].kind != ref.ContentKind {
			continue
		}
		o := owners[ref.ContentID]
		res := resolve.Resolution{Ref: ref, Editor: iv.editor, ReadRestricted: iv.readRestricted, Owner: o.creator, Comments: o.comments, Channel: o.entity}
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

// ownership is a content item's authenticated author, comments setting and active
// user-owned owner entity.
type ownership struct {
	creator, comments string
	entity            *resolve.Channel
}

func (s *Store) owners(ctx context.Context, ids []string) (map[string]ownership, error) {
	rows, err := s.pool.Query(ctx, `SELECT c.id, c.comments, CASE WHEN c.owner_entity_id IS NULL THEN coalesce(c.created_by, '') ELSE '' END, coalesce(g.id, ''), coalesce(g.kind, ''), coalesce(g.owner_user_id, ''), coalesce(g.auth_scope, '')
 FROM `+s.t.items+` c LEFT JOIN `+s.t.entities+` g ON g.tenant_id = c.tenant_id AND g.id = c.owner_entity_id AND g.state = 'active'
 WHERE c.tenant_id = $1 AND c.id = ANY($2)`, s.tenant, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]ownership{}
	for rows.Next() {
		var id string
		var o ownership
		var g resolve.Channel
		if err := rows.Scan(&id, &o.comments, &o.creator, &g.ID, &g.Kind, &g.Owner, &g.Scope); err != nil {
			return nil, err
		}
		if g.ID != "" && g.Kind == "channel" {
			if g.Scope == "" {
				// Keep the channel ban boundary while refusing the owner fallback.
				g.Owner = ""
			}
			o.entity = &g
		}
		out[id] = o
	}
	return out, rows.Err()
}
