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
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/contenturl"
	"github.com/open-rails/contentkit/internal/codes"
	"github.com/open-rails/contentkit/kinds"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/versions"
)

// ImportContent is a content item as the bulk import writes it: its id,
// state and times kept, trusted (not screened). Nil Creators, Text and a
// version's Text keep what is stored; given ones replace it. Chapters are
// authored through PutVersions after content and source media are imported.
type ImportContent struct {
	ID               string
	Kind             string
	State            string
	PublishAt        *time.Time
	PublishedAt      *time.Time
	DeletedAt        *time.Time
	OriginalTitle    *string
	OriginalLanguage *string
	// Comments is open (when empty), closed or hidden.
	Comments     string
	Access       *access.Rule
	Restrictions []string
	Attributes   map[string]any
	ChannelID    string
	Creators     []Creator
	Text         map[string]Text
	Versions     []ImportVersion
	Code         string
	CreatedAt    *time.Time
	UpdatedAt    *time.Time
}

// ImportVersion is one version of an ImportContent, positioned as listed.
type ImportVersion struct {
	ID string
	// Code is the version code to keep; empty keeps the stored one or
	// assigns one. A version never changes its code.
	Code        string
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
	// Cover selects one of the version's cover uploads (i-{uuid}).
	Cover string
	Text  map[string]VersionText
	// Segments must be nil. Use PutVersions after importing source media so
	// chapter changes hold the recovered manifest lock through commit.
	Segments  []Segment
	CreatedAt *time.Time
	UpdatedAt *time.Time
}

// stamped is a stored item's or version's state and times.
type stamped struct {
	kind, contentID, state string
	access                 json.RawMessage
	publishAt, publishedAt *time.Time
	deletedAt              *time.Time
	createdAt              time.Time
}

type importItemRow struct {
	ID               string          `json:"id"`
	Kind             string          `json:"kind"`
	State            string          `json:"state"`
	PublishAt        *time.Time      `json:"publish_at"`
	PublishedAt      *time.Time      `json:"published_at"`
	DeletedAt        *time.Time      `json:"deleted_at"`
	OriginalTitle    *string         `json:"original_title"`
	OriginalLanguage *string         `json:"original_language"`
	Comments         string          `json:"comments"`
	Access           json.RawMessage `json:"access"`
	Restrictions     []string        `json:"restrictions"`
	Attributes       map[string]any  `json:"attributes"`
	ChannelID        *string         `json:"owner_entity_id"`
	CreatedAt        time.Time       `json:"created_at"`
	UpdatedAt        time.Time       `json:"updated_at"`
}

type importVersionRow struct {
	ID          string          `json:"id"`
	ContentID   string          `json:"content_id"`
	Code        *string         `json:"code"`
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
	Cover       *Cover          `json:"cover"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
	// Playable is a new version's starting fact; an existing one keeps its own.
	Playable *bool `json:"playable"`
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
	ID        string     `json:"id"`
	VersionID string     `json:"version_id"`
	Position  int        `json:"position"`
	Title     string     `json:"title"`
	Start     *int       `json:"start_index"`
	End       *int       `json:"end_index"`
	StartPage *string    `json:"start_page"`
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
// exposure and publications. A version of another
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
		owner, err := s.fixedOwner(ctx, tx, in[i].Kind, in[i].ChannelID)
		if err != nil {
			return err
		}
		in[i].ChannelID = owner
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
	items, err := s.lockStamped(ctx, tx, `SELECT id, kind, '', state, publish_at, published_at, deleted_at, created_at, access FROM `+s.t.items+`
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
	stored, err := s.lockStamped(ctx, tx, `SELECT v.id, c.kind, v.content_id, v.state, v.publish_at, v.published_at, v.deleted_at, v.created_at, v.access
 FROM `+s.t.versions+` v JOIN `+s.t.items+` c ON c.tenant_id = v.tenant_id AND c.id = v.content_id
 WHERE v.tenant_id = $1 AND v.id = ANY($2) ORDER BY v.id FOR UPDATE OF v`, vids)
	if err != nil {
		return err
	}
	b, rules, owners, err := s.importRows(in, items, stored, hidden, now)
	if err != nil {
		return err
	}
	if err := s.checkRuleEntities(ctx, tx, rules...); err != nil {
		return err
	}
	if err := s.checkOwners(ctx, tx, owners); err != nil {
		return err
	}
	if err := s.checkVersionCodes(ctx, tx, b.versions); err != nil {
		return err
	}
	if _, err := s.recordReleases(ctx, tx, ids); err != nil {
		return err
	}
	changed, err := s.writeImport(ctx, tx, b)
	if err != nil {
		return err
	}
	var versionParents []string
	for _, version := range b.versions {
		if !slices.Contains(versionParents, version.ContentID) {
			versionParents = append(versionParents, version.ContentID)
		}
	}
	emptyEvents, err := s.deleteEmptyParents(ctx, tx, versionParents)
	if err != nil {
		return err
	}
	for _, event := range emptyEvents {
		changed[event.ContentID] = true
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
	events = append(events, emptyEvents...)
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
	if err := s.emit(ctx, tx, events...); err != nil {
		return err
	}
	if err := s.RefreshFacets(ctx, tx, written); err != nil {
		return err
	}
	if err := s.schedulePublicationTx(ctx, tx, contentTargets(written)); err != nil {
		return err
	}
	return nil
}

// importRows turns the checked items into rows, with the rules and owner
// entities to check.
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
		row := importItemRow{ID: c.ID, Kind: c.Kind, State: c.State, OriginalTitle: c.OriginalTitle, OriginalLanguage: c.OriginalLanguage, Comments: c.Comments,
			Restrictions: c.Restrictions, Attributes: c.Attributes}
		row.PublishAt, row.PublishedAt, row.DeletedAt = importTimes(c.State, c.PublishAt, c.PublishedAt, c.DeletedAt, oldp, now)
		row.Access, _ = json.Marshal(c.Access)
		rules = append(rules, c.Access)
		if c.ChannelID != "" {
			row.ChannelID = &c.ChannelID
			owners = append(owners, c.ChannelID)
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
			if v.Code != "" {
				vr.Code = &v.Code
			}
			if v.Cover != "" {
				vr.Cover = &Cover{Image: v.Cover}
			}
			if k.Versions.Hidden {
				switch {
				case c.Text != nil:
					vr.Languages = s.textLanguages(k, c.Text)
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
			} else if had && len(op.access) > 0 {
				vr.Access = op.access
			} else {
				vr.Access, _ = json.Marshal(c.Access)
			}
			vr.CreatedAt, vr.UpdatedAt = importStamps(v.CreatedAt, v.UpdatedAt, op, now)
			vr.Playable = s.startsUnplayable(c.Kind)
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
					row := importSegmentRow{ID: sg.ID, VersionID: v.ID, Position: sg.Position, Title: sg.Title, Start: &sg.Start, End: &sg.End, ReleaseAt: sg.ReleaseAt}
					if sg.StartPage != "" {
						row.Start, row.End, row.StartPage = nil, nil, &sg.StartPage
					}
					b.segments = append(b.segments, row)
				}
			}
		}
	}
	return b, rules, owners, nil
}

// writeImport upserts the rows, changing only what differs, and answers the
// content items it changed and those whose text changed.
func (s *Store) writeImport(ctx context.Context, tx pgx.Tx, b importBatch) (changed map[string]bool, err error) {
	changed = map[string]bool{}
	run := func(sql string, args ...any) error {
		rows, err := tx.Query(ctx, sql, args...)
		if err != nil {
			return err
		}
		got, err := pgx.CollectRows(rows, pgx.RowTo[string])
		for _, id := range got {
			changed[id] = true
		}
		return err
	}
	data := func(v any) string { b, _ := json.Marshal(v); return string(b) }
	if err := run(`INSERT INTO `+s.t.items+` AS c (tenant_id, id, kind, state, publish_at, published_at, deleted_at, original_title, original_language, comments,
 access, restrictions, attributes, owner_entity_id, created_at, updated_at)
 SELECT $1, r.id, r.kind, r.state, r.publish_at, r.published_at, r.deleted_at, r.original_title, r.original_language, r.comments, r.access, r.restrictions,
  r.attributes, r.owner_entity_id, r.created_at, r.updated_at
 FROM jsonb_to_recordset($2::jsonb) AS r(id text, kind text, state text, publish_at timestamptz, published_at timestamptz, deleted_at timestamptz,
  original_title text, original_language text, comments text, access jsonb, restrictions text[], attributes jsonb, owner_entity_id text,
  created_at timestamptz, updated_at timestamptz)
 ON CONFLICT (tenant_id, id) DO UPDATE SET state = EXCLUDED.state, publish_at = EXCLUDED.publish_at, published_at = EXCLUDED.published_at,
  deleted_at = EXCLUDED.deleted_at, deleted_without_versions=false, original_title = coalesce(EXCLUDED.original_title,c.original_title),
  original_language = coalesce(EXCLUDED.original_language,c.original_language), comments = EXCLUDED.comments,
  access = EXCLUDED.access, restrictions = EXCLUDED.restrictions, attributes = EXCLUDED.attributes, owner_entity_id = EXCLUDED.owner_entity_id,
  created_at = EXCLUDED.created_at
 WHERE (c.state, c.publish_at, c.published_at, c.deleted_at, c.original_title, c.original_language, c.comments, c.access, c.restrictions, c.attributes,
  c.owner_entity_id, c.created_at)
  IS DISTINCT FROM (EXCLUDED.state, EXCLUDED.publish_at, EXCLUDED.published_at, EXCLUDED.deleted_at, coalesce(EXCLUDED.original_title,c.original_title),
  coalesce(EXCLUDED.original_language,c.original_language), EXCLUDED.comments, EXCLUDED.access, EXCLUDED.restrictions, EXCLUDED.attributes, EXCLUDED.owner_entity_id, EXCLUDED.created_at) OR c.deleted_without_versions
 RETURNING c.id`, s.tenant, data(b.items)); err != nil {
		return nil, err
	}
	if len(b.textIDs) > 0 {
		if err := run(`DELETE FROM `+s.t.text+` t WHERE t.tenant_id = $1 AND t.content_id = ANY($2) AND NOT EXISTS (
 SELECT 1 FROM jsonb_to_recordset($3::jsonb) AS r(content_id text, language text) WHERE r.content_id = t.content_id AND r.language = t.language)
 RETURNING t.content_id`, s.tenant, b.textIDs, data(b.texts)); err != nil {
			return nil, err
		}
		if err := run(`INSERT INTO `+s.t.text+` AS t (tenant_id, content_id, language, title, slug, description, body, sort_key, aliases)
 SELECT $1, r.content_id, r.language, r.title, coalesce(r.slug, ''), coalesce(r.description, ''), coalesce(r.body, ''), coalesce(r.sort_key, ''),
  coalesce(r.aliases, '{}')
 FROM jsonb_to_recordset($2::jsonb) AS r(content_id text, language text, title text, slug text, description text, body text, sort_key text, aliases text[])
 ON CONFLICT (tenant_id, content_id, language) DO UPDATE SET title = EXCLUDED.title, slug = EXCLUDED.slug, description = EXCLUDED.description,
  body = EXCLUDED.body, sort_key = EXCLUDED.sort_key, aliases = EXCLUDED.aliases, updated_at = clock_timestamp()
 WHERE (t.title, t.slug, t.description, t.body, t.sort_key, t.aliases) IS DISTINCT FROM
  (EXCLUDED.title, EXCLUDED.slug, EXCLUDED.description, EXCLUDED.body, EXCLUDED.sort_key, EXCLUDED.aliases)
 RETURNING t.content_id`, s.tenant, data(b.texts)); err != nil {
			return nil, err
		}
	}
	if len(b.creatorID) > 0 {
		if err := run(`DELETE FROM `+s.t.creators+` c WHERE c.tenant_id = $1 AND c.content_id = ANY($2) AND NOT EXISTS (
 SELECT 1 FROM jsonb_to_recordset($3::jsonb) AS r(content_id text, user_id text, role text)
 WHERE r.content_id = c.content_id AND r.user_id = c.user_id AND r.role = c.role) RETURNING c.content_id`, s.tenant, b.creatorID, data(b.creators)); err != nil {
			return nil, err
		}
		if err := run(`INSERT INTO `+s.t.creators+` AS c (tenant_id, content_id, user_id, role, position)
 SELECT $1, r.content_id, r.user_id, r.role, r.position FROM jsonb_to_recordset($2::jsonb) AS r(content_id text, user_id text, role text, position int)
 ON CONFLICT (tenant_id, content_id, user_id, role) DO UPDATE SET position = EXCLUDED.position WHERE c.position <> EXCLUDED.position
 RETURNING c.content_id`, s.tenant, data(b.creators)); err != nil {
			return nil, err
		}
	}
	if err := run(`INSERT INTO `+s.t.versions+` AS v (tenant_id, id, content_id, code, position, languages, traits, label, state, publish_at,
 published_at, deleted_at, access, attributes, ordinal, cover, created_at, updated_at, playable)
 SELECT $1, r.id, r.content_id, r.code, r.position, r.languages, r.traits, r.label, r.state, r.publish_at, r.published_at, r.deleted_at,
  r.access, r.attributes, r.ordinal, r.cover, r.created_at, r.updated_at, r.playable
 FROM jsonb_to_recordset($2::jsonb) AS r(id text, content_id text, code text, position int, languages text[], traits text[], label text, state text,
  publish_at timestamptz, published_at timestamptz, deleted_at timestamptz, access jsonb, attributes jsonb, ordinal int, cover jsonb, created_at timestamptz,
  updated_at timestamptz, playable boolean)
 ON CONFLICT (tenant_id, id) DO UPDATE SET position = EXCLUDED.position, languages = coalesce(EXCLUDED.languages, v.languages), traits = EXCLUDED.traits,
  label = EXCLUDED.label, state = EXCLUDED.state, publish_at = EXCLUDED.publish_at, published_at = EXCLUDED.published_at, deleted_at = EXCLUDED.deleted_at,
  access = EXCLUDED.access, attributes = EXCLUDED.attributes, ordinal = EXCLUDED.ordinal, cover = coalesce(EXCLUDED.cover, v.cover), created_at = EXCLUDED.created_at,
  updated_at = EXCLUDED.updated_at, revision = v.revision + 1
 WHERE (v.position, v.languages, v.traits, v.label, v.state, v.publish_at, v.published_at, v.deleted_at, v.access, v.attributes, v.ordinal, v.cover,
  v.created_at)
  IS DISTINCT FROM (EXCLUDED.position, coalesce(EXCLUDED.languages, v.languages), EXCLUDED.traits, EXCLUDED.label, EXCLUDED.state, EXCLUDED.publish_at,
  EXCLUDED.published_at, EXCLUDED.deleted_at, EXCLUDED.access, EXCLUDED.attributes, EXCLUDED.ordinal, coalesce(EXCLUDED.cover, v.cover), EXCLUDED.created_at)
 RETURNING v.content_id`, s.tenant, data(b.versions)); err != nil {
		return nil, err
	}
	// Version rows answer their content item.
	content := func(sql string) string {
		return `WITH x AS (` + sql + ` RETURNING version_id) SELECT v.content_id FROM x JOIN ` + s.t.versions + ` v ON v.tenant_id = $1 AND v.id = x.version_id`
	}
	if len(b.vtextIDs) > 0 {
		if err := run(content(`DELETE FROM `+s.t.versionText+` t WHERE t.tenant_id = $1 AND t.version_id = ANY($2) AND NOT EXISTS (
 SELECT 1 FROM jsonb_to_recordset($3::jsonb) AS r(version_id text, language text) WHERE r.version_id = t.version_id AND r.language = t.language)`),
			s.tenant, b.vtextIDs, data(b.vtexts)); err != nil {
			return nil, err
		}
		if err := run(content(`INSERT INTO `+s.t.versionText+` AS t (tenant_id, version_id, language, title, description)
 SELECT $1, r.version_id, r.language, coalesce(r.title, ''), coalesce(r.description, '') FROM jsonb_to_recordset($2::jsonb) AS r(version_id text, language text, title text, description text)
 ON CONFLICT (tenant_id, version_id, language) DO UPDATE SET title = EXCLUDED.title, description = EXCLUDED.description
 WHERE (t.title, t.description) IS DISTINCT FROM (EXCLUDED.title, EXCLUDED.description)`), s.tenant, data(b.vtexts)); err != nil {
			return nil, err
		}
	}
	if len(b.segIDs) > 0 {
		for _, version := range b.segIDs {
			var chapters []Segment
			for _, row := range b.segments {
				if row.VersionID != version {
					continue
				}
				chapter := Segment{ID: row.ID, Position: row.Position, Title: row.Title, ReleaseAt: row.ReleaseAt}
				if row.StartPage != nil {
					chapter.StartPage = *row.StartPage
				} else if row.Start != nil && row.End != nil {
					chapter.Start, chapter.End = *row.Start, *row.End
				}
				chapters = append(chapters, chapter)
			}
			updated, err := s.writeChaptersTx(ctx, tx, version, chapters)
			if err != nil {
				return nil, err
			}
			if updated {
				for _, row := range b.versions {
					if row.ID == version {
						changed[row.ContentID] = true
						break
					}
				}
			}
		}
	}
	return changed, nil
}

// checkVersionCodes refuses codes the import can't keep: one a stored version
// holds under another, or another version of its content holds.
func (s *Store) checkVersionCodes(ctx context.Context, tx pgx.Tx, rows []importVersionRow) error {
	seen := map[[2]string]string{}
	for _, r := range rows {
		if r.Code == nil {
			continue
		}
		k := [2]string{r.ContentID, *r.Code}
		if id, ok := seen[k]; ok && id != r.ID {
			return invalid("versions %s and %s share code %s", id, r.ID, *r.Code)
		}
		seen[k] = r.ID
	}
	if len(seen) == 0 {
		return nil
	}
	data, _ := json.Marshal(rows)
	var id, code string
	err := tx.QueryRow(ctx, `SELECT r.id, r.code FROM jsonb_to_recordset($2::jsonb) AS r(id text, content_id text, code text)
 JOIN `+s.t.versions+` v ON v.tenant_id = $1 AND ((v.id = r.id AND v.code <> r.code) OR (v.content_id = r.content_id AND v.code = r.code AND v.id <> r.id))
 WHERE r.code IS NOT NULL LIMIT 1`, s.tenant, string(data)).Scan(&id, &code)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil
	case err != nil:
		return err
	}
	return fmt.Errorf("%w: version %s cannot take code %s: it has another, or another version of its content has it", ErrConflict, id, code)
}

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
	if c.OriginalTitle != nil {
		value, _, err := cleanOriginalTitle(*c.OriginalTitle, "")
		if err != nil {
			return err
		}
		c.OriginalTitle = &value
	}
	if c.OriginalLanguage != nil {
		_, value, err := cleanOriginalTitle("", *c.OriginalLanguage)
		if err != nil {
			return err
		}
		c.OriginalLanguage = &value
	}
	if c.Comments == "" {
		c.Comments = s.defaultComments(c.Kind)
	}
	if err := checkComments(c.Comments); err != nil {
		return err
	}
	// Deleted records preserve historical settings. Restore applies the
	// current policy before the retained item can become live again.
	if c.State != StateDeleted {
		if err := s.checkKindComments(c.Kind, c.Comments); err != nil {
			return err
		}
	}

	rule := s.defaultRule(c.Kind)
	if c.Access != nil {
		rule = *c.Access
	}
	if rule, err = s.checkRule(rule); err != nil {
		return err
	}
	if c.State != StateDeleted {
		if err := s.checkKindRule(c.Kind, rule); err != nil {
			return err
		}
	}
	c.Access = &rule
	if c.Restrictions, err = s.checkRestrictions(c.Restrictions); err != nil {
		return err
	}
	if c.Attributes, err = checkAttributes(k.Attributes, c.Attributes); err != nil {
		return err
	}
	if c.State == StatePublished {
		if err := kinds.ValidateRequiredAttributes(k.Attributes, c.Attributes); err != nil {
			return invalid("%v", err)
		}
	}
	if c.ChannelID != "" {
		if err := contentref.ValidateID(c.ChannelID); err != nil {
			return invalid("owner group: %v", err)
		}
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
		if err := s.checkImportVersion(c.Kind, c.State == StateDeleted, &c.Versions[i]); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) checkImportVersion(kind string, parentDeleted bool, v *ImportVersion) error {
	k := s.cfg.Kinds[kind]
	if err := contentref.ValidateID(v.ID); err != nil {
		return invalid("version id: %v", err)
	}
	if v.Cover != "" && !media.ValidNamed(v.Cover) {
		return invalid("cover must be an image name (i-{uuid})")
	}
	if v.Code != "" {
		code, err := contenturl.ParseVersionCode(v.Code)
		if err != nil {
			return invalid("code %q is not a version code", v.Code)
		}
		v.Code = string(code)
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
		// Deleted imports preserve historical access, including versions whose
		// parent is deleted while their own state remains published. Restoring
		// either the item or version applies the current kind policy.
		if !parentDeleted && v.State != StateDeleted {
			if err := s.checkKindRule(kind, r); err != nil {
				return err
			}
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
		for i, chapter := range v.Segments {
			if chapter.StartPage == "" || chapter.Start != 0 || chapter.End != 0 {
				return invalid("chapter %d: start_page must name a virtual manifest file; numeric ranges are migration-only", i+1)
			}
		}
		// Import's transaction belongs to its caller and may already hold
		// catalog rows. Taking a manifest lock here would invert upload's lock
		// order; omitting it would race source removal and marker validation.
		return invalid("import content and source media first, then update chapters with PUT /contents/{id}/versions/{version}/segments using start_page; bulk import cannot safely mutate chapters")
	}
	return nil
}

// lockStamped locks the rows sql reads (id, kind, content_id, state,
// publish_at, published_at, deleted_at, created_at, access for $2's ids), by id.
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
		if err := rows.Scan(&id, &r.kind, &r.contentID, &r.state, &r.publishAt, &r.publishedAt, &r.deletedAt, &r.createdAt, &r.access); err != nil {
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
 WHERE NOT EXISTS (SELECT 1 FROM `+s.t.entities+` g WHERE g.tenant_id = $1 AND g.id = u.id AND g.state = 'active')`, s.tenant, ids).Scan(&missing); err != nil {
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
  OR (r.id = ANY($4) AND (SELECT coalesce(jsonb_agg(jsonb_build_array(g.position, g.start_index, g.end_index, g.release_at, g.start_page) ORDER BY g.position), '[]')
   FROM `+s.t.segments+` g WHERE g.tenant_id = $1 AND g.version_id = r.id) IS DISTINCT FROM
   (SELECT coalesce(jsonb_agg(jsonb_build_array(n.position, n.start_index, n.end_index, n.release_at, n.start_page) ORDER BY n.position), '[]')
   FROM jsonb_to_recordset($5::jsonb) AS n(version_id text, position int, start_index int, end_index int, release_at timestamptz, start_page text) WHERE n.version_id = r.id))
 ORDER BY r.id`, s.tenant, data(had), data(b.items), append([]string{}, b.segIDs...), data(append([]importSegmentRow{}, b.segments...)))
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

// ImportDeletes preserves the source's independent content tombstones and
// follows version deletion, including hidden versions, with automatic
// empty-parent deletion. All soft-deleted content follows the deletion retention.
func (s *Store) ImportDeletes(ctx context.Context, tx pgx.Tx, contents, versions []string) error {
	var events []Event
	touched := map[string]bool{}
	locked, err := tx.Query(ctx, `SELECT c.id FROM `+s.t.items+` c WHERE c.tenant_id=$1 AND
 (c.id=ANY($2) OR c.id IN (SELECT v.content_id FROM `+s.t.versions+` v WHERE v.tenant_id=$1 AND v.id=ANY($3))) ORDER BY c.id FOR UPDATE`, s.tenant, contents, versions)
	if err != nil {
		return err
	}
	var releaseIDs []string
	for locked.Next() {
		var id string
		if err := locked.Scan(&id); err != nil {
			locked.Close()
			return err
		}
		releaseIDs = append(releaseIDs, id)
	}
	err = locked.Err()
	locked.Close()
	if err != nil {
		return err
	}
	if _, err := s.recordReleases(ctx, tx, releaseIDs); err != nil {
		return err
	}
	if len(contents) > 0 {
		rows, err := tx.Query(ctx, `UPDATE `+s.t.items+` SET state = 'deleted', deleted_at = clock_timestamp(),deleted_without_versions=false
 WHERE tenant_id = $1 AND id = ANY($2) AND (state <> 'deleted' OR deleted_without_versions) RETURNING id, kind`, s.tenant, contents)
		if err != nil {
			return err
		}
		for rows.Next() {
			var e Event
			if err := rows.Scan(&e.ContentID, &e.Kind); err != nil {
				rows.Close()
				return err
			}
			e.Type, touched[e.ContentID] = EventContentDeleted, true
			events = append(events, e)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
	}
	if len(versions) > 0 {
		rows, err := tx.Query(ctx, `UPDATE `+s.t.versions+` v SET state = 'deleted', deleted_at = clock_timestamp(), updated_at = clock_timestamp(), revision = v.revision + 1
 FROM `+s.t.items+` c WHERE v.tenant_id = $1 AND v.id = ANY($2) AND v.state <> 'deleted' AND c.tenant_id = v.tenant_id AND c.id = v.content_id
 RETURNING v.content_id, v.id, c.kind`, s.tenant, versions)
		if err != nil {
			return err
		}
		for rows.Next() {
			e := Event{Type: EventVersionDeleted}
			if err := rows.Scan(&e.ContentID, &e.VersionID, &e.Kind); err != nil {
				rows.Close()
				return err
			}
			touched[e.ContentID] = true
			events = append(events, e)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
	}
	if len(touched) == 0 {
		return nil
	}
	ids := slices.Sorted(maps.Keys(touched))
	deleted, err := s.deleteEmptyParents(ctx, tx, ids)
	if err != nil {
		return err
	}
	events = append(events, deleted...)
	return s.apply(ctx, tx, ids, effects{events: events})
}
