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
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/contenturl"
	"github.com/open-rails/contentkit/groups"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/versions"
	"github.com/open-rails/contentkit/offers"
	"github.com/open-rails/contentkit/signal"
)

// Card is one content item as a grid shows it, with the version that fits
// the reader best.
type Card struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	// Code and Path are its page: [/{lang}]/{route}/{code}/{slug}.
	Code string `json:"code"`
	Path string `json:"path"`
	// Title is in Language: the display language, else a fallback.
	Title    string                  `json:"title"`
	Language string                  `json:"language"`
	Cover    []media.PublicRendition `json:"cover"`
	Version  CardVersion             `json:"version"`
	// Groups are the version's effective groups with a page, by group kind.
	Groups map[string][]Chip `json:"groups"`
	// Owner is the owner group's card (a channel's name, path, avatar and
	// cover).
	Owner       *groups.GroupCard `json:"owner,omitempty"`
	Counts      Counts            `json:"counts"`
	Access      CardAccess        `json:"access"`
	Viewer      *ViewerState      `json:"viewer,omitempty"`
	PublishedAt *time.Time        `json:"published_at,omitempty"`
	Attributes  map[string]any    `json:"attributes"`
}

// CardVersion is the version a card shows.
type CardVersion struct {
	ID         string   `json:"id"`
	Languages  []string `json:"languages"`
	Traits     []string `json:"traits"`
	Label      string   `json:"label,omitempty"`
	Pages      *int     `json:"pages,omitempty"`
	DurationMS *int64   `json:"duration_ms,omitempty"`
}

// Chip names one group on a card.
type Chip struct {
	ID   string `json:"id"`
	Slug string `json:"slug"`
	Name string `json:"name"`
	Path string `json:"path,omitempty"`
}

// Counts are a content item's interaction counts; Views counts its distinct
// viewers of all time plus its imported legacy view count.
type Counts struct {
	Likes     int   `json:"likes"`
	Dislikes  int   `json:"dislikes"`
	Favorites int   `json:"favorites"`
	Views     int64 `json:"views"`
}

// CardAccess is the reader's access decision on the card's version.
type CardAccess struct {
	Allowed bool `json:"allowed"`
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

// ViewerState is the signed-in reader's own standing with a content item.
type ViewerState struct {
	Favorited bool `json:"favorited"`
	// Seen, Completed and Progress come from the signal plane.
	Seen        bool       `json:"seen"`
	Completed   bool       `json:"completed"`
	Progress    uint32     `json:"progress"`
	ProgressMax uint32     `json:"progress_max"`
	Resume      string     `json:"resume,omitempty"`
	LastViewAt  *time.Time `json:"last_view_at,omitempty"`
}

// ContentPage is one page of cards.
type ContentPage struct {
	Contents []Card `json:"contents"`
	// NextCursor pages on; empty on the last page.
	NextCursor string `json:"next_cursor,omitempty"`
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
	slug        string
}

// cards hydrates picks in order. In hide mode a card the reader does not
// open is dropped.
func (s *Service) cards(ctx context.Context, q *sel, picks []pick, o cardOptions) ([]Card, error) {
	out := []Card{}
	if len(picks) == 0 {
		return out, nil
	}
	vids := make([]string, len(picks))
	for i, p := range picks {
		vids[i] = p.version
	}
	rows, err := s.o.Pool.Query(ctx, fmt.Sprintf(`SELECT c.id, c.kind, c.access, v.access, f.languages, f.traits, f.group_ids, f.likes, f.dislikes, f.favorites,
 f.popularity, f.view_baseline, f.published_at, f.attributes, v.id, v.label, v.pages, v.duration_ms,
 coalesce(cd.code, ''), coalesce(cd.slugs->>@d_display, cd.slug, ''), coalesce(nullif(vt.title, ''), t.title, ''), coalesce(t.language, ''),
 coalesce(f.owner_group_id, ''), @d_user <> '' AND EXISTS (SELECT 1 FROM %[7]s cc WHERE cc.tenant_id = c.tenant_id AND cc.content_id = c.id AND cc.user_id = @d_user)
FROM unnest(@d_vids::text[]) WITH ORDINALITY AS p(vid, ord)
JOIN %[1]s f ON f.tenant_id = @d_tenant AND f.version_id = p.vid
JOIN %[2]s c ON c.tenant_id = f.tenant_id AND c.id = f.content_id
JOIN %[3]s v ON v.tenant_id = f.tenant_id AND v.id = f.version_id
LEFT JOIN %[4]s cd ON cd.tenant_id = c.tenant_id AND cd.content_kind = c.kind AND cd.content_id = c.id
LEFT JOIN LATERAL (SELECT x.title, x.language FROM %[5]s x WHERE x.tenant_id = c.tenant_id AND x.content_id = c.id
 ORDER BY x.language = @d_display DESC, array_position(@d_langs::text[], x.language), x.language LIMIT 1) t ON true
LEFT JOIN %[6]s vt ON vt.tenant_id = v.tenant_id AND vt.version_id = v.id AND vt.language = t.language
ORDER BY p.ord`, s.t.facets, s.t.items, s.t.versions, s.t.codes, s.t.text, s.t.versionText, s.t.creators),
		pgx.NamedArgs{"d_vids": vids, "d_tenant": s.o.Tenant, "d_display": q.display, "d_langs": s.languages(), "d_user": person(q.reader.Actor)})
	if err != nil {
		return nil, err
	}
	got, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (cardRow, error) {
		var c cardRow
		var pop []byte
		var code string
		var baseline int64
		err := r.Scan(&c.ID, &c.Kind, &c.contentRule, &c.override, &c.Version.Languages, &c.Version.Traits, &c.groupIDs, &c.Counts.Likes,
			&c.Counts.Dislikes, &c.Counts.Favorites, &pop, &baseline, &c.PublishedAt, &c.Attributes, &c.Version.ID, &c.Version.Label, &c.Version.Pages, &c.Version.DurationMS,
			&code, &c.slug, &c.Title, &c.Language, &c.owner, &c.creator)
		if err != nil {
			return c, err
		}
		var p popularityFacet
		if len(pop) > 0 {
			if err := json.Unmarshal(pop, &p); err != nil {
				return c, err
			}
		}
		c.Counts.Views = p.Views + baseline
		c.Code = code
		if route := s.o.Kinds.Kinds[c.Kind].Route; route != "" && code != "" {
			c.Path = contenturl.Path(contenturl.Prefix(s.o.Kinds.Languages, q.path), route, contenturl.Code(code), c.slug)
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
	if q.reader.Mode == access.Hide {
		got = slices.DeleteFunc(got, func(c cardRow) bool { return !c.Access.Allowed })
	}
	if err := s.covers(ctx, got); err != nil {
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
		items[i] = access.Item{Content: c.ID, Version: c.Version.ID, Rule: access.Effective(c.contentRule, c.override), Groups: c.groupIDs}
	}
	ds, err := s.o.Gate.Decide(ctx, q.reader.Actor, items)
	if err != nil && (ds == nil || !errors.Is(err, access.ErrUnavailable)) {
		return err
	}
	var keys []string
	for i, d := range ds {
		a := CardAccess{Allowed: d.Allowed, Unknown: d.Unknown, Preview: d.Preview, FreeAfter: d.FreeAfter}
		for _, k := range d.Sell {
			a.Sell = append(a.Sell, k.String())
		}
		if d.Requires != nil {
			a.Requires = d.Requires.String()
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
			c.Access = CardAccess{Allowed: true}
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

func (s *Service) covers(ctx context.Context, rows []cardRow) error {
	if s.o.Covers == nil || len(rows) == 0 {
		return nil
	}
	asked := make([]versions.CoverOf, len(rows))
	for i, c := range rows {
		asked[i] = versions.CoverOf{Kind: c.Kind, ID: c.ID}
	}
	got, err := s.o.Covers.Covers(ctx, asked)
	if err != nil {
		return err
	}
	for i := range rows {
		if got[i] != nil {
			rows[i].Cover = got[i]
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
	cards, err := s.o.Groups.Cards(ctx, ids, groups.CardOptions{Language: q.path})
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
	cards, err := s.o.Groups.Cards(ctx, ids, groups.CardOptions{Language: q.path, Covers: true})
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
	if a.Person() {
		return a.ID
	}
	return ""
}

// viewerState fills a signed-in reader's favorites and, with a signal plane,
// their progress. A failed signal read leaves progress out.
func (s *Service) viewerState(ctx context.Context, actor access.Actor, rows []cardRow) error {
	if !actor.Person() || len(rows) == 0 {
		return nil
	}
	kinds, ids := make([]string, len(rows)), make([]string, len(rows))
	refs := make([]contentref.ContentRef, len(rows))
	for i, c := range rows {
		kinds[i], ids[i] = c.Kind, c.ID
		refs[i] = contentref.New(s.o.Tenant, c.Kind, c.ID)
		rows[i].Viewer = &ViewerState{}
	}
	fav, err := s.o.Pool.Query(ctx, `SELECT f.content_id FROM `+s.t.favorites+` f JOIN unnest($3::text[], $4::text[]) AS r(kind, id)
 ON f.content_kind = r.kind AND f.content_id = r.id WHERE f.tenant_id = $1 AND f.user_id = $2 AND f.content_version_id IS NULL AND f.value = 1`,
		s.o.Tenant, actor.ID, kinds, ids)
	if err != nil {
		return err
	}
	favorited, err := pgx.CollectRows(fav, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for i := range rows {
		rows[i].Viewer.Favorited = slices.Contains(favorited, rows[i].ID)
	}
	if s.recs == nil {
		return nil
	}
	states, err := s.o.Hub.States(ctx, signal.Subject{UserID: actor.ID}, refs)
	if err != nil {
		s.o.Logger.WarnContext(ctx, "discovery: viewer progress unavailable", "err", err)
		return nil
	}
	for i, ref := range refs {
		st, ok := states[ref.Key()]
		if !ok {
			continue
		}
		v := rows[i].Viewer
		v.Seen, v.Completed, v.Progress, v.ProgressMax, v.Resume = st.Seen, st.Completed, st.MaxProgress, st.ProgressMax, st.Resume
		if !st.LastViewAt.IsZero() {
			t := st.LastViewAt
			v.LastViewAt = &t
		}
	}
	return nil
}
