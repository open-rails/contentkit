package catalog

import (
	"context"
	"encoding/json"
	"slices"
	"time"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contenturl"
	"github.com/open-rails/contentkit/entities"
	"github.com/open-rails/contentkit/internal/resolve"
	"github.com/open-rails/contentkit/kinds"
	"github.com/open-rails/contentkit/media/versions"
)

// Contents reads content items as v sees them (spec §4): an item the viewer
// may not see, or that does not exist, is nil. Text is in the viewer's
// language, else the site's first language, else the first it has. Editors
// also get every language's text and every version.
func (s *Store) Contents(ctx context.Context, v *Viewer, ids []string) ([]*Content, error) {
	return s.contents(ctx, v, ids, "")
}

// contents is Contents; a non-empty vid limits the items' versions to those
// with that ID or version code.
func (s *Store) contents(ctx context.Context, v *Viewer, ids []string, vid string) ([]*Content, error) {
	if v.management {
		ctx = resolve.WithEditorRead(ctx)
	}
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
	versions, err := s.readVersions(ctx, visible, vid)
	if err != nil {
		return nil, err
	}
	if vid != "" {
		// A specific edition must not inherit another edition's permission to
		// disclose the body or locked metadata through the aggregate verdict.
		for id, iv := range views {
			selected := *iv
			selected.versions = slices.DeleteFunc(slices.Clone(iv.versions), func(row facetRow) bool {
				return versions[row.versionID] == nil
			})
			selected.visible = iv.visible && len(selected.versions) > 0
			views[id] = &selected
		}
		visible = slices.DeleteFunc(visible, func(id string) bool { return !views[id].visible })
		if len(visible) == 0 {
			return out, nil
		}
	}
	creators, err := s.readCreators(ctx, visible)
	if err != nil {
		return nil, err
	}
	links, err := s.links(ctx, items)
	if err != nil {
		return nil, err
	}
	var entityIDs []string
	for _, id := range visible {
		for _, r := range views[id].versions {
			for _, g := range r.entityIDs {
				if !slices.Contains(entityIDs, g) {
					entityIDs = append(entityIDs, g)
				}
			}
		}
	}
	built := map[string]*Content{}
	for _, id := range visible {
		iv, it := views[id], items[id]
		if it == nil {
			continue
		}
		c := &Content{ID: id, Kind: it.kind, State: it.state, Comments: it.comments, PublishAt: it.publishAt, PublishedAt: it.publishedAt,
			Access: it.access, Restrictions: it.restrictions, Attributes: map[string]any{}, ChannelID: it.ownerEntity, Creators: orEmpty(creators[id]),
			Entities: []EntitySummary{}, Versions: []Version{}, Editor: iv.editor, Revision: it.revision, CreatedAt: it.createdAt,
			UpdatedAt: it.updatedAt, DeletedAt: it.deletedAt}
		c.Live = v.itemLive(iv)
		c.Decision = bundle(iv)
		if s.shows(it.kind, c.Decision, kinds.ShowAttributes) {
			c.Attributes = it.attributes
		}
		if l, ok := links[id]; ok {
			page := contenturl.Prefix(s.cfg.Languages, v.language)
			c.Code, c.Slug, c.Slugs = string(l.Code), l.SlugFor(page), l.Slugs
			if route := s.cfg.Kinds[it.kind].Route; route != "" {
				c.Path = contenturl.Path(s.cfg.Languages, page, route, l.Code, c.Slug)
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
			s.resolveVersionText(it.kind, &out, v.language)
			if !out.Decision.Allowed && !iv.editor {
				out.Body = ""
			}
			if !iv.editor {
				out.Text, out.State, out.Access = nil, "", nil
				out.Publication = nil
				out.Segments = append([]Segment{}, out.readerSegments...)
				for i := range out.Segments {
					out.Segments[i].StartPage = ""
				}
			}
			out.Segments = released(out.Segments)
			s.lockVersion(it.kind, &out)
			c.Versions = append(c.Versions, out)
		}
		if selected := DefaultVersion(c.Versions, v.language); selected != nil {
			c.SelectedVersionID = selected.ID
			c.Title, c.Description, c.Body, c.Language = selected.Title, selected.Description, selected.Body, selected.Language
			c.OriginalTitle, c.OriginalLanguage = selected.OriginalTitle, selected.OriginalLanguage
			c.Decision = selected.Decision
		}
		built[id] = c
	}
	if err := s.fillCovers(ctx, built, v.language); err != nil {
		return nil, err
	}
	names, err := s.entitySummaries(ctx, entityIDs, v.language, v.management && v.staff)
	if err != nil {
		return nil, err
	}
	scopes, err := s.entityScopes(ctx, visible)
	if err != nil {
		return nil, err
	}
	for id, c := range built {
		// Each edition resolves only its own private image references.
		for i := range c.Versions {
			edition := &c.Versions[i]
			names := imageNames(edition.Body)
			if len(names) == 0 || s.media == nil || s.media.Reader == nil {
				continue
			}
			ref, err := s.mediaRef(c.Kind, id, edition.ID)
			if err != nil {
				return nil, err
			}
			urls, err := s.media.Reader.NamedURLs(ctx, ref, v.actor, names)
			if err != nil {
				s.log.WarnContext(ctx, "catalog: edition body image URLs unavailable", "content", id, "version", edition.ID, "err", err)
				continue
			}
			edition.Images = urls
			if edition.ID == c.SelectedVersionID {
				c.Images = urls
			}
		}
		if s.shows(c.Kind, c.Decision, kinds.ShowEntities) {
			c.Entities = scopedEntities(scopes[entityScope{content: id}], names)
		}
		for i := range c.Versions {
			ver := &c.Versions[i]
			ver.Entities = []EntitySummary{}
			if s.shows(c.Kind, ver.Decision, kinds.ShowEntities) {
				ver.Entities = scopedEntities(scopes[entityScope{content: id, version: ver.ID}], names)
			}
		}
	}
	if err := s.fillOwners(ctx, v.language, built, v.management && v.staff); err != nil {
		return nil, err
	}
	if !v.management || !v.staff {
		if err := s.redactInternalEntities(ctx, built); err != nil {
			return nil, err
		}
	}
	for i, id := range ids {
		out[i] = built[id]
	}
	return out, nil
}

// itemLive reports whether any of an item's visible versions is live.
func (v *Viewer) itemLive(iv *itemView) bool {
	return slices.ContainsFunc(iv.versions, func(r facetRow) bool { return v.live(r) && !r.deleted && !r.contentDeleted })
}

// fillOwners attaches the items' owner entities' cards, avatars and covers
// resolved, in one call.
func (s *Store) fillOwners(ctx context.Context, language string, built map[string]*Content, management bool) error {
	if s.channels == nil {
		return nil
	}
	var ids []string
	for _, c := range built {
		if c.ChannelID != "" && !slices.Contains(ids, c.ChannelID) {
			ids = append(ids, c.ChannelID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	cards, err := s.channels.Cards(ctx, ids, language)
	if err != nil {
		return err
	}
	for _, c := range built {
		for i := range cards {
			if string(cards[i].ID) == c.ChannelID {
				c.Channel = &cards[i]
			}
		}
	}
	return nil
}

// shows reports whether a viewer decided d sees property p of content of
// kind: all of what they open, of the rest what the kind's locked view shows.
func (s *Store) shows(kind string, d Decision, p string) bool {
	return d.Allowed || s.cfg.Kinds[kind].LockedView().Shows(p)
}

// lockVersion withholds from a version its viewer does not open what its
// kind's locked view hides.
func (s *Store) lockVersion(kind string, v *Version) {
	if !s.shows(kind, v.Decision, kinds.ShowCover) {
		v.Cover = nil
	}
	if !s.shows(kind, v.Decision, kinds.ShowDescription) {
		v.Description = ""
	}
	if !s.shows(kind, v.Decision, kinds.ShowAttributes) {
		v.Attributes = map[string]any{}
	}
	if !s.shows(kind, v.Decision, kinds.ShowLength) {
		v.Pages, v.DurationMS, v.Segments = nil, nil, []Segment{}
	}
}

// fillCovers resolves every version's cover in one call; a content's is its
// default edition's in language.
func (s *Store) fillCovers(ctx context.Context, built map[string]*Content, language string) error {
	var asked []versions.CoverOf
	var at []*Version
	for id, c := range built {
		for i := range c.Versions {
			if v := &c.Versions[i]; v.Cover != nil {
				asked, at = append(asked, versions.CoverOf{Kind: c.Kind, ID: id, Version: v.ID}), append(at, v)
			}
		}
	}
	if len(asked) > 0 && s.media != nil {
		got, err := s.media.Versions.Covers(ctx, asked)
		if err != nil {
			return err
		}
		for i, v := range at {
			v.Cover.Renditions, v.Cover.URL = got[i], coverURL(got[i])
			if len(got[i]) == 0 && v.Cover.Image == "" {
				v.Cover = nil
			}
		}
	}
	for _, c := range built {
		if d := DefaultVersion(c.Versions, language); d != nil && d.Cover != nil {
			cover := *d.Cover
			c.Cover = &cover
		}
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

// released projects durable completion. Chapters release independently; the
// clock never grants access while their storage publication is unfinished.
func released(ss []Segment) []Segment {
	out := make([]Segment, len(ss))
	for i, sg := range ss {
		sg.Released = sg.ReleasedAt != nil
		out[i] = sg
	}
	return out
}

type itemRow struct {
	kind, state, ownerEntity          string
	comments                          string
	access                            access.Rule
	restrictions                      []string
	attributes                        map[string]any
	publishAt, publishedAt, deletedAt *time.Time
	createdAt, updatedAt              time.Time
	revision                          int64
}

func (s *Store) readItems(ctx context.Context, ids []string) (map[string]*itemRow, error) {
	rows, err := s.pool.Query(ctx, `SELECT c.id, c.kind, c.state, coalesce(c.owner_entity_id, ''),
 c.access, c.restrictions, c.attributes, c.publish_at, c.published_at, c.deleted_at, c.created_at, c.updated_at,
 c.revision, c.comments
 FROM `+s.t.items+` c WHERE c.tenant_id = $1 AND c.id = ANY($2)`, s.tenant, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]*itemRow{}
	for rows.Next() {
		var id string
		var r itemRow
		var access, attrs []byte
		if err := rows.Scan(&id, &r.kind, &r.state, &r.ownerEntity, &access, &r.restrictions,
			&attrs, &r.publishAt, &r.publishedAt, &r.deletedAt, &r.createdAt, &r.updatedAt, &r.revision, &r.comments); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(access, &r.access); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(attrs, &r.attributes); err != nil {
			return nil, err
		}
		out[id] = &r
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

// readVersions reads every version of the items with its text and segments,
// or with a non-empty vid only those with that ID or version code.
func (s *Store) readVersions(ctx context.Context, ids []string, vid string) (map[string]*Version, error) {
	q := `SELECT id, code, position, languages, traits, label, state, publish_at, published_at, access, attributes,
 pages, duration_ms, width, height, has_audio, ordinal, coalesce(cover->>'image', ''),coalesce(original_title,''),coalesce(original_language,''),
 (SELECT jsonb_build_object('id',r.id,'state',CASE WHEN r.state='retiring' THEN 'running'
          WHEN r.state='completed' AND coalesce((r.result->>'waiting')::boolean,false) THEN 'waiting' ELSE r.state END,
          'scheduled_at',r.scheduled_at,'completed_at',r.completed_at,'error',r.error)
  FROM ` + s.qs + `.content_publication_requests r WHERE r.tenant_id=v.tenant_id AND r.version_id=v.id AND r.current),
 (SELECT r.result#>'{pages,segments}' FROM ` + s.qs + `.content_publication_requests r
  WHERE r.tenant_id=v.tenant_id AND r.id=v.release_id AND r.version_id=v.id AND r.state IN ('retiring','completed'))
 FROM ` + s.t.versions + ` v WHERE tenant_id = $1 AND content_id = ANY($2)`
	args := []any{s.tenant, ids}
	if vid != "" {
		code, _ := contenturl.ParseVersionCode(vid)
		q += ` AND (id = $3 OR code = $4)`
		args = append(args, vid, string(code))
	}
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	out := map[string]*Version{}
	var vids []string
	for rows.Next() {
		v := &Version{Segments: []Segment{}, Cover: &Cover{}}
		var access, attrs, publication, readerSegments []byte
		if err := rows.Scan(&v.ID, &v.Code, &v.Position, &v.Languages, &v.Traits, &v.Label, &v.State, &v.PublishAt, &v.PublishedAt, &access, &attrs,
			&v.Pages, &v.DurationMS, &v.Width, &v.Height, &v.HasAudio, &v.Ordinal, &v.Cover.Image, &v.OriginalTitle, &v.OriginalLanguage, &publication, &readerSegments); err != nil {
			rows.Close()
			return nil, err
		}
		if access != nil {
			if err := json.Unmarshal(access, &v.Access); err != nil {
				rows.Close()
				return nil, err
			}
		}
		if publication != nil {
			if err := json.Unmarshal(publication, &v.Publication); err != nil {
				rows.Close()
				return nil, err
			}
		}
		if readerSegments != nil {
			if err := json.Unmarshal(readerSegments, &v.readerSegments); err != nil {
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
	rows, err = s.pool.Query(ctx, `SELECT version_id, language, title, description,slug,body,sort_key,aliases FROM `+s.t.versionText+` WHERE tenant_id = $1 AND version_id = ANY($2)`, s.tenant, vids)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var vid, l string
		var t VersionText
		if err := rows.Scan(&vid, &l, &t.Title, &t.Description, &t.Slug, &t.Body, &t.SortKey, &t.Aliases); err != nil {
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
	rows, err = s.pool.Query(ctx, `SELECT version_id, id, position, title, coalesce(start_index, 0), coalesce(end_index, 0), release_at, coalesce(start_page, ''), released_at FROM `+s.t.segments+`
 WHERE tenant_id = $1 AND version_id = ANY($2) ORDER BY version_id, position`, s.tenant, vids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var vid string
		var sg Segment
		if err := rows.Scan(&vid, &sg.ID, &sg.Position, &sg.Title, &sg.Start, &sg.End, &sg.ReleaseAt, &sg.StartPage, &sg.ReleasedAt); err != nil {
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

// groupSummaries names the active entities among ids in language, with their
// pages' paths, as entity cards do.
func (s *Store) entitySummaries(ctx context.Context, ids []string, language string, management bool) (map[string]EntitySummary, error) {
	out := map[string]EntitySummary{}
	if len(ids) == 0 || s.entities == nil {
		return out, nil
	}
	gids := make([]entities.EntityID, len(ids))
	for i, id := range ids {
		gids[i] = entities.EntityID(id)
	}
	cards, err := s.entities.Cards(ctx, gids, entities.CardOptions{Language: language, IncludeInternal: management})
	if err != nil {
		return nil, err
	}
	for _, g := range cards {
		out[string(g.ID)] = EntitySummary{ID: string(g.ID), Kind: g.Kind, Slug: g.Slug, Name: g.Name, Code: g.Code, Path: g.Path}
	}
	return out, nil
}
