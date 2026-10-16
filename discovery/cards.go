package discovery

import (
	"context"
	"encoding/json"
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
	"github.com/open-rails/contentkit/groups"
	"github.com/open-rails/contentkit/internal/edition"
	"github.com/open-rails/contentkit/kinds"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/versions"
	"github.com/open-rails/contentkit/offers"
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
	// Groups are the version's effective groups with a page, by group kind.
	Groups map[string][]Chip `json:"entities"`
	// Owner is the owner group's card (a channel's name, path, avatar and
	// cover).
	Owner       *groups.GroupCard    `json:"owner,omitempty"`
	Counts      Counts               `json:"counts"`
	Access      CardAccess           `json:"access"`
	Viewer      *catalog.ViewerState `json:"viewer,omitempty"`
	PublishedAt *time.Time           `json:"published_at,omitempty"`
	Attributes  map[string]any       `json:"attributes"`
	// Ranking is present for a population-normalized popular result. Raw
	// participation is retained alongside its single reliability adjustment.
	Ranking *ContentRanking `json:"ranking,omitempty"`
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

// Chip names one group on a card.
type Chip struct {
	ID   string `json:"id"`
	Slug string `json:"slug"`
	Name string `json:"name"`
	Path string `json:"path,omitempty"`
}

// Counts are a content item's interaction counts; Views counts its distinct
// viewers of all time plus its imported legacy view count. A count its
// kind's locked view hides from the reader is absent.
type Counts struct {
	Likes     *int   `json:"likes,omitempty"`
	Dislikes  *int   `json:"dislikes,omitempty"`
	Favorites *int   `json:"favorites,omitempty"`
	Views     *int64 `json:"views,omitempty"`
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
	Sell      []string        `json:"sell,omitempty"`
	Requires  string          `json:"requires,omitempty"`
	Preview   *access.Preview `json:"preview,omitempty"`
	FreeAfter *time.Time      `json:"free_after,omitempty"`
	// Offers are the purchasable offers of Sell's keys; tiers are the host's
	// to label.
	Offers []offers.Sale `json:"offers,omitempty"`
}

// ContentPage is one page of cards.
type ContentPage struct {
	Contents []Card `json:"contents"`
	// NextCursor pages on; empty on the last page.
	NextCursor string                  `json:"next_cursor,omitempty"`
	Ranking    *popularity.RankingInfo `json:"ranking,omitempty"`
}

// cardOptions shape hydration.
type cardOptions struct {
	// chips are the group kinds cards name; nil is every kind with a page.
	chips []string
	// lean leaves out groups and the reader's state (typeahead).
	lean bool
}

// popularityFacet is content_facets.popularity: policy scores and window
// ratings by period, and all-time viewers.
type popularityFacet struct {
	Score  map[string]float64 `json:"score,omitempty"`
	Rating map[string]float64 `json:"rating,omitempty"`
	Views  int64              `json:"views,omitempty"`
}

type cardRow struct {
	Card
	contentRule access.Rule
	override    *access.Rule
	groupIDs    []string
	owner       string
	creator     bool
	listing     string
	slug        string
	counts      struct {
		likes, dislikes, favorites int
		views                      int64
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
		if s.shows(c, kinds.ShowViews) {
			c.Counts.Views = &n.views
		}
		if !s.shows(c, kinds.ShowAttributes) {
			c.Attributes = map[string]any{}
		}
		if !s.shows(c, kinds.ShowLength) {
			c.Version.Pages, c.Version.DurationMS = nil, nil
		}
		if !s.shows(c, kinds.ShowGroups) {
			c.groupIDs = nil
		}
	}
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
	args := pgx.NamedArgs{"d_vids": vids, "d_tenant": s.o.Tenant, "d_display": q.display, "d_langs": s.languages(), "d_user": person(q.reader.Actor)}
	for k, v := range largs {
		args[k] = v
	}
	rows, err := s.o.Pool.Query(ctx, fmt.Sprintf(`SELECT c.id, c.kind, c.access, v.access, f.languages, f.traits, f.group_ids, f.likes, f.dislikes, f.favorites,
 f.popularity, f.view_baseline, f.published_at, f.attributes, v.id, v.code, v.label, v.pages, v.duration_ms, v.has_audio, f.listing,
 coalesce(cd.code, ''), coalesce(cd.slugs->>@d_display, cd.slug, ''), coalesce(nullif(vt.title, ''), t.title, ''), coalesce(t.language, ''),
 coalesce(f.owner_group_id, ''), @d_user <> '' AND EXISTS (SELECT 1 FROM %[7]s cc WHERE cc.tenant_id = c.tenant_id AND cc.content_id = c.id AND cc.user_id = @d_user),
 coalesce((SELECT g.version_id FROM %[1]s g WHERE g.tenant_id = f.tenant_id AND g.content_id = f.content_id AND %[8]s
  ORDER BY %[9]s LIMIT 1) = f.version_id, true)
FROM unnest(@d_vids::text[]) WITH ORDINALITY AS p(vid, ord)
JOIN %[1]s f ON f.tenant_id = @d_tenant AND f.version_id = p.vid
JOIN %[2]s c ON c.tenant_id = f.tenant_id AND c.id = f.content_id
JOIN %[3]s v ON v.tenant_id = f.tenant_id AND v.id = f.version_id
LEFT JOIN %[4]s cd ON cd.tenant_id = c.tenant_id AND cd.content_kind = c.kind AND cd.content_id = c.id
LEFT JOIN LATERAL (SELECT x.title, x.language FROM %[5]s x WHERE x.tenant_id = c.tenant_id AND x.content_id = c.id
 ORDER BY x.language = @d_display DESC, array_position(@d_langs::text[], x.language), x.language LIMIT 1) t ON true
LEFT JOIN %[6]s vt ON vt.tenant_id = v.tenant_id AND vt.version_id = v.id AND vt.language = t.language
ORDER BY p.ord`, s.t.facets, s.t.items, s.t.versions, s.t.codes, s.t.text, s.t.versionText, s.t.creators, listed, edition.Order("g", "@d_display::text")),
		args)
	if err != nil {
		return nil, err
	}
	got, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (cardRow, error) {
		var c cardRow
		var pop []byte
		var code string
		var baseline int64
		var isDefault bool
		err := r.Scan(&c.ID, &c.Kind, &c.contentRule, &c.override, &c.Version.Languages, &c.Version.Traits, &c.groupIDs, &c.counts.likes,
			&c.counts.dislikes, &c.counts.favorites, &pop, &baseline, &c.PublishedAt, &c.Attributes, &c.Version.ID, &c.Version.Code, &c.Version.Label, &c.Version.Pages, &c.Version.DurationMS,
			&c.Version.HasAudio, &c.listing, &code, &c.slug, &c.Title, &c.Language, &c.owner, &c.creator, &isDefault)
		if err != nil {
			return c, err
		}
		var p popularityFacet
		if len(pop) > 0 {
			if err := json.Unmarshal(pop, &p); err != nil {
				return c, err
			}
		}
		c.counts.views = p.Views + baseline
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
		c.Cover, c.Groups = []media.PublicRendition{}, map[string][]Chip{}
		return c, nil
	})
	if err != nil {
		return nil, err
	}
	if err := s.decide(ctx, q, got); err != nil {
		return nil, err
	}
	// Members-only listings, like hide mode, need the exact verdict after an
	// incomplete entitlement keyspace admitted them as SQL candidates.
	got = slices.DeleteFunc(got, func(c cardRow) bool {
		return !c.Access.Allowed && (q.reader.Mode == access.Hide || c.listing == "members")
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
	}
	out := make([]Card, 0, len(got))
	for _, c := range got {
		out = append(out, c.Card)
	}
	return out, nil
}

// decide asks the Gate for every card's version in one call, and the offers
// its paywalls sell in one more. Editors open what they edit (staff, its
// creators, its owner group's editors): owner-group editorship is asked only
// of cards the rule leaves locked, once per owner group.
func (s *Service) decide(ctx context.Context, q *sel, rows []cardRow) error {
	items := make([]access.Item, len(rows))
	for i, c := range rows {
		items[i] = access.Item{Content: c.ID, Version: c.Version.ID, Rule: access.Effective(c.contentRule, c.override).Previewed(s.o.Kinds.Kinds[c.Kind]),
			Groups: c.groupIDs}
	}
	ds, err := s.o.Gate.Decide(ctx, q.reader.Actor, items)
	if err != nil && (ds == nil || !errors.Is(err, access.ErrUnavailable)) {
		return err
	}
	internal, err := s.internalAccessGroups(ctx, items, ds)
	if err != nil {
		return err
	}
	var keys []string
	now := s.o.Now()
	for i, d := range ds {
		a := CardAccess{Allowed: d.Allowed, Unknown: d.Unknown, Preview: d.Preview, FreeAfter: d.FreeAfter, Level: access.Public}
		if r := items[i].Rule; !r.Free(now) {
			a.Level, a.Tiers = r.Level, r.Tiers
		}
		for _, k := range d.Sell {
			if k.Kind() != access.KeyGroup || !internal[k.Group()] {
				a.Sell = append(a.Sell, k.String())
			}
		}
		if d.Requires != nil {
			if d.Requires.Kind() == access.KeyGroup && internal[d.Requires.Group()] {
				a.Sell = nil
			} else {
				a.Requires = d.Requires.String()
			}
		}
		if internal[items[i].Rule.Group] && (a.Level == access.GroupLevel || a.Level == access.GroupPPV) {
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
		keys = append(keys, c.Access.Sell...)
	}
	if s.o.Sales == nil || len(keys) == 0 {
		return nil
	}
	sales, err := s.o.Sales.Sales(ctx, keys)
	if err != nil {
		return err
	}
	for i := range rows {
		a := &rows[i].Access
		for _, k := range a.Sell {
			if sale, ok := sales[k]; ok {
				a.Offers = append(a.Offers, sale)
			}
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

// chips names the cards' groups of the kinds asked, by name.
func (s *Service) chips(ctx context.Context, q *sel, rows []cardRow, kinds []string) error {
	var ids []groups.GroupID
	seen := map[string]bool{}
	for _, c := range rows {
		for _, g := range c.groupIDs {
			if !seen[g] {
				seen[g] = true
				ids = append(ids, groups.GroupID(g))
			}
		}
	}
	if len(ids) == 0 {
		return nil
	}
	cards, err := s.o.Groups.Cards(ctx, ids, groups.CardOptions{Language: q.display})
	if err != nil {
		return err
	}
	byID := make(map[string]groups.GroupCard, len(cards))
	for _, g := range cards {
		shown := slices.Contains(kinds, g.Kind)
		if kinds == nil {
			shown = s.o.Kinds.GroupKinds[g.Kind].Route != ""
		}
		if shown {
			byID[string(g.ID)] = g
		}
	}
	for i := range rows {
		for _, id := range rows[i].groupIDs {
			if g, ok := byID[id]; ok {
				rows[i].Groups[g.Kind] = append(rows[i].Groups[g.Kind], Chip{ID: id, Slug: g.Slug, Name: g.Name, Path: g.Path})
			}
		}
		for _, cs := range rows[i].Groups {
			slices.SortFunc(cs, func(a, b Chip) int { return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)) })
		}
	}
	return nil
}

// owners fills the cards' owner groups, avatars and covers resolved, in one
// call.
func (s *Service) owners(ctx context.Context, q *sel, rows []cardRow) error {
	var ids []groups.GroupID
	for _, c := range rows {
		if c.owner != "" && !slices.Contains(ids, groups.GroupID(c.owner)) {
			ids = append(ids, groups.GroupID(c.owner))
		}
	}
	if len(ids) == 0 {
		return nil
	}
	cards, err := s.o.Groups.Cards(ctx, ids, groups.CardOptions{Language: q.display, Covers: true})
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
	got, err := s.Standing(ctx, actor, refs)
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
	if !s.o.Hub.SignalPlane() {
		return out, nil
	}
	states, err := s.o.Hub.States(ctx, signal.Subject{UserID: actor.AccountID()}, refs)
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
		if !st.LastViewAt.IsZero() {
			t := st.LastViewAt
			v.LastViewAt = &t
		}
	}
	return out, nil
}
