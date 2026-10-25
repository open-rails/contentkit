package discovery

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/contenturl"
	"github.com/open-rails/contentkit/entities"
	"github.com/open-rails/contentkit/internal/edition"
	"github.com/open-rails/contentkit/kinds"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/versions"
	"github.com/open-rails/contentkit/popularity"
	"github.com/open-rails/contentkit/signal"
)

// Card is one content item as a grid shows it, with the version that fits
// the reader best: of the versions the query matches, the one the display
// language opens by default (internal/edition), whose cover it shows. Of a
// version the reader does not open it shows what its kind's locked view does
// (kinds.Locked).
type Card struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	// Code and Path are its page, /{lang}/{route}/{code}/{slug}, which opens
	// the card's version: with ?v= when that is not the page's default.
	Code string `json:"code"`
	Path string `json:"path"`
	// Title is in Language: the display language, else a fallback.
	Title    string                  `json:"title"`
	Language string                  `json:"language"`
	Cover    []media.PublicRendition `json:"cover"`
	Version  CardVersion             `json:"version"`
	// Entities are the version's effective entities with a page, by entity kind.
	Entities map[string][]Chip `json:"entities"`
	// Owner is the owner entity's card (a channel's name, path, avatar and
	// cover).
	Owner       *entities.EntityCard `json:"owner,omitempty"`
	Counts      Counts               `json:"counts"`
	Access      CardAccess           `json:"access"`
	Viewer      *catalog.ViewerState `json:"viewer,omitempty"`
	PublishedAt *time.Time           `json:"published_at,omitempty"`
	Attributes  map[string]any       `json:"attributes"`
	// Ranking is present for a population-normalized popular result. Raw
	// participation is retained alongside its single reliability adjustment.
	Ranking *ContentRanking `json:"ranking,omitempty"`
	// UpdatedAt is when the work last gained something after this card's
	// version was published: a newer version, or pages added to this one
	// (new chapters); absent until then.
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
	// Deleted marks a deleted item or version for staff and its creators.
	Deleted bool `json:"deleted,omitempty"`
	// Excerpt is a post kind's plain-text summary, about 300 characters: its
	// description, else its body's opening for readers who open it.
	Excerpt string `json:"excerpt,omitempty"`
}

type ContentRanking struct {
	Score                 float64 `json:"score"`
	Participation         float64 `json:"participation"`
	AdjustedParticipation float64 `json:"adjusted_participation"`
}

// CardVersion is the version a card shows; Code selects it on the card's
// page (?v=).
type CardVersion struct {
	ID         string   `json:"id"`
	Code       string   `json:"code"`
	Languages  []string `json:"languages"`
	Traits     []string `json:"traits"`
	Label      string   `json:"label,omitempty"`
	Pages      *int     `json:"pages,omitempty"`
	DurationMS *int64   `json:"duration_ms,omitempty"`
	HasAudio   bool     `json:"has_audio"`
	// HLS is the folder of its ladder (a read's hls entry) once its video is
	// in, for a reader who opens it: the card previews it inline.
	HLS string `json:"hls,omitempty"`
}

// Chip names one entity on a card. Position is the content's sort key in an
// ordered entity: auto-filled from 1, but a host may set any value of 0 or more.
type Chip struct {
	ID       string `json:"id"`
	Slug     string `json:"slug"`
	Name     string `json:"name"`
	Path     string `json:"path,omitempty"`
	Position *int   `json:"position,omitempty"`
}

// Counts are explicit reactions and favorites; reading quality is not a public open counter.
type Counts struct {
	Likes     *int `json:"likes,omitempty"`
	Dislikes  *int `json:"dislikes,omitempty"`
	Favorites *int `json:"favorites,omitempty"`
}

// CardAccess is the reader's access decision on the card's version.
type CardAccess struct {
	Allowed bool `json:"allowed"`
	// Level and Tiers are the version's effective rule, the same for every
	// reader (public once free): what a premium badge shows.
	Level access.Level `json:"level"`
	Tiers []string     `json:"tiers,omitempty"`
	// Unknown: entitlements could not be read; denied, nothing offered.
	Unknown bool `json:"unknown,omitempty"`
	// Sell are the keys whose purchase opens it; Requires is the key a buyer
	// holds first.
	Sell     []string         `json:"sell,omitempty"`
	Requires string           `json:"requires,omitempty"`
	Preview  *access.Preview  `json:"preview,omitempty"`
	Releases []access.Release `json:"releases,omitempty"`
}

// ContentPage is one page of cards.
type ContentPage struct {
	Contents []Card          `json:"contents"`
	Summary  *ListingSummary `json:"summary,omitempty"`
	// NextCursor pages on; empty on the last page.
	NextCursor string                  `json:"next_cursor,omitempty"`
	Ranking    *popularity.RankingInfo `json:"ranking,omitempty"`
}

// cardOptions shape hydration.
type cardOptions struct {
	// chips are the entity kinds cards name; nil is every kind with a page.
	chips []string
	// lean leaves out entities and the reader's state (typeahead).
	lean bool
}

type cardRow struct {
	Card
	contentRule access.Rule
	override    *access.Rule
	entityIDs   []string
	owner       string
	creator     bool
	slug        string
	description string
	bodyExcerpt string
	counts      struct {
		likes, dislikes, favorites int
	}
}

// shows reports whether the reader sees property p of the card: all of what
// they open, of the rest what its kind's locked view shows.
func (s *Service) shows(c *cardRow, p string) bool {
	return c.Access.Allowed || s.o.Kinds.Kinds[c.Kind].LockedView().Shows(p)
}

// lock withholds from the cards the reader does not open what their kinds'
// locked views hide; counts show only where shown.
func (s *Service) lock(rows []cardRow) {
	for i := range rows {
		c := &rows[i]
		n := c.counts
		if s.shows(c, kinds.ShowReactions) {
			c.Counts.Likes, c.Counts.Dislikes = &n.likes, &n.dislikes
		}
		if s.shows(c, kinds.ShowFavorites) {
			c.Counts.Favorites = &n.favorites
		}
		if !s.shows(c, kinds.ShowAttributes) {
			c.Attributes = map[string]any{}
		}
		if !s.shows(c, kinds.ShowLength) {
			c.Version.Pages, c.Version.DurationMS = nil, nil
		}
		if !s.shows(c, kinds.ShowEntities) {
			c.entityIDs = nil
		}
		if s.o.Kinds.Kinds[c.Kind].Presentation == "post" {
			text := c.description
			if !s.shows(c, kinds.ShowDescription) {
				text = ""
			}
			if text == "" && c.Access.Allowed {
				text = c.bodyExcerpt
			}
			c.Excerpt = excerpt(text, excerptChars)
		}
	}
}

// standingTimeout bounds a card list's progress read.
const standingTimeout = 500 * time.Millisecond

// excerptChars bounds a card excerpt; clients trim further.
const excerptChars = 300

// excerpt cuts text to at most n characters at a word boundary, with an ellipsis.
func excerpt(text string, n int) string {
	text = strings.Join(strings.Fields(text), " ")
	r := []rune(text)
	if len(r) <= n {
		return text
	}
	cut := string(r[:n])
	if i := strings.LastIndexByte(cut, ' '); i > len(cut)*3/5 {
		cut = cut[:i]
	}
	return strings.TrimRight(cut, " .,;:!?-") + "…"
}

// cardRows hydrates and settles picks in order, applying locked-property
// redaction before a caller can use entity memberships. In hide mode a card
// the reader does not open is dropped. It does not fetch covers or names.
func (s *Service) cardRows(ctx context.Context, q *sel, picks []pick) ([]cardRow, error) {
	out := []cardRow{}
	if len(picks) == 0 {
		return out, nil
	}
	vids := make([]string, len(picks))
	for i, p := range picks {
		vids[i] = p.version
	}
	// The page opens the version its language does by default among those
	// the reader may see; another one needs ?v=.
	listed, largs := q.viewer.Shown("g")
	titleKinds := []string{}
	for kind, spec := range s.o.Kinds.Kinds {
		if spec.TitlePolicy == kinds.TitleOriginalAndVersion {
			titleKinds = append(titleKinds, kind)
		}
	}
	args := pgx.NamedArgs{"d_title_kinds": titleKinds, "d_vids": vids, "d_tenant": s.o.Tenant, "d_display": q.display, "d_langs": s.languages(), "d_user": person(q.reader.Actor)}
	for k, v := range largs {
		args[k] = v
	}
	rows, err := s.o.Pool.Query(ctx, fmt.Sprintf(`SELECT c.id, c.kind, c.access, v.access, f.languages, f.traits, f.entity_ids, coalesce(counts.likes, 0), coalesce(counts.dislikes, 0), coalesce(counts.favorites, 0),
 f.published_at, f.attributes, v.id, v.code, v.label, v.pages, v.duration_ms, v.has_audio,
 coalesce(cd.code, ''), coalesce(cd.slugs->>@d_display, cd.slug, ''), coalesce(nullif(vt.title, ''), CASE WHEN c.kind=ANY(@d_title_kinds::text[]) THEN nullif(c.original_title,'') END, t.title, ''),
 CASE WHEN nullif(vt.title,'') IS NOT NULL THEN vt.language WHEN c.kind=ANY(@d_title_kinds::text[]) AND nullif(c.original_title,'') IS NOT NULL THEN coalesce(nullif(c.original_language,''),t.language,'') ELSE coalesce(t.language,'') END,
 coalesce(nullif(vt.description, ''), t.description, ''), coalesce(t.body_excerpt, ''),
 coalesce(f.owner_entity_id, ''), @d_user <> '' AND EXISTS (SELECT 1 FROM %[7]s cc WHERE cc.tenant_id = c.tenant_id AND cc.content_id = c.id AND cc.user_id = @d_user),
 coalesce((SELECT g.version_id FROM %[1]s g WHERE g.tenant_id = f.tenant_id AND g.content_id = f.content_id AND %[8]s
  ORDER BY %[9]s LIMIT 1) = f.version_id, true)
FROM unnest(@d_vids::text[]) WITH ORDINALITY AS p(vid, ord)
JOIN %[1]s f ON f.tenant_id = @d_tenant AND f.version_id = p.vid
JOIN %[2]s c ON c.tenant_id = f.tenant_id AND c.id = f.content_id
JOIN %[3]s v ON v.tenant_id = f.tenant_id AND v.id = f.version_id
LEFT JOIN %[10]s counts ON counts.tenant_id = f.tenant_id AND counts.content_kind = f.kind AND counts.content_id = f.content_id AND counts.content_version_id IS NULL
LEFT JOIN %[4]s cd ON cd.tenant_id = c.tenant_id AND cd.content_kind = c.kind AND cd.content_id = c.id
LEFT JOIN LATERAL (SELECT x.title, x.language, left(x.description, 600) AS description, x.body_excerpt FROM %[5]s x WHERE x.tenant_id = c.tenant_id AND x.content_id = c.id
 ORDER BY x.language = @d_display DESC, array_position(@d_langs::text[], x.language), x.language LIMIT 1) t ON true
LEFT JOIN LATERAL (SELECT x.title,x.description,x.language FROM %[6]s x
 WHERE x.tenant_id=v.tenant_id AND x.version_id=v.id
 AND ((c.kind=ANY(@d_title_kinds::text[]) AND x.language=ANY(v.languages)) OR (NOT(c.kind=ANY(@d_title_kinds::text[])) AND x.language=t.language))
 ORDER BY (nullif(x.title,'') IS NOT NULL) DESC,x.language=@d_display DESC,array_position(v.languages,x.language),x.language LIMIT 1) vt ON true
ORDER BY p.ord`, s.t.facets, s.t.items, s.t.versions, s.t.codes, s.t.text, s.t.versionText, s.t.creators, listed, edition.Order("g", "@d_display::text"), s.t.counts),
		args)
	if err != nil {
		return nil, err
	}
	got, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (cardRow, error) {
		var c cardRow
		var code string
		var isDefault bool
		err := r.Scan(&c.ID, &c.Kind, &c.contentRule, &c.override, &c.Version.Languages, &c.Version.Traits, &c.entityIDs, &c.counts.likes,
			&c.counts.dislikes, &c.counts.favorites, &c.PublishedAt, &c.Attributes, &c.Version.ID, &c.Version.Code, &c.Version.Label, &c.Version.Pages, &c.Version.DurationMS,
			&c.Version.HasAudio, &code, &c.slug, &c.Title, &c.Language, &c.description, &c.bodyExcerpt, &c.owner, &c.creator, &isDefault)
		if err != nil {
			return c, err
		}
		c.Code = code
		if route := s.o.Kinds.Kinds[c.Kind].Route; route != "" && code != "" {
			c.Path = contenturl.Path(s.o.Kinds.Languages, q.display, route, contenturl.Code(code), c.slug)
			if !isDefault {
				c.Path = contenturl.Versioned(c.Path, contenturl.VersionCode(c.Version.Code))
			}
		}
		if c.Attributes == nil {
			c.Attributes = map[string]any{}
		}
		c.Cover, c.Entities = []media.PublicRendition{}, map[string][]Chip{}
		return c, nil
	})
	if err != nil {
		return nil, err
	}
	if err := s.settle(ctx, q, got); err != nil {
		return nil, err
	}
	// Hide mode needs the exact verdict after an
	// incomplete entitlement keyspace admitted them as SQL candidates.
	got = slices.DeleteFunc(got, func(c cardRow) bool {
		return !c.Access.Allowed && q.reader.Mode == access.Hide
	})
	s.lock(got)
	return got, nil
}

// cards fills the settled rows' public media, names and viewer state.
func (s *Service) cards(ctx context.Context, q *sel, picks []pick, o cardOptions) ([]Card, error) {
	got, err := s.cardRows(ctx, q, picks)
	if err != nil {
		return nil, err
	}
	if err := s.cardMedia(ctx, got); err != nil {
		return nil, err
	}
	if !o.lean {
		if err := s.chips(ctx, q, got, o.chips); err != nil {
			return nil, err
		}
		if err := s.owners(ctx, q, got); err != nil {
			return nil, err
		}
		if err := s.viewerState(ctx, q.reader.Actor, got); err != nil {
			return nil, err
		}
		if err := s.cardStatus(ctx, got, q.viewer.Staff()); err != nil {
			return nil, err
		}
	}
	out := make([]Card, 0, len(got))
	for _, c := range got {
		out = append(out, c.Card)
	}
	return out, nil
}

// settle asks the Gate for every card's version in one call. Editors open
// what they edit (staff, its creators, its owner entity's editors):
// owner-entity editorship is asked only of cards the rule leaves locked, once
// per owner entity.
func (s *Service) settle(ctx context.Context, q *sel, rows []cardRow) error {
	items := make([]access.Item, len(rows))
	for i, c := range rows {
		items[i] = access.Item{Content: c.ID, Version: c.Version.ID, Rule: access.Effective(c.contentRule, c.override).Previewed(s.o.Kinds.Kinds[c.Kind]),
			Entities: c.entityIDs}
	}
	ds, err := s.o.Gate.Decide(ctx, q.reader.Actor, items)
	if err != nil && (ds == nil || !errors.Is(err, access.ErrUnavailable)) {
		return err
	}
	internal, err := s.internalAccessEntities(ctx, items, ds)
	if err != nil {
		return err
	}
	now := s.o.Now()
	for i, d := range ds {
		a := CardAccess{Allowed: d.Allowed, Unknown: d.Unknown, Preview: d.Preview, Releases: d.Releases, Level: access.Public}
		a.Releases = slices.DeleteFunc(slices.Clone(a.Releases), func(r access.Release) bool { return internal[r.Entity] })
		if r := items[i].Rule; !r.Free(now) {
			a.Level, a.Tiers = r.Level, r.AudienceTiers(now)
		}
		for _, k := range d.Sell {
			if k.Kind() != access.KeyEntity || !internal[k.Entity()] {
				a.Sell = append(a.Sell, k.String())
			}
		}
		if d.Requires != nil {
			if d.Requires.Kind() == access.KeyEntity && internal[d.Requires.Entity()] {
				a.Sell = nil
			} else {
				a.Requires = d.Requires.String()
			}
		}
		if internal[items[i].Rule.Entity] && (a.Level == access.EntityLevel || a.Level == access.EntityPPV) {
			a.Level = access.PPV
		}
		rows[i].Access = a
	}
	var locked []string
	for _, c := range rows {
		if !c.Access.Allowed && !c.creator && c.owner != "" {
			locked = append(locked, c.owner)
		}
	}
	editors := q.viewer.Editors(ctx, locked)
	for i := range rows {
		c := &rows[i]
		if !c.Access.Allowed && (q.viewer.Staff() || c.creator || editors[c.owner]) {
			c.Access = CardAccess{Allowed: true, Level: c.Access.Level, Tiers: c.Access.Tiers}
		}
	}
	return nil
}

// cardMedia fills the cards' covers, and the ladders of the videos their
// readers open.
func (s *Service) cardMedia(ctx context.Context, rows []cardRow) error {
	if s.o.Media == nil || len(rows) == 0 {
		return nil
	}
	var asked []versions.CoverOf
	var at []int
	for i := range rows {
		if c := &rows[i]; c.Access.Allowed && c.Version.DurationMS != nil {
			c.Version.HLS = s.o.Media.Ladder(c.Kind)
		}
		if s.shows(&rows[i], kinds.ShowCover) {
			asked, at = append(asked, versions.CoverOf{Kind: rows[i].Kind, ID: rows[i].ID, Version: rows[i].Version.ID}), append(at, i)
		}
	}
	if len(asked) == 0 {
		return nil
	}
	got, err := s.o.Media.Covers(ctx, asked)
	if err != nil {
		return err
	}
	for j, i := range at {
		if got[j] != nil {
			rows[i].Cover = got[j]
		}
	}
	return nil
}

// chips names the cards' entities of the kinds asked, by name.
func (s *Service) chips(ctx context.Context, q *sel, rows []cardRow, kinds []string) error {
	var ids []entities.EntityID
	seen := map[string]bool{}
	for _, c := range rows {
		for _, g := range c.entityIDs {
			if !seen[g] {
				seen[g] = true
				ids = append(ids, entities.EntityID(g))
			}
		}
	}
	if len(ids) == 0 {
		return nil
	}
	cards, err := s.o.Entities.Cards(ctx, ids, entities.CardOptions{Language: q.display, NamesOnly: true})
	if err != nil {
		return err
	}
	byID := make(map[string]entities.EntityCard, len(cards))
	for _, g := range cards {
		shown := slices.Contains(kinds, g.Kind)
		if kinds == nil {
			shown = s.o.Kinds.EntityKinds[g.Kind].Route != ""
		}
		if shown {
			byID[string(g.ID)] = g
		}
	}
	positions, err := s.positions(ctx, rows, byID)
	if err != nil {
		return err
	}
	for i := range rows {
		for _, id := range rows[i].entityIDs {
			if g, ok := byID[id]; ok {
				chip := Chip{ID: id, Slug: g.Slug, Name: g.Name, Path: g.Path}
				if p, ok := positions[[2]string{rows[i].ID, id}]; ok {
					chip.Position = &p
				}
				rows[i].Entities[g.Kind] = append(rows[i].Entities[g.Kind], chip)
			}
		}
		for _, cs := range rows[i].Entities {
			slices.SortFunc(cs, func(a, b Chip) int { return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)) })
		}
	}
	return nil
}

// positions reads each card's place in the ordered entities it shows, keyed by
// content and entity: the membership of its content or of its shown version
// that is active now, as the card's own entities count it.
func (s *Service) positions(ctx context.Context, rows []cardRow, shown map[string]entities.EntityCard) (map[[2]string]int, error) {
	var ordered []string
	for id, g := range shown {
		if s.o.Kinds.EntityKinds[g.Kind].Ordered {
			ordered = append(ordered, id)
		}
	}
	out := map[[2]string]int{}
	if len(ordered) == 0 {
		return out, nil
	}
	contents, versions := make([]string, len(rows)), make([]string, len(rows))
	for i, c := range rows {
		contents[i], versions[i] = c.ID, c.Version.ID
	}
	r, err := s.o.Pool.Query(ctx, `SELECT m.content_id, m.entity_id, min(m.position)
 FROM unnest($3::text[], $4::text[]) AS v(content_id, version_id)
 JOIN `+s.t.memberships+` m ON m.tenant_id = $1 AND m.content_id = v.content_id AND (m.version_id IS NULL OR m.version_id = v.version_id)
 WHERE m.entity_id = ANY($2) AND m.state = 'active' AND m.position IS NOT NULL
 GROUP BY m.content_id, m.entity_id`, s.o.Tenant, ordered, contents, versions)
	if err != nil {
		return nil, err
	}
	var content, entity string
	var position int
	_, err = pgx.ForEachRow(r, []any{&content, &entity, &position}, func() error {
		out[[2]string{content, entity}] = position
		return nil
	})
	return out, err
}

// owners fills the cards' owner entities, avatars and covers resolved, in one
// call.
func (s *Service) owners(ctx context.Context, q *sel, rows []cardRow) error {
	var ids []entities.EntityID
	for _, c := range rows {
		if c.owner != "" && !slices.Contains(ids, entities.EntityID(c.owner)) {
			ids = append(ids, entities.EntityID(c.owner))
		}
	}
	if len(ids) == 0 {
		return nil
	}
	cards, err := s.o.Entities.Cards(ctx, ids, entities.CardOptions{Language: q.display, Covers: true})
	if err != nil {
		return err
	}
	for i := range rows {
		for j := range cards {
			if string(cards[j].ID) == rows[i].owner {
				rows[i].Owner = &cards[j]
			}
		}
	}
	return nil
}

// person is a signed-in person's id, else "".
func person(a access.Actor) string {
	if a.Member() {
		return a.AccountID()
	}
	return ""
}

// viewerState fills a signed-in reader's standing with the cards' items.
func (s *Service) viewerState(ctx context.Context, actor access.Actor, rows []cardRow) error {
	if !actor.Member() || len(rows) == 0 {
		return nil
	}
	refs := make([]contentref.ContentRef, len(rows))
	for i, c := range rows {
		refs[i] = contentref.New(s.o.Tenant, c.Kind, c.ID)
	}
	got, err := s.standing(ctx, actor, refs, standingTimeout)
	if err != nil {
		return err
	}
	for i := range rows {
		rows[i].Viewer = &got[i]
	}
	return nil
}

// Standing is a signed-in person's standing with content items and versions,
// per ref in order: their favorites and, with a signal plane, their progress.
// A failed signal read leaves progress out.
func (s *Service) Standing(ctx context.Context, actor access.Actor, refs []contentref.ContentRef) ([]catalog.ViewerState, error) {
	return s.standing(ctx, actor, refs, 0)
}

// standing is Standing with its progress read bounded by limit (0: none).
func (s *Service) standing(ctx context.Context, actor access.Actor, refs []contentref.ContentRef, limit time.Duration) ([]catalog.ViewerState, error) {
	out := make([]catalog.ViewerState, len(refs))
	if !actor.Member() || len(refs) == 0 {
		return out, nil
	}
	kinds, ids, versions := make([]string, len(refs)), make([]string, len(refs)), make([]string, len(refs))
	for i, r := range refs {
		kinds[i], ids[i], versions[i] = r.ContentKind, r.ContentID, r.Version()
	}
	fav, err := s.o.Pool.Query(ctx, `SELECT r.n FROM unnest($3::text[], $4::text[], $5::text[]) WITH ORDINALITY AS r(kind, id, version, n)
 WHERE EXISTS (SELECT 1 FROM `+s.t.favorites+` f WHERE f.tenant_id = $1 AND f.user_id = $2 AND f.content_kind = r.kind AND f.content_id = r.id
  AND f.content_version_id IS NOT DISTINCT FROM nullif(r.version, '') AND f.value = 1)`, s.o.Tenant, actor.AccountID(), kinds, ids, versions)
	if err != nil {
		return nil, err
	}
	favorited, err := pgx.CollectRows(fav, pgx.RowTo[int64])
	if err != nil {
		return nil, err
	}
	for _, n := range favorited {
		out[n-1].Favorited = true
	}
	// A version's own reaction wins over one on the whole item.
	reacted, err := s.o.Pool.Query(ctx, `SELECT r.n, x.value FROM unnest($3::text[], $4::text[], $5::text[]) WITH ORDINALITY AS r(kind, id, version, n)
 JOIN LATERAL (SELECT a.value FROM `+s.t.reactions+` a WHERE a.tenant_id = $1 AND a.user_id = $2 AND a.content_kind = r.kind AND a.content_id = r.id
  AND a.value <> 0 AND (a.content_version_id IS NULL OR a.content_version_id = nullif(r.version, ''))
  ORDER BY a.content_version_id IS NULL, a.updated_at DESC LIMIT 1) x ON true`, s.o.Tenant, actor.AccountID(), kinds, ids, versions)
	if err != nil {
		return nil, err
	}
	reactions, err := pgx.CollectRows(reacted, func(row pgx.CollectableRow) (struct{ n, value int64 }, error) {
		var v struct{ n, value int64 }
		err := row.Scan(&v.n, &v.value)
		return v, err
	})
	if err != nil {
		return nil, err
	}
	for _, r := range reactions {
		out[r.n-1].Reaction = int(r.value)
	}
	if !s.o.Hub.SignalPlane() {
		return out, nil
	}
	readCtx := ctx
	if limit > 0 {
		// Progress is optional on a card: a slow signal plane drops it, not the list.
		var cancel context.CancelFunc
		readCtx, cancel = context.WithTimeout(ctx, limit)
		defer cancel()
	}
	states, err := s.o.Hub.States(readCtx, signal.Subject{UserID: actor.AccountID()}, refs)
	if err != nil {
		s.o.Logger.WarnContext(ctx, "discovery: viewer progress unavailable", "err", err)
		return out, nil
	}
	for i, ref := range refs {
		st, ok := states[ref.Key()]
		if !ok {
			continue
		}
		v := &out[i]
		v.Seen, v.Completed, v.Progress, v.ProgressMax, v.Resume = st.Seen, st.Completed, st.MaxProgress, st.ProgressMax, st.Resume
		if !st.LastReadAt.IsZero() {
			t := st.LastReadAt
			v.LastReadAt = &t
		}
	}
	return out, nil
}
