package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/codes"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/versions"
	"github.com/open-rails/contentkit/offers"
)

// ImportContent is a content item as the bulk import writes it: its id,
// state and times kept, trusted (not screened). Nil Creators, Text and a
// version's Text and Segments keep what is stored; given ones replace it.
type ImportContent struct {
	ID          string
	Kind        string
	State       string
	PublishAt   *time.Time
	PublishedAt *time.Time
	DeletedAt   *time.Time
	Listing     string
	// Comments is open (when empty), closed or hidden.
	Comments string
	// ViewBaseline is the legacy all-time view count, added to the viewers
	// the signal plane counts.
	ViewBaseline int64
	Access       *access.Rule
	Restrictions []string
	Attributes   map[string]any
	OwnerGroupID string
	Cover        string
	Creators     []Creator
	Text         map[string]Text
	Versions     []ImportVersion
	Code         string
	CreatedAt    *time.Time
	UpdatedAt    *time.Time
}

// ImportVersion is one version of an ImportContent, positioned as listed.
type ImportVersion struct {
	ID          string
	Languages   []string
	Traits      []string
	Label       string
	State       string
	PublishAt   *time.Time
	PublishedAt *time.Time
	DeletedAt   *time.Time
	Access      *access.Rule
	Attributes  map[string]any
	Ordinal     *int
	Text        map[string]VersionText
	Segments    []Segment
	CreatedAt   *time.Time
	UpdatedAt   *time.Time
}

// stamped is a stored item's or version's state and times.
type stamped struct {
	kind, contentID, state string
	publishAt, publishedAt *time.Time
	deletedAt              *time.Time
	createdAt              time.Time
}

type importItemRow struct {
	ID           string          `json:"id"`
	Kind         string          `json:"kind"`
	State        string          `json:"state"`
	PublishAt    *time.Time      `json:"publish_at"`
	PublishedAt  *time.Time      `json:"published_at"`
	DeletedAt    *time.Time      `json:"deleted_at"`
	Listing      string          `json:"listing"`
	Comments     string          `json:"comments"`
	ViewBaseline int64           `json:"view_baseline"`
	Access       json.RawMessage `json:"access"`
	Restrictions []string        `json:"restrictions"`
	Attributes   map[string]any  `json:"attributes"`
	OwnerGroupID *string         `json:"owner_group_id"`
	Cover        *Cover          `json:"cover"`
	CreatedAt    time.Time       `json:"created_at"`
	UpdatedAt    time.Time       `json:"updated_at"`
}

type importVersionRow struct {
	ID          string          `json:"id"`
	ContentID   string          `json:"content_id"`
	Position    int             `json:"position"`
	Languages   []string        `json:"languages"`
	Traits      []string        `json:"traits"`
	Label       string          `json:"label"`
	State       string          `json:"state"`
	PublishAt   *time.Time      `json:"publish_at"`
	PublishedAt *time.Time      `json:"published_at"`
	DeletedAt   *time.Time      `json:"deleted_at"`
	Access      json.RawMessage `json:"access"`
	Attributes  map[string]any  `json:"attributes"`
	Ordinal     *int            `json:"ordinal"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
}

type importTextRow struct {
	ContentID string `json:"content_id"`
	Language  string `json:"language"`
	Text
}

type importCreatorRow struct {
	ContentID string `json:"content_id"`
	Creator
}

type importVersionTextRow struct {
	VersionID string `json:"version_id"`
	Language  string `json:"language"`
	VersionText
}

type importSegmentRow struct {
	VersionID string     `json:"version_id"`
	Position  int        `json:"position"`
	Title     string     `json:"title"`
	Start     int        `json:"start_index"`
	End       int        `json:"end_index"`
	ReleaseAt *time.Time `json:"release_at"`
}

// importBatch is one Import's rows.
type importBatch struct {
	items     []importItemRow
	versions  []importVersionRow
	texts     []importTextRow
	creators  []importCreatorRow
	vtexts    []importVersionTextRow
	segments  []importSegmentRow
	textIDs   []string // items whose text is replaced
	creatorID []string // items whose creators are replaced
	vtextIDs  []string // versions whose text is replaced
	segIDs    []string // versions whose segments are replaced
	fresh     map[string]bool
	keep      map[codes.Key]string
}

// Import writes content items in tx: new ones are inserted, stored ones
// updated where the import changes them. Then what depends on them follows,
// as after every write: codes, facets, search documents, events, media
// exposure and publications, and offers' product names. A version of another
// content item or a kind change is ErrInvalid.
func (s *Store) Import(ctx context.Context, tx pgx.Tx, in []ImportContent) error {
	if len(in) == 0 {
		return nil
	}
	now := s.now()
	var ids, vids []string
	for i := range in {
		if err := s.checkImport(ctx, &in[i]); err != nil {
			return err
		}
		if slices.Contains(ids, in[i].ID) {
			return invalid("content %s is imported twice", in[i].ID)
		}
		ids = append(ids, in[i].ID)
		for _, v := range in[i].Versions {
			if slices.Contains(vids, v.ID) {
				return invalid("version %s is imported twice", v.ID)
			}
			vids = append(vids, v.ID)
		}
	}
	items, err := s.lockStamped(ctx, tx, `SELECT id, kind, '', state, publish_at, published_at, deleted_at, created_at FROM `+s.t.items+`
 WHERE tenant_id = $1 AND id = ANY($2) ORDER BY id FOR UPDATE`, ids)
	if err != nil {
		return err
	}
	hidden, err := s.hiddenVersions(ctx, tx, in)
	if err != nil {
		return err
	}
	for _, v := range hidden {
		vids = append(vids, v)
	}
	stored, err := s.lockStamped(ctx, tx, `SELECT v.id, c.kind, v.content_id, v.state, v.publish_at, v.published_at, v.deleted_at, v.created_at
 FROM `+s.t.versions+` v JOIN `+s.t.items+` c ON c.tenant_id = v.tenant_id AND c.id = v.content_id
 WHERE v.tenant_id = $1 AND v.id = ANY($2) ORDER BY v.id FOR UPDATE OF v`, vids)
	if err != nil {
		return err
	}
	b, rules, owners, err := s.importRows(in, items, stored, hidden, now)
	if err != nil {
		return err
	}
	if err := s.checkRuleGroups(ctx, tx, rules...); err != nil {
		return err
	}
	if err := s.checkOwners(ctx, tx, owners); err != nil {
		return err
	}
	var republish []versions.Target
	if s.media != nil {
		if republish, err = s.republishImported(ctx, tx, stored, b); err != nil {
			return err
		}
	}
	changed, retitled, err := s.writeImport(ctx, tx, b)
	if err != nil {
		return err
	}
	if err := codes.Reserve(ctx, codes.Pgx(tx), s.qs, s.tenant, b.keep); err != nil {
		return codeError(err)
	}
	var events []Event
	var written, updated []string
	for _, c := range in {
		switch {
		case b.fresh[c.ID]:
			events = append(events, Event{Type: EventContentCreated, Kind: c.Kind, ContentID: c.ID})
		case changed[c.ID]:
			updated = append(updated, c.ID)
			events = append(events, Event{Type: EventContentUpdated, Kind: c.Kind, ContentID: c.ID})
		default:
			continue
		}
		written = append(written, c.ID)
	}
	if len(written) == 0 {
		return nil
	}
	if len(updated) > 0 {
		data, _ := json.Marshal(b.items)
		if _, err := tx.Exec(ctx, `UPDATE `+s.t.items+` c SET revision = c.revision + 1, updated_at = r.updated_at
 FROM jsonb_to_recordset($3::jsonb) AS r(id text, updated_at timestamptz) WHERE c.tenant_id = $1 AND c.id = ANY($2) AND c.id = r.id`,
			s.tenant, updated, string(data)); err != nil {
			return err
		}
	}
	if err := s.putCodes(ctx, tx, written); err != nil {
		return codeError(err)
	}
	if err := s.RefreshFacets(ctx, tx, written); err != nil {
		return err
	}
	if err := s.emit(ctx, tx, events...); err != nil {
		return err
	}
	if s.media != nil {
		if err := s.exposeTx(ctx, tx, updated); err != nil {
			return err
		}
		if err := s.media.Versions.RepublishTx(ctx, tx, republish); err != nil {
			return err
		}
	}
	return s.resyncSold(ctx, tx, retitled)
}

// importRows turns the checked items into rows, with the rules and owner
// groups to check.
func (s *Store) importRows(in []ImportContent, items, stored map[string]stamped, hidden map[string]string, now time.Time) (importBatch, []*access.Rule, []string, error) {
	b := importBatch{fresh: map[string]bool{}, keep: map[codes.Key]string{}}
	var rules []*access.Rule
	var owners []string
	for _, c := range in {
		old, existed := items[c.ID]
		if existed && old.kind != c.Kind {
			return b, nil, nil, invalid("content %s is a %s, not a %s", c.ID, old.kind, c.Kind)
		}
		var oldp *stamped
		if existed {
			oldp = &old
		} else {
			b.fresh[c.ID] = true
		}
		row := importItemRow{ID: c.ID, Kind: c.Kind, State: c.State, Listing: c.Listing, Comments: c.Comments, ViewBaseline: c.ViewBaseline,
			Restrictions: c.Restrictions, Attributes: c.Attributes}
		row.PublishAt, row.PublishedAt, row.DeletedAt = importTimes(c.State, c.PublishAt, c.PublishedAt, c.DeletedAt, oldp, now)
		row.Access, _ = json.Marshal(c.Access)
		rules = append(rules, c.Access)
		if c.OwnerGroupID != "" {
			row.OwnerGroupID = &c.OwnerGroupID
			owners = append(owners, c.OwnerGroupID)
		}
		if c.Cover != "" {
			row.Cover = &Cover{Image: c.Cover}
		}
		row.CreatedAt, row.UpdatedAt = importStamps(c.CreatedAt, c.UpdatedAt, oldp, now)
		b.items = append(b.items, row)
		if c.Code != "" {
			b.keep[codes.Key{Kind: c.Kind, ID: c.ID}] = c.Code
		}
		if c.Text != nil {
			b.textIDs = append(b.textIDs, c.ID)
			for l, t := range c.Text {
				b.texts = append(b.texts, importTextRow{ContentID: c.ID, Language: l, Text: t})
			}
		}
		if c.Creators != nil {
			b.creatorID = append(b.creatorID, c.ID)
			for _, cr := range c.Creators {
				b.creators = append(b.creators, importCreatorRow{ContentID: c.ID, Creator: cr})
			}
		}
		k := s.cfg.Kinds[c.Kind]
		vs := c.Versions
		if k.Versions.Hidden && len(vs) == 0 {
			id, ok := hidden[c.ID]
			if !ok {
				id = contentref.IDAt(row.CreatedAt)
			}
			vs = []ImportVersion{{ID: id, State: StatePublished, Traits: []string{}, Attributes: map[string]any{}}}
		}
		for pos, v := range vs {
			o, had := stored[v.ID]
			if had && o.contentID != c.ID {
				return b, nil, nil, invalid("version %s belongs to content %s", v.ID, o.contentID)
			}
			var op *stamped
			if had {
				op = &o
			}
			vr := importVersionRow{ID: v.ID, ContentID: c.ID, Position: pos, Languages: v.Languages, Traits: v.Traits, Label: v.Label,
				State: v.State, Attributes: v.Attributes, Ordinal: v.Ordinal}
			if k.Versions.Hidden {
				switch {
				case c.Text != nil:
					vr.Languages = s.hiddenLanguages(k, c.Text)
				case !had:
					vr.Languages = []string{s.defaultLanguage()}
				default:
					vr.Languages = nil // the stored ones stay
				}
			}
			vr.PublishAt, vr.PublishedAt, vr.DeletedAt = importTimes(v.State, v.PublishAt, v.PublishedAt, v.DeletedAt, op, now)
			if v.Access != nil {
				vr.Access, _ = json.Marshal(v.Access)
				rules = append(rules, v.Access)
			}
			vr.CreatedAt, vr.UpdatedAt = importStamps(v.CreatedAt, v.UpdatedAt, op, now)
			b.versions = append(b.versions, vr)
			if v.Text != nil {
				b.vtextIDs = append(b.vtextIDs, v.ID)
				for l, t := range v.Text {
					b.vtexts = append(b.vtexts, importVersionTextRow{VersionID: v.ID, Language: l, VersionText: t})
				}
			}
			if v.Segments != nil {
				b.segIDs = append(b.segIDs, v.ID)
				for _, sg := range v.Segments {
					b.segments = append(b.segments, importSegmentRow{VersionID: v.ID, Position: sg.Position, Title: sg.Title, Start: sg.Start, End: sg.End,
						ReleaseAt: sg.ReleaseAt})
				}
			}
		}
	}
	return b, rules, owners, nil
}

// writeImport upserts the rows, changing only what differs, and answers the
// content items it changed and those whose text changed.
func (s *Store) writeImport(ctx context.Context, tx pgx.Tx, b importBatch) (changed, retitled map[string]bool, err error) {
	changed, retitled = map[string]bool{}, map[string]bool{}
	run := func(into map[string]bool, sql string, args ...any) error {
		rows, err := tx.Query(ctx, sql, args...)
		if err != nil {
			return err
		}
		got, err := pgx.CollectRows(rows, pgx.RowTo[string])
		for _, id := range got {
			into[id], changed[id] = true, true
		}
		return err
	}
	data := func(v any) string { b, _ := json.Marshal(v); return string(b) }
	if err := run(changed, `INSERT INTO `+s.t.items+` AS c (tenant_id, id, kind, state, publish_at, published_at, deleted_at, listing, comments,
 view_baseline, access, restrictions, attributes, owner_group_id, cover, created_at, updated_at)
 SELECT $1, r.id, r.kind, r.state, r.publish_at, r.published_at, r.deleted_at, r.listing, r.comments, r.view_baseline, r.access, r.restrictions,
  r.attributes, r.owner_group_id, r.cover, r.created_at, r.updated_at
 FROM jsonb_to_recordset($2::jsonb) AS r(id text, kind text, state text, publish_at timestamptz, published_at timestamptz, deleted_at timestamptz,
  listing text, comments text, view_baseline bigint, access jsonb, restrictions text[], attributes jsonb, owner_group_id text, cover jsonb,
  created_at timestamptz, updated_at timestamptz)
 ON CONFLICT (tenant_id, id) DO UPDATE SET state = EXCLUDED.state, publish_at = EXCLUDED.publish_at, published_at = EXCLUDED.published_at,
  deleted_at = EXCLUDED.deleted_at, listing = EXCLUDED.listing, comments = EXCLUDED.comments, view_baseline = EXCLUDED.view_baseline,
  access = EXCLUDED.access, restrictions = EXCLUDED.restrictions, attributes = EXCLUDED.attributes, owner_group_id = EXCLUDED.owner_group_id,
  cover = EXCLUDED.cover, created_at = EXCLUDED.created_at
 WHERE (c.state, c.publish_at, c.published_at, c.deleted_at, c.listing, c.comments, c.view_baseline, c.access, c.restrictions, c.attributes,
  c.owner_group_id, c.cover, c.created_at)
  IS DISTINCT FROM (EXCLUDED.state, EXCLUDED.publish_at, EXCLUDED.published_at, EXCLUDED.deleted_at, EXCLUDED.listing, EXCLUDED.comments,
  EXCLUDED.view_baseline, EXCLUDED.access, EXCLUDED.restrictions, EXCLUDED.attributes, EXCLUDED.owner_group_id, EXCLUDED.cover, EXCLUDED.created_at)
 RETURNING c.id`, s.tenant, data(b.items)); err != nil {
		return nil, nil, err
	}
	if len(b.textIDs) > 0 {
		if err := run(retitled, `DELETE FROM `+s.t.text+` t WHERE t.tenant_id = $1 AND t.content_id = ANY($2) AND NOT EXISTS (
 SELECT 1 FROM jsonb_to_recordset($3::jsonb) AS r(content_id text, language text) WHERE r.content_id = t.content_id AND r.language = t.language)
 RETURNING t.content_id`, s.tenant, b.textIDs, data(b.texts)); err != nil {
			return nil, nil, err
		}
		if err := run(retitled, `INSERT INTO `+s.t.text+` AS t (tenant_id, content_id, language, title, slug, description, body, sort_key, aliases)
 SELECT $1, r.content_id, r.language, r.title, coalesce(r.slug, ''), coalesce(r.description, ''), coalesce(r.body, ''), coalesce(r.sort_key, ''),
  coalesce(r.aliases, '{}')
 FROM jsonb_to_recordset($2::jsonb) AS r(content_id text, language text, title text, slug text, description text, body text, sort_key text, aliases text[])
 ON CONFLICT (tenant_id, content_id, language) DO UPDATE SET title = EXCLUDED.title, slug = EXCLUDED.slug, description = EXCLUDED.description,
  body = EXCLUDED.body, sort_key = EXCLUDED.sort_key, aliases = EXCLUDED.aliases, updated_at = clock_timestamp()
 WHERE (t.title, t.slug, t.description, t.body, t.sort_key, t.aliases) IS DISTINCT FROM
  (EXCLUDED.title, EXCLUDED.slug, EXCLUDED.description, EXCLUDED.body, EXCLUDED.sort_key, EXCLUDED.aliases)
 RETURNING t.content_id`, s.tenant, data(b.texts)); err != nil {
			return nil, nil, err
		}
	}
	if len(b.creatorID) > 0 {
		if err := run(changed, `DELETE FROM `+s.t.creators+` c WHERE c.tenant_id = $1 AND c.content_id = ANY($2) AND NOT EXISTS (
 SELECT 1 FROM jsonb_to_recordset($3::jsonb) AS r(content_id text, user_id text, role text)
 WHERE r.content_id = c.content_id AND r.user_id = c.user_id AND r.role = c.role) RETURNING c.content_id`, s.tenant, b.creatorID, data(b.creators)); err != nil {
			return nil, nil, err
		}
		if err := run(changed, `INSERT INTO `+s.t.creators+` AS c (tenant_id, content_id, user_id, role, position)
 SELECT $1, r.content_id, r.user_id, r.role, r.position FROM jsonb_to_recordset($2::jsonb) AS r(content_id text, user_id text, role text, position int)
 ON CONFLICT (tenant_id, content_id, user_id, role) DO UPDATE SET position = EXCLUDED.position WHERE c.position <> EXCLUDED.position
 RETURNING c.content_id`, s.tenant, data(b.creators)); err != nil {
			return nil, nil, err
		}
	}
	if err := run(changed, `INSERT INTO `+s.t.versions+` AS v (tenant_id, id, content_id, position, languages, traits, label, state, publish_at,
 published_at, deleted_at, access, attributes, ordinal, created_at, updated_at)
 SELECT $1, r.id, r.content_id, r.position, r.languages, r.traits, r.label, r.state, r.publish_at, r.published_at, r.deleted_at,
  r.access, r.attributes, r.ordinal, r.created_at, r.updated_at
 FROM jsonb_to_recordset($2::jsonb) AS r(id text, content_id text, position int, languages text[], traits text[], label text, state text,
  publish_at timestamptz, published_at timestamptz, deleted_at timestamptz, access jsonb, attributes jsonb, ordinal int, created_at timestamptz, updated_at timestamptz)
 ON CONFLICT (tenant_id, id) DO UPDATE SET position = EXCLUDED.position, languages = coalesce(EXCLUDED.languages, v.languages), traits = EXCLUDED.traits,
  label = EXCLUDED.label, state = EXCLUDED.state, publish_at = EXCLUDED.publish_at, published_at = EXCLUDED.published_at, deleted_at = EXCLUDED.deleted_at,
  access = EXCLUDED.access, attributes = EXCLUDED.attributes, ordinal = EXCLUDED.ordinal, created_at = EXCLUDED.created_at,
  updated_at = EXCLUDED.updated_at, revision = v.revision + 1
 WHERE (v.position, v.languages, v.traits, v.label, v.state, v.publish_at, v.published_at, v.deleted_at, v.access, v.attributes, v.ordinal, v.created_at)
  IS DISTINCT FROM (EXCLUDED.position, coalesce(EXCLUDED.languages, v.languages), EXCLUDED.traits, EXCLUDED.label, EXCLUDED.state, EXCLUDED.publish_at,
  EXCLUDED.published_at, EXCLUDED.deleted_at, EXCLUDED.access, EXCLUDED.attributes, EXCLUDED.ordinal, EXCLUDED.created_at)
 RETURNING v.content_id`, s.tenant, data(b.versions)); err != nil {
		return nil, nil, err
	}
	// Version rows answer their content item.
	content := func(sql string) string {
		return `WITH x AS (` + sql + ` RETURNING version_id) SELECT v.content_id FROM x JOIN ` + s.t.versions + ` v ON v.tenant_id = $1 AND v.id = x.version_id`
	}
	if len(b.vtextIDs) > 0 {
		if err := run(changed, content(`DELETE FROM `+s.t.versionText+` t WHERE t.tenant_id = $1 AND t.version_id = ANY($2) AND NOT EXISTS (
 SELECT 1 FROM jsonb_to_recordset($3::jsonb) AS r(version_id text, language text) WHERE r.version_id = t.version_id AND r.language = t.language)`),
			s.tenant, b.vtextIDs, data(b.vtexts)); err != nil {
			return nil, nil, err
		}
		if err := run(changed, content(`INSERT INTO `+s.t.versionText+` AS t (tenant_id, version_id, language, title, description)
 SELECT $1, r.version_id, r.language, coalesce(r.title, ''), coalesce(r.description, '') FROM jsonb_to_recordset($2::jsonb) AS r(version_id text, language text, title text, description text)
 ON CONFLICT (tenant_id, version_id, language) DO UPDATE SET title = EXCLUDED.title, description = EXCLUDED.description
 WHERE (t.title, t.description) IS DISTINCT FROM (EXCLUDED.title, EXCLUDED.description)`), s.tenant, data(b.vtexts)); err != nil {
			return nil, nil, err
		}
	}
	if len(b.segIDs) > 0 {
		if err := run(changed, content(`DELETE FROM `+s.t.segments+` g WHERE g.tenant_id = $1 AND g.version_id = ANY($2) AND NOT EXISTS (
 SELECT 1 FROM jsonb_to_recordset($3::jsonb) AS r(version_id text, position int) WHERE r.version_id = g.version_id AND r.position = g.position)`),
			s.tenant, b.segIDs, data(b.segments)); err != nil {
			return nil, nil, err
		}
		if err := run(changed, content(`INSERT INTO `+s.t.segments+` AS g (tenant_id, version_id, position, title, start_index, end_index, release_at)
 SELECT $1, r.version_id, r.position, coalesce(r.title, ''), r.start_index, r.end_index, r.release_at
 FROM jsonb_to_recordset($2::jsonb) AS r(version_id text, position int, title text, start_index int, end_index int, release_at timestamptz)
 ON CONFLICT (tenant_id, version_id, position) DO UPDATE SET title = EXCLUDED.title, start_index = EXCLUDED.start_index, end_index = EXCLUDED.end_index,
  release_at = EXCLUDED.release_at
 WHERE (g.title, g.start_index, g.end_index, g.release_at) IS DISTINCT FROM (EXCLUDED.title, EXCLUDED.start_index, EXCLUDED.end_index, EXCLUDED.release_at)`),
			s.tenant, data(b.segments)); err != nil {
			return nil, nil, err
		}
	}
	return changed, retitled, nil
}

// RefreshCountsJob is RefreshCounts' job, for Queue: an import queues it.
func RefreshCountsJob() river.JobArgs { return refreshCountsArgs{} }

// importTimes are a record's publication times. A published one is live from
// publish_at (its stored time, else now, when omitted) and went live at
// published_at; a deleted one keeps the times it gives and its deletion's.
func importTimes(state string, publishAt, publishedAt, deletedAt *time.Time, old *stamped, now time.Time) (pa, pd, da *time.Time) {
	switch state {
	case StatePublished:
		pa = firstTime(publishAt, publishedAt)
		if pa == nil && old != nil && old.state == StatePublished {
			pa, pd = old.publishAt, old.publishedAt
		}
		if pa == nil {
			pa = &now
		}
	case StateDeleted:
		pa = firstTime(publishAt, publishedAt)
		if da = deletedAt; da == nil && old != nil {
			da = old.deletedAt
		}
		if da == nil {
			da = &now
		}
	default:
		return nil, nil, nil
	}
	switch {
	case publishedAt != nil:
		pd = publishedAt
	case pd == nil && pa != nil && !pa.After(now):
		pd = pa
	}
	return pa, pd, da
}

func firstTime(ts ...*time.Time) *time.Time {
	for _, t := range ts {
		if t != nil {
			return t
		}
	}
	return nil
}

// importStamps are a record's created_at (the stored one when omitted) and
// the updated_at a change writes.
func importStamps(created, updated *time.Time, old *stamped, now time.Time) (time.Time, time.Time) {
	c := now
	switch {
	case created != nil:
		c = *created
	case old != nil:
		c = old.createdAt
	}
	switch {
	case updated != nil:
		return c, *updated
	case old == nil:
		return c, c
	}
	return c, now
}

func codeError(err error) error {
	switch {
	case errors.Is(err, codes.ErrConflict):
		return errors.Join(ErrConflict, err)
	case errors.Is(err, codes.ErrInvalid):
		return errors.Join(ErrInvalid, err)
	}
	return err
}

// checkImport validates and normalizes one item and its versions.
func (s *Store) checkImport(ctx context.Context, c *ImportContent) error {
	if err := contentref.ValidateID(c.ID); err != nil {
		return invalid("content id: %v", err)
	}
	k, err := s.kind(c.Kind)
	if err != nil {
		return err
	}
	switch c.State {
	case StateDraft, StatePublished, StateDeleted:
	default:
		return invalid("state %q must be draft, published or deleted", c.State)
	}
	if c.Listing == "" {
		c.Listing = ListingListed
	}
	if err := s.checkListing(c.Listing); err != nil {
		return err
	}
	if c.Comments == "" {
		c.Comments = content.CommentsOpen
	}
	if err := checkComments(c.Comments); err != nil {
		return err
	}
	if c.ViewBaseline < 0 {
		return invalid("view_baseline %d is negative", c.ViewBaseline)
	}
	rule := access.Rule{Level: access.Public}
	if c.Access != nil {
		rule = *c.Access
	}
	if rule, err = s.checkRule(rule); err != nil {
		return err
	}
	c.Access = &rule
	if c.Restrictions, err = s.checkRestrictions(c.Restrictions); err != nil {
		return err
	}
	if c.Attributes, err = checkAttributes(k.Attributes, c.Attributes); err != nil {
		return err
	}
	if c.OwnerGroupID != "" {
		if err := contentref.ValidateID(c.OwnerGroupID); err != nil {
			return invalid("owner group: %v", err)
		}
	}
	if c.Cover != "" && !media.ValidNamed(c.Cover) {
		return invalid("cover must be an inline image name (i-{uuid})")
	}
	if c.Code != "" && !codes.ValidCode(c.Code) {
		return invalid("code %q is not a content code", c.Code)
	}
	if c.Text != nil {
		texts := make(map[string]Text, len(c.Text))
		for l, t := range c.Text {
			if texts[l], err = s.cleanText(ctx, l, t); err != nil {
				return err
			}
		}
		c.Text = texts
	}
	if c.Creators != nil {
		if c.Creators, err = s.checkCreators(c.Creators); err != nil {
			return err
		}
	}
	switch {
	case k.Versions.Hidden && len(c.Versions) > 1:
		return invalid("kind %s has one hidden version", c.Kind)
	case !k.Versions.Hidden && len(c.Versions) == 0:
		return invalid("a content item needs at least one version")
	}
	for i := range c.Versions {
		if err := s.checkImportVersion(c.Kind, &c.Versions[i]); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) checkImportVersion(kind string, v *ImportVersion) error {
	k := s.cfg.Kinds[kind]
	if err := contentref.ValidateID(v.ID); err != nil {
		return invalid("version id: %v", err)
	}
	switch v.State {
	case "":
		v.State = StatePublished
	case StateDraft, StatePublished, StateDeleted:
	default:
		return invalid("version state %q must be draft, published or deleted", v.State)
	}
	var err error
	if !k.Versions.Hidden {
		if v.Languages, err = s.checkVersionLanguages(k, v.Languages); err != nil {
			return err
		}
	}
	if v.Traits, err = s.checkTraits(k, kind, v.Traits); err != nil {
		return err
	}
	if v.Label = strings.TrimSpace(v.Label); utf8.RuneCountInString(v.Label) > maxLabel {
		return invalid("label too long")
	}
	if v.Access != nil {
		r, err := s.checkRule(*v.Access)
		if err != nil {
			return err
		}
		v.Access = &r
	}
	if v.Attributes, err = checkAttributes(k.Attributes, v.Attributes); err != nil {
		return err
	}
	for l, t := range v.Text {
		if err := s.checkLanguage(l); err != nil {
			return err
		}
		t.Title, t.Description = strings.TrimSpace(t.Title), strings.TrimSpace(t.Description)
		if utf8.RuneCountInString(t.Title) > maxTitle || utf8.RuneCountInString(t.Description) > maxDescription {
			return invalid("version text %s exceeds its limits", l)
		}
		v.Text[l] = t
	}
	if v.Segments != nil {
		if !k.Segments {
			return invalid("kind %s has no segments", kind)
		}
		segs, err := checkSegments(v.Segments)
		if err != nil {
			return err
		}
		for i := range segs {
			segs[i].Title = strings.TrimSpace(segs[i].Title)
		}
		v.Segments = segs
	}
	return nil
}

// lockStamped locks the rows sql reads (id, kind, content_id, state,
// publish_at, published_at, deleted_at, created_at for $2's ids), by id.
func (s *Store) lockStamped(ctx context.Context, tx pgx.Tx, sql string, ids []string) (map[string]stamped, error) {
	out := map[string]stamped{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := tx.Query(ctx, sql, s.tenant, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var r stamped
		if err := rows.Scan(&id, &r.kind, &r.contentID, &r.state, &r.publishAt, &r.publishedAt, &r.deletedAt, &r.createdAt); err != nil {
			return nil, err
		}
		out[id] = r
	}
	return out, rows.Err()
}

// hiddenVersions are the stored versions of the hidden-kind items an import
// gives no version.
func (s *Store) hiddenVersions(ctx context.Context, tx pgx.Tx, in []ImportContent) (map[string]string, error) {
	var ids []string
	for _, c := range in {
		if s.cfg.Kinds[c.Kind].Versions.Hidden && len(c.Versions) == 0 {
			ids = append(ids, c.ID)
		}
	}
	out := map[string]string{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := tx.Query(ctx, `SELECT DISTINCT ON (content_id) content_id, id FROM `+s.t.versions+` WHERE tenant_id = $1 AND content_id = ANY($2)
 ORDER BY content_id, position, id`, s.tenant, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var c, v string
		if err := rows.Scan(&c, &v); err != nil {
			return nil, err
		}
		out[c] = v
	}
	return out, rows.Err()
}

func (s *Store) checkOwners(ctx context.Context, q Querier, ids []string) error {
	ids = slices.Compact(slices.Sorted(slices.Values(ids)))
	if len(ids) == 0 {
		return nil
	}
	var missing string
	if err := q.QueryRow(ctx, `SELECT coalesce(min(u.id), '') FROM unnest($2::text[]) AS u(id)
 WHERE NOT EXISTS (SELECT 1 FROM `+s.t.groups+` g WHERE g.tenant_id = $1 AND g.id = u.id AND g.state = 'active')`, s.tenant, ids).Scan(&missing); err != nil {
		return err
	}
	if missing != "" {
		return fmt.Errorf("%w: owner group %s", ErrNotFound, missing)
	}
	return nil
}

// republishImported are the stored versions whose effective rule or segments
// the import changes, read before it writes: their publications follow.
func (s *Store) republishImported(ctx context.Context, tx pgx.Tx, stored map[string]stamped, b importBatch) ([]versions.Target, error) {
	var had []importVersionRow
	for _, v := range b.versions {
		if _, ok := stored[v.ID]; ok {
			had = append(had, v)
		}
	}
	if len(had) == 0 {
		return nil, nil
	}
	data := func(v any) string { b, _ := json.Marshal(v); return string(b) }
	rows, err := tx.Query(ctx, `SELECT r.content_id, r.id FROM jsonb_to_recordset($2::jsonb) AS r(id text, content_id text, access jsonb)
 JOIN `+s.t.versions+` v ON v.tenant_id = $1 AND v.id = r.id
 JOIN `+s.t.items+` c ON c.tenant_id = $1 AND c.id = r.content_id
 JOIN jsonb_to_recordset($3::jsonb) AS i(id text, access jsonb) ON i.id = r.content_id
 WHERE coalesce(v.access, c.access) IS DISTINCT FROM coalesce(r.access, i.access)
  OR (r.id = ANY($4) AND (SELECT coalesce(jsonb_agg(jsonb_build_array(g.position, g.start_index, g.end_index, g.release_at) ORDER BY g.position), '[]')
   FROM `+s.t.segments+` g WHERE g.tenant_id = $1 AND g.version_id = r.id) IS DISTINCT FROM
   (SELECT coalesce(jsonb_agg(jsonb_build_array(n.position, n.start_index, n.end_index, n.release_at) ORDER BY n.position), '[]')
   FROM jsonb_to_recordset($5::jsonb) AS n(version_id text, position int, start_index int, end_index int, release_at timestamptz) WHERE n.version_id = r.id))
 ORDER BY r.id`, s.tenant, data(had), data(b.items), b.segIDs, data(b.segments))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []versions.Target
	for rows.Next() {
		var t versions.Target
		if err := rows.Scan(&t.Content, &t.Version); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// resyncSold re-syncs the active offers of content items whose text changed
// and of their versions, whose products are named after their titles.
func (s *Store) resyncSold(ctx context.Context, tx pgx.Tx, retitled map[string]bool) error {
	if s.offers == nil || len(retitled) == 0 {
		return nil
	}
	ids := make([]string, 0, len(retitled))
	for id := range retitled {
		ids = append(ids, id)
	}
	rows, err := tx.Query(ctx, `SELECT o.target_kind, o.target_id, coalesce(v.content_id, '') FROM `+s.qs+`.content_offers o
 LEFT JOIN `+s.t.versions+` v ON o.target_kind = 'version' AND v.tenant_id = o.tenant_id AND v.id = o.target_id
 WHERE o.tenant_id = $1 AND o.state = 'active' AND ((o.target_kind = 'content' AND o.target_id = ANY($2)) OR v.content_id = ANY($2))
 ORDER BY o.target_kind, o.target_id`, s.tenant, ids)
	if err != nil {
		return err
	}
	var targets []offers.Target
	for rows.Next() {
		var t offers.Target
		if err := rows.Scan(&t.Kind, &t.ID, &t.Content); err != nil {
			rows.Close()
			return err
		}
		targets = append(targets, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(targets) == 0 {
		return err
	}
	return s.offers.Resync(ctx, tx, targets)
}
