package catalog

import (
	"context"
	"encoding/json"
	"slices"
	"time"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contenturl"
	"github.com/open-rails/contentkit/groups"
	"github.com/open-rails/contentkit/offers"
)

// Contents reads content items as v sees them (spec §4): an item the viewer
// may not see, or that does not exist, is nil. Text is in the viewer's
// language, else the site's first language, else the first it has; an item
// whose edit awaits review shows its last approved text to everyone but its
// editors. Editors also get every language's text, every version and the
// moderation state.
func (s *Store) Contents(ctx context.Context, v *Viewer, ids []string) ([]*Content, error) {
	out := make([]*Content, len(ids))
	views, err := v.decide(ctx, ids, true)
	if err != nil {
		return nil, err
	}
	var visible []string
	for _, id := range ids {
		if iv := views[id]; iv != nil && iv.visible && !slices.Contains(visible, id) {
			visible = append(visible, id)
		}
	}
	if len(visible) == 0 {
		return out, nil
	}
	items, err := s.readItems(ctx, visible)
	if err != nil {
		return nil, err
	}
	texts, err := s.readTexts(ctx, s.pool, visible)
	if err != nil {
		return nil, err
	}
	versions, err := s.readVersions(ctx, visible)
	if err != nil {
		return nil, err
	}
	creators, err := s.readCreators(ctx, visible)
	if err != nil {
		return nil, err
	}
	links, err := s.links(ctx, items)
	if err != nil {
		return nil, err
	}
	var groupIDs []string
	for _, id := range visible {
		for _, r := range views[id].versions {
			for _, g := range r.groupIDs {
				if !slices.Contains(groupIDs, g) {
					groupIDs = append(groupIDs, g)
				}
			}
		}
	}
	built := map[string]*Content{}
	images := s.imageSet()
	for _, id := range visible {
		iv, it := views[id], items[id]
		if it == nil {
			continue
		}
		c := &Content{ID: id, Kind: it.kind, State: it.state, Listing: it.listing, Comments: it.comments, PublishAt: it.publishAt, PublishedAt: it.publishedAt,
			Access: it.access, Restrictions: it.restrictions, Attributes: it.attributes, OwnerGroupID: it.ownerGroup, Creators: orEmpty(creators[id]),
			Groups: []GroupSummary{}, Versions: []Version{}, Editor: iv.editor, Revision: it.revision, CreatedAt: it.createdAt, UpdatedAt: it.updatedAt,
			DeletedAt: it.deletedAt}
		c.Live = slices.ContainsFunc(iv.versions, func(r facetRow) bool { return v.live(r) && !r.deleted && !r.contentDeleted })
		shown := texts[id]
		if !iv.editor && it.moderation != "approved" && it.snapshot != nil {
			shown = it.snapshot
		}
		lang := s.pickLanguage(v.language, shown)
		t := shown[lang]
		c.Language, c.Title, c.Slug, c.Description = lang, t.Title, t.Slug, t.Description
		if c.Decision = bundle(iv); c.Decision.Allowed {
			c.Body = t.Body
		}
		if iv.editor {
			c.Text = texts[id]
			if it.moderation != "approved" {
				c.Moderation, c.ModerationReason = it.moderation, it.moderationReason
			}
			c.EverSold = it.everSold
		}
		if it.cover != nil {
			c.Cover = &Cover{Image: it.cover.Image}
			images.add(it.kind, id, it.cover.Image)
		}
		for _, name := range imageNames(c.Body) {
			images.add(it.kind, id, name)
		}
		if l, ok := links[id]; ok {
			c.Code = string(l.Code)
			if route := s.cfg.Kinds[it.kind].Route; route != "" {
				c.Path = contenturl.Path(v.language, route, l.Code, l.SlugFor(lang))
			}
		}
		for _, r := range iv.versions {
			ver, ok := versions[r.versionID]
			if !ok {
				continue
			}
			out := *ver
			out.Live = v.live(r) && !r.deleted && !r.contentDeleted
			out.Decision = iv.decisions[r.versionID]
			if vt, ok := ver.Text[lang]; ok {
				out.Title, out.Description = vt.Title, vt.Description
			}
			if !iv.editor {
				out.Text, out.State, out.Access = nil, "", nil
			}
			out.Segments = released(out.Segments, v.now)
			c.Versions = append(c.Versions, out)
		}
		built[id] = c
	}
	if err := images.resolve(ctx); err != nil {
		return nil, err
	}
	names, err := s.groupSummaries(ctx, groupIDs, v.language)
	if err != nil {
		return nil, err
	}
	for id, c := range built {
		if c.Cover != nil {
			if u, ok := images.url(c.Kind, id, c.Cover.Image); ok {
				c.Cover.URL = &u
				c.images()[c.Cover.Image] = u
			}
		}
		for _, name := range imageNames(c.Body) {
			if u, ok := images.url(c.Kind, id, name); ok {
				c.images()[name] = u
			}
		}
		c.Body = resolveBody(c.Body, c.Images)
		var gs []string
		for _, r := range views[id].versions {
			for _, g := range r.groupIDs {
				if !slices.Contains(gs, g) {
					gs = append(gs, g)
				}
			}
		}
		for _, g := range gs {
			if n, ok := names[g]; ok {
				c.Groups = append(c.Groups, n)
			}
		}
		slices.SortFunc(c.Groups, func(a, b GroupSummary) int {
			if a.Kind != b.Kind {
				return compare(a.Kind, b.Kind)
			}
			return compare(a.Slug, b.Slug)
		})
	}
	if err := s.fillOffers(ctx, v, built); err != nil {
		return nil, err
	}
	if err := s.fillOwners(ctx, v.language, built); err != nil {
		return nil, err
	}
	for i, id := range ids {
		out[i] = built[id]
	}
	return out, nil
}

// fillOwners attaches the items' owner groups' cards, avatars and covers
// resolved, in one call.
func (s *Store) fillOwners(ctx context.Context, language string, built map[string]*Content) error {
	if s.groups == nil {
		return nil
	}
	var ids []groups.GroupID
	for _, c := range built {
		if c.OwnerGroupID != "" && !slices.Contains(ids, groups.GroupID(c.OwnerGroupID)) {
			ids = append(ids, groups.GroupID(c.OwnerGroupID))
		}
	}
	if len(ids) == 0 {
		return nil
	}
	cards, err := s.groups.Cards(ctx, ids, groups.CardOptions{Language: language, Covers: true})
	if err != nil {
		return err
	}
	for _, c := range built {
		for i := range cards {
			if string(cards[i].ID) == c.OwnerGroupID {
				c.Owner = &cards[i]
			}
		}
	}
	return nil
}

// fillOffers attaches every paywall's purchasable offers in one read and, on
// the items the viewer edits, their and their versions' current offers for
// offer editors in one more. A failed editor check leaves those out.
func (s *Store) fillOffers(ctx context.Context, v *Viewer, built map[string]*Content) error {
	if s.offers == nil {
		return nil
	}
	var walls []*Decision
	var keys []string
	var targets []offers.Target
	var put []**offers.Offer
	for _, c := range built {
		walls = append(walls, &c.Decision)
		if c.Editor {
			targets, put = append(targets, offers.Target{Kind: offers.ContentTarget, ID: c.ID}), append(put, &c.Offer)
		}
		for i := range c.Versions {
			ver := &c.Versions[i]
			walls = append(walls, &ver.Decision)
			if c.Editor {
				targets, put = append(targets, offers.Target{Kind: offers.VersionTarget, ID: ver.ID, Content: c.ID}), append(put, &ver.Offer)
			}
		}
	}
	for _, d := range walls {
		keys = append(keys, d.Sell...)
	}
	if len(keys) > 0 {
		sales, err := s.offers.Sales(ctx, keys)
		if err != nil {
			return err
		}
		for _, d := range walls {
			for _, k := range d.Sell {
				if sale, ok := sales[k]; ok {
					d.Offers = append(d.Offers, sale)
				}
			}
		}
	}
	if len(targets) == 0 {
		return nil
	}
	current, err := s.offers.Current(ctx, s.guard.Auth, v.actor, targets)
	if err != nil {
		s.log.WarnContext(ctx, "catalog: offer editor check failed; reading without offers", "err", err)
		return nil
	}
	for i, o := range current {
		*put[i] = o
	}
	return nil
}

// bundle is the viewer's access to an item's body: allowed when they edit it
// or open any of its versions, else its first version's paywall.
func bundle(iv *itemView) Decision {
	if iv.editor {
		return Decision{Allowed: true}
	}
	for _, r := range iv.versions {
		if iv.decisions[r.versionID].Allowed {
			return Decision{Allowed: true}
		}
	}
	if len(iv.versions) == 0 {
		return Decision{}
	}
	return iv.decisions[iv.versions[0].versionID]
}

func compare(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func (c *Content) images() map[string]string {
	if c.Images == nil {
		c.Images = map[string]string{}
	}
	return c.Images
}

// released marks each segment released while it and every earlier one are
// past their release time.
func released(ss []Segment, now time.Time) []Segment {
	out := make([]Segment, len(ss))
	open := true
	for i, sg := range ss {
		open = open && (sg.ReleaseAt == nil || !sg.ReleaseAt.After(now))
		sg.Released = open
		out[i] = sg
	}
	return out
}

func (s *Store) pickLanguage(want string, texts map[string]Text) string {
	if _, ok := texts[want]; ok && want != "" {
		return want
	}
	for _, l := range s.cfg.Languages {
		if _, ok := texts[l]; ok {
			return l
		}
	}
	if want != "" {
		return want
	}
	return s.defaultLanguage()
}

type itemRow struct {
	kind, state, listing, ownerGroup  string
	comments                          string
	moderation, moderationReason      string
	access                            access.Rule
	restrictions                      []string
	attributes                        map[string]any
	cover                             *Cover
	snapshot                          map[string]Text
	everSold                          bool
	publishAt, publishedAt, deletedAt *time.Time
	createdAt, updatedAt              time.Time
	revision                          int64
}

func (s *Store) readItems(ctx context.Context, ids []string) (map[string]*itemRow, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, kind, state, listing, coalesce(owner_group_id, ''), moderation, coalesce(moderation_reason, ''), access,
 restrictions, attributes, cover, published_snapshot, ever_sold, publish_at, published_at, deleted_at, created_at, updated_at, revision, comments
 FROM `+s.t.items+` WHERE tenant_id = $1 AND id = ANY($2)`, s.tenant, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]*itemRow{}
	for rows.Next() {
		var id string
		var r itemRow
		var access, attrs, cover, snapshot []byte
		if err := rows.Scan(&id, &r.kind, &r.state, &r.listing, &r.ownerGroup, &r.moderation, &r.moderationReason, &access, &r.restrictions,
			&attrs, &cover, &snapshot, &r.everSold, &r.publishAt, &r.publishedAt, &r.deletedAt, &r.createdAt, &r.updatedAt, &r.revision, &r.comments); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(access, &r.access); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(attrs, &r.attributes); err != nil {
			return nil, err
		}
		if cover != nil {
			if err := json.Unmarshal(cover, &r.cover); err != nil {
				return nil, err
			}
		}
		if snapshot != nil {
			if err := json.Unmarshal(snapshot, &r.snapshot); err != nil {
				return nil, err
			}
		}
		out[id] = &r
	}
	return out, rows.Err()
}

func (s *Store) readTexts(ctx context.Context, q Querier, ids []string) (map[string]map[string]Text, error) {
	rows, err := q.Query(ctx, `SELECT content_id, language, title, slug, description, body, sort_key, aliases FROM `+s.t.text+`
 WHERE tenant_id = $1 AND content_id = ANY($2)`, s.tenant, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]map[string]Text{}
	for rows.Next() {
		var id, l string
		var t Text
		if err := rows.Scan(&id, &l, &t.Title, &t.Slug, &t.Description, &t.Body, &t.SortKey, &t.Aliases); err != nil {
			return nil, err
		}
		if len(t.Aliases) == 0 {
			t.Aliases = nil
		}
		if out[id] == nil {
			out[id] = map[string]Text{}
		}
		out[id][l] = t
	}
	return out, rows.Err()
}

func (s *Store) readCreators(ctx context.Context, ids []string) (map[string][]Creator, error) {
	rows, err := s.pool.Query(ctx, `SELECT content_id, user_id, role, position FROM `+s.t.creators+`
 WHERE tenant_id = $1 AND content_id = ANY($2) ORDER BY content_id, position, user_id`, s.tenant, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]Creator{}
	for rows.Next() {
		var id string
		var c Creator
		if err := rows.Scan(&id, &c.UserID, &c.Role, &c.Position); err != nil {
			return nil, err
		}
		out[id] = append(out[id], c)
	}
	return out, rows.Err()
}

// readVersions reads every version of the items with its text and segments.
func (s *Store) readVersions(ctx context.Context, ids []string) (map[string]*Version, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, position, languages, traits, label, state, publish_at, published_at, access, attributes,
 pages, duration_ms, width, height, has_audio, ordinal FROM `+s.t.versions+` WHERE tenant_id = $1 AND content_id = ANY($2)`, s.tenant, ids)
	if err != nil {
		return nil, err
	}
	out := map[string]*Version{}
	var vids []string
	for rows.Next() {
		v := &Version{Segments: []Segment{}}
		var access, attrs []byte
		if err := rows.Scan(&v.ID, &v.Position, &v.Languages, &v.Traits, &v.Label, &v.State, &v.PublishAt, &v.PublishedAt, &access, &attrs,
			&v.Pages, &v.DurationMS, &v.Width, &v.Height, &v.HasAudio, &v.Ordinal); err != nil {
			rows.Close()
			return nil, err
		}
		if access != nil {
			if err := json.Unmarshal(access, &v.Access); err != nil {
				rows.Close()
				return nil, err
			}
		}
		if err := json.Unmarshal(attrs, &v.Attributes); err != nil {
			rows.Close()
			return nil, err
		}
		out[v.ID] = v
		vids = append(vids, v.ID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows, err = s.pool.Query(ctx, `SELECT version_id, language, title, description FROM `+s.t.versionText+` WHERE tenant_id = $1 AND version_id = ANY($2)`, s.tenant, vids)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var vid, l string
		var t VersionText
		if err := rows.Scan(&vid, &l, &t.Title, &t.Description); err != nil {
			rows.Close()
			return nil, err
		}
		v := out[vid]
		if v.Text == nil {
			v.Text = map[string]VersionText{}
		}
		v.Text[l] = t
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows, err = s.pool.Query(ctx, `SELECT version_id, position, title, start_index, end_index, release_at FROM `+s.t.segments+`
 WHERE tenant_id = $1 AND version_id = ANY($2) ORDER BY version_id, position`, s.tenant, vids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var vid string
		var sg Segment
		if err := rows.Scan(&vid, &sg.Position, &sg.Title, &sg.Start, &sg.End, &sg.ReleaseAt); err != nil {
			return nil, err
		}
		out[vid].Segments = append(out[vid].Segments, sg)
	}
	return out, rows.Err()
}

// links are the items' content codes and slugs.
func (s *Store) links(ctx context.Context, items map[string]*itemRow) (map[string]contenturl.Link, error) {
	var kinds, ids []string
	for id, it := range items {
		kinds, ids = append(kinds, it.kind), append(ids, id)
	}
	rows, err := s.pool.Query(ctx, `SELECT c.content_id, c.code, c.slug, c.slugs FROM `+s.qs+`.content_codes c
 JOIN unnest($2::text[], $3::text[]) AS r(kind, id) ON c.content_kind = r.kind AND c.content_id = r.id WHERE c.tenant_id = $1`, s.tenant, kinds, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]contenturl.Link{}
	for rows.Next() {
		var id, code string
		var l contenturl.Link
		var slugs []byte
		if err := rows.Scan(&id, &code, &l.Slug, &slugs); err != nil {
			return nil, err
		}
		l.Code = contenturl.Code(code)
		if err := json.Unmarshal(slugs, &l.Slugs); err != nil {
			return nil, err
		}
		out[id] = l
	}
	return out, rows.Err()
}

// groupSummaries names groups in language, falling back to any name.
func (s *Store) groupSummaries(ctx context.Context, ids []string, language string) (map[string]GroupSummary, error) {
	out := map[string]GroupSummary{}
	if len(ids) == 0 {
		return out, nil
	}
	if language == "" {
		language = s.defaultLanguage()
	}
	rows, err := s.pool.Query(ctx, `SELECT g.id, g.kind, g.slug, coalesce((SELECT n.name FROM `+s.t.groupNames+` n
 WHERE n.tenant_id = g.tenant_id AND n.group_id = g.id AND n.kind = 'name' ORDER BY n.language = $3 DESC, n.language = $4 DESC, n.language LIMIT 1), g.slug)
 FROM `+s.t.groups+` g WHERE g.tenant_id = $1 AND g.id = ANY($2) AND g.state = 'active'`, s.tenant, ids, language, s.defaultLanguage())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var g GroupSummary
		if err := rows.Scan(&g.ID, &g.Kind, &g.Slug, &g.Name); err != nil {
			return nil, err
		}
		out[g.ID] = g
	}
	return out, rows.Err()
}
