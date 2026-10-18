package groups

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/contenturl"
	"github.com/open-rails/contentkit/offers"
)

// Sort orders a group listing; every order is made total by id.
type Sort string

const (
	// SortName orders by sort key, else display name. The default.
	SortName Sort = "name"
	// SortCount orders by content count, largest first.
	SortCount Sort = "count"
	// SortNewest orders by creation, newest first.
	SortNewest Sort = "newest"
)

// ListOptions filters, orders and pages List. Only active groups are listed.
type ListOptions struct {
	Kind string
	// Query matches a name, alias or slug for management selectors.
	Query string
	// IncludeInternal is a trusted management read; As stores require staff.
	IncludeInternal bool
	// Deleted lists deleted groups instead of active ones; with IncludeInternal only.
	Deleted bool
	// Parent keeps the children of one group (sub-series of a series).
	Parent GroupID
	// Scope keeps the groups scoped by one group (characters of a series).
	Scope GroupID
	Slug  string
	// Letter keeps groups whose sort name starts with it (a-z), or with
	// anything else ("#").
	Letter string
	// Language resolves names, sort keys and slugs, scopes counts and
	// prefixes paths. Empty: names and paths in the first configured
	// language, counts over every language.
	Language string
	// ContentKind scopes counts to one content kind; empty sums them.
	ContentKind string
	Sort        Sort
	// Cursor is the previous page's NextCursor under the same Sort.
	Cursor string
	// Page numbers pages from 1 instead of Cursor; the page then carries Total.
	Page int
	// Limit defaults to 50 and is capped at 200.
	Limit int
}

// maxPageOffset bounds numbered pages; deeper reading pages by cursor.
const maxPageOffset = 10_000

// GroupCard is a listed group: what a directory page or a chip needs.
type GroupCard struct {
	ID           GroupID `json:"id"`
	Kind         string  `json:"kind"`
	Slug         string  `json:"slug"`
	Internal     bool    `json:"internal"`
	ScopeID      GroupID `json:"scope_id,omitempty"`
	ParentID     GroupID `json:"parent_id,omitempty"`
	Name         string  `json:"name"`
	NameLanguage string  `json:"name_language,omitempty"`
	Code         string  `json:"code,omitempty"`
	Path         string  `json:"path,omitempty"`
	Cover        *Cover  `json:"cover,omitempty"`
	// Avatar is a user-owned group's avatar.
	Avatar     *Avatar        `json:"avatar,omitempty"`
	Count      int            `json:"count"`
	Attributes map[string]any `json:"attributes"`
	// Access is a sellable group's sale: its key, active offer and the
	// viewer's standing.
	Access    *offers.GroupAccess `json:"access,omitempty"`
	CreatedAt time.Time           `json:"created_at"`
}

// GroupPage is one page of a listing.
type GroupPage struct {
	Groups []GroupCard `json:"entities"`
	// NextCursor pages on; empty on the last page.
	NextCursor string `json:"next_cursor,omitempty"`
	// Total is every matching group, on numbered pages only.
	Total *int `json:"total,omitempty"`
}

// GroupDetail is one group with everything its page shows.
type GroupDetail struct {
	Group
	Name         string      `json:"name"`
	NameLanguage string      `json:"name_language,omitempty"`
	Code         string      `json:"code,omitempty"`
	Path         string      `json:"path,omitempty"`
	Names        []Name      `json:"names"`
	Text         []Text      `json:"text"`
	Parent       *GroupCard  `json:"parent,omitempty"`
	Scope        *GroupCard  `json:"scope,omitempty"`
	Children     []GroupCard `json:"children"`
	Edges        []Edge      `json:"edges"`
	Counts       []Count     `json:"counts"`
	// Access is a sellable group's sale: its key, active offer and the
	// viewer's standing.
	Access *offers.GroupAccess `json:"access,omitempty"`
	// Offer is a sellable group's current offer, for its offer editors.
	Offer *offers.Offer `json:"offer,omitempty"`
}

type listCursor struct {
	Name  string    `json:"s,omitempty"`
	Count int       `json:"n,omitempty"`
	Time  time.Time `json:"t,omitzero"`
	ID    string    `json:"id"`
}

var letterRE = regexp.MustCompile(`^([a-z]|#)$`)

// cardQuery is a compiled listing.
type cardQuery struct {
	sql      string
	count    string
	args     pgx.NamedArgs
	sort     Sort
	limit    int
	pathLang string
}

const sortName = `coalesce(nullif(lower(t.sort_key), ''), nm.normalized, '')`

func (s *Store) compile(opts ListOptions, ids []string) (cardQuery, error) {
	c := cardQuery{args: pgx.NamedArgs{"tenant": s.tenant, "fallback": s.cfg.Languages}, sort: opts.Sort, limit: opts.Limit}
	if c.sort == "" {
		c.sort = SortName
	}
	order := map[Sort]string{SortName: sortName + ", g.id", SortCount: "coalesce(cc.n, 0) DESC, g.id", SortNewest: "g.created_at DESC, g.id DESC"}[c.sort]
	if order == "" {
		return c, fmt.Errorf("%w: sort %q", ErrInvalid, opts.Sort)
	}
	if c.limit <= 0 {
		c.limit = 50
	}
	c.limit = min(c.limit, 200)
	c.args["offset"] = 0
	switch {
	case opts.Page < 0:
		return c, fmt.Errorf("%w: page", ErrInvalid)
	case opts.Page > 0 && opts.Cursor != "":
		return c, fmt.Errorf("%w: page and cursor are exclusive", ErrInvalid)
	case opts.Page > maxPageOffset/c.limit: // Page*limit could overflow
		return c, fmt.Errorf("%w: page × limit is at most %d", ErrInvalid, maxPageOffset)
	case opts.Page > 0:
		c.args["offset"] = (opts.Page - 1) * c.limit
	}
	lang := s.cfg.Languages[0]
	if l := strings.TrimSpace(opts.Language); l != "" {
		var err error
		if lang, err = normalizeLanguage(l); err != nil {
			return c, err
		}
		c.pathLang = lang
	}
	c.args["language"], c.args["count_language"] = lang, c.pathLang
	where := []string{"g.tenant_id = @tenant", "g.state = 'active'"}
	if opts.Deleted {
		if !opts.IncludeInternal {
			return c, fmt.Errorf("%w: deleted groups are listed for management only", ErrForbidden)
		}
		where[1] = "g.state = 'deleted'"
	}
	if !opts.IncludeInternal {
		where = append(where, "NOT g.internal")
	}
	if query := strings.TrimSpace(opts.Query); query != "" {
		c.args["query"] = query
		where = append(where, fmt.Sprintf(`(strpos(%[1]s.contentkit_keyword_normalize(g.slug), %[1]s.contentkit_keyword_normalize(@query)) > 0
 OR EXISTS (SELECT 1 FROM %[2]s query_name WHERE query_name.tenant_id = g.tenant_id AND query_name.group_id = g.id
 AND strpos(query_name.normalized, %[1]s.contentkit_keyword_normalize(@query)) > 0))`, s.qs, s.table("content_group_names")))
	}
	if ids != nil {
		where, c.args["ids"] = append(where, "g.id = ANY(@ids::text[])"), ids
	}
	if k := strings.TrimSpace(opts.Kind); k != "" {
		if _, err := s.groupKind(k); err != nil {
			return c, err
		}
		where, c.args["kind"] = append(where, "g.kind = @kind"), k
	}
	for col, id := range map[string]GroupID{"parent_id": opts.Parent, "scope_id": opts.Scope} {
		if id == "" {
			continue
		}
		if err := validateID(id); err != nil {
			return c, err
		}
		where, c.args[col] = append(where, fmt.Sprintf("g.%s = @%s", col, col)), string(id)
	}
	if slug := strings.TrimSpace(opts.Slug); slug != "" {
		where, c.args["slug"] = append(where, "g.slug = @slug"), slug
	}
	switch l := strings.ToLower(strings.TrimSpace(opts.Letter)); {
	case l == "":
	case !letterRE.MatchString(l):
		return c, fmt.Errorf("%w: letter %q is one of a-z or #", ErrInvalid, opts.Letter)
	case l == "#":
		where = append(where, sortName+" !~ '^[a-z]'")
	default:
		where, c.args["letter"] = append(where, "left("+sortName+", 1) = @letter"), l
	}
	countKind := ""
	if ck := strings.TrimSpace(opts.ContentKind); ck != "" {
		countKind, c.args["content_kind"] = " AND gc.content_kind = @content_kind", ck
	}
	if opts.Cursor != "" {
		var cur listCursor
		b, err := base64.RawURLEncoding.DecodeString(opts.Cursor)
		if err == nil {
			err = json.Unmarshal(b, &cur)
		}
		if err != nil || cur.ID == "" {
			return c, fmt.Errorf("%w: cursor", ErrInvalid)
		}
		c.args["cid"] = cur.ID
		switch c.sort {
		case SortName:
			where, c.args["cs"] = append(where, "("+sortName+", g.id) > (@cs, @cid)"), cur.Name
		case SortCount:
			where, c.args["cn"] = append(where, "(coalesce(cc.n, 0) < @cn OR (coalesce(cc.n, 0) = @cn AND g.id > @cid))"), cur.Count
		case SortNewest:
			where, c.args["ct"] = append(where, "(g.created_at, g.id) < (@ct, @cid)"), cur.Time
		}
	}
	c.args["limit"] = c.limit + 1
	scope, parent := "g.scope_id", "coalesce(g.parent_id, '')"
	if !opts.IncludeInternal {
		visibleLink := func(column string) string {
			return fmt.Sprintf("CASE WHEN EXISTS (SELECT 1 FROM %s link WHERE link.tenant_id=g.tenant_id AND link.id=g.%s AND NOT link.internal) THEN g.%s ELSE '' END", s.table("content_groups"), column, column)
		}
		scope, parent = visibleLink("scope_id"), visibleLink("parent_id")
	}
	base := fmt.Sprintf(`SELECT g.id, g.kind, g.slug, %[9]s, %[10]s, coalesce(nm.name, ''), coalesce(nm.language, ''),
 coalesce(cd.code, ''), coalesce(cd.slugs->>@language, cd.slug, ''), g.cover, coalesce(g.avatar, ''), coalesce(cc.n, 0), g.attributes, g.created_at, %[6]s, g.internal
FROM %[1]s g
LEFT JOIN LATERAL (SELECT nn.name, nn.language, nn.normalized FROM %[2]s nn
  WHERE nn.tenant_id = g.tenant_id AND nn.group_id = g.id AND nn.kind = 'name'
  ORDER BY CASE WHEN nn.language = @language THEN 0 WHEN nn.language = 'en' THEN 1 ELSE 2 + coalesce(array_position(@fallback::text[], nn.language), 1000) END, nn.language
  LIMIT 1) nm ON true
LEFT JOIN %[3]s t ON t.tenant_id = g.tenant_id AND t.group_id = g.id AND t.language = @language
LEFT JOIN %[4]s cd ON cd.tenant_id = g.tenant_id AND cd.content_kind = g.kind AND cd.content_id = g.id
LEFT JOIN LATERAL (SELECT sum(gc.content_count)::int AS n FROM %[5]s gc
  WHERE gc.tenant_id = g.tenant_id AND gc.group_id = g.id AND gc.language = @count_language%[7]s) cc ON true
WHERE %[8]s`,
		s.table("content_groups"), s.table("content_group_names"), s.table("content_group_text"), s.table("content_codes"),
		s.table("content_group_counts"), sortName, countKind, strings.Join(where, " AND "), scope, parent)
	c.sql = base + " ORDER BY " + order + " LIMIT @limit OFFSET @offset"
	if opts.Page > 0 {
		c.count = "SELECT count(*) FROM (" + base + ") counted"
	}
	return c, nil
}

func (s *Store) cards(ctx context.Context, q Querier, c cardQuery) (GroupPage, error) {
	page := GroupPage{Groups: []GroupCard{}}
	rows, err := q.Query(ctx, c.sql, c.args)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var g GroupCard
		var code, slug, avatar, name string
		if err := rows.Scan(&g.ID, &g.Kind, &g.Slug, &g.ScopeID, &g.ParentID, &g.Name, &g.NameLanguage, &code, &slug, &g.Cover, &avatar, &g.Count,
			&g.Attributes, &g.CreatedAt, &name, &g.Internal); err != nil {
			return page, err
		}
		if g.Attributes == nil {
			g.Attributes = map[string]any{}
		}
		if avatar != "" {
			g.Avatar = &Avatar{Image: avatar}
		}
		g.Code, g.Path = code, s.path(g.Kind, code, slug, c.pathLang)
		page.Groups, names = append(page.Groups, g), append(names, name)
	}
	if err := rows.Err(); err != nil {
		return page, err
	}
	if len(page.Groups) > c.limit {
		page.Groups = page.Groups[:c.limit]
		last := page.Groups[c.limit-1]
		cur := listCursor{ID: string(last.ID)}
		switch c.sort {
		case SortName:
			cur.Name = names[c.limit-1]
		case SortCount:
			cur.Count = last.Count
		case SortNewest:
			cur.Time = last.CreatedAt
		}
		b, _ := json.Marshal(cur)
		page.NextCursor = base64.RawURLEncoding.EncodeToString(b)
	}
	return page, nil
}

// path is the group page's canonical path, "" when its kind has no route.
func (s *Store) path(kind, code, slug, language string) string {
	route := s.cfg.GroupKinds[kind].Route
	if route == "" || code == "" {
		return ""
	}
	return contenturl.Path(s.cfg.Languages, language, route, contenturl.Code(code), slug)
}

// List pages active groups with their display name, code, path, cover,
// avatar, count and, for a sellable kind, its sale to the store's viewer
// (As).
func (s *Store) List(ctx context.Context, opts ListOptions) (GroupPage, error) {
	if err := s.requireInternalRead(ctx, opts.IncludeInternal); err != nil {
		return GroupPage{}, err
	}
	c, err := s.compile(opts, nil)
	if err != nil {
		return GroupPage{Groups: []GroupCard{}}, err
	}
	var page GroupPage
	err = s.read(ctx, func(q Querier) error {
		if page, err = s.cards(ctx, q, c); err != nil || c.count == "" {
			return err
		}
		var total int
		if err := q.QueryRow(ctx, c.count, c.args).Scan(&total); err != nil {
			return err
		}
		page.Total = &total
		return nil
	})
	if err != nil {
		return page, err
	}
	shows := make([]shown, len(page.Groups))
	for i := range page.Groups {
		shows[i] = cardShown(&page.Groups[i])
	}
	return page, s.fill(ctx, shows, opts.Language)
}

// CardOptions resolve Cards.
type CardOptions struct {
	// IncludeInternal is a trusted management read; As stores require staff.
	IncludeInternal bool
	// Language resolves names and paths as ListOptions.Language does.
	Language string
	// Covers fills covers' and avatars' renditions in one call; chips leave
	// them out.
	Covers bool
}

// Cards returns the active groups among ids as cards, resolved as List
// resolves them, in no particular order.
func (s *Store) Cards(ctx context.Context, ids []GroupID, o CardOptions) ([]GroupCard, error) {
	if err := s.requireInternalRead(ctx, o.IncludeInternal); err != nil {
		return nil, err
	}
	want, err := uniqueIDs(ids)
	if err != nil || len(want) == 0 {
		return []GroupCard{}, err
	}
	c, err := s.compile(ListOptions{Language: o.Language, IncludeInternal: o.IncludeInternal}, want)
	if err != nil {
		return nil, err
	}
	c.limit, c.args["limit"] = len(want), len(want)+1
	var page GroupPage
	if err := s.read(ctx, func(q Querier) error {
		page, err = s.cards(ctx, q, c)
		return err
	}); err != nil {
		return nil, err
	}
	if o.Covers {
		shows := make([]shown, len(page.Groups))
		for i := range page.Groups {
			g := &page.Groups[i]
			shows[i] = shown{kind: g.Kind, id: g.ID, cover: g.Cover, avatar: &g.Avatar}
		}
		if err := s.fill(ctx, shows, o.Language); err != nil {
			return nil, err
		}
	}
	return page.Groups, nil
}

const maxMergeHops = 8

// Detail returns an active group with its names, text, parent, scope,
// children, edges, counts and sale, resolved for language as List does. A merged
// group answers with its survivor; a deleted one is ErrNotFound.
func (s *Store) Detail(ctx context.Context, id GroupID, language string) (GroupDetail, error) {
	return s.detail(ctx, id, language, false)
}

// ManagementDetail includes internal classifications and requires staff on an
// As-bound store. Public Detail remains public even for an administrator.
func (s *Store) ManagementDetail(ctx context.Context, id GroupID, language string) (GroupDetail, error) {
	if err := s.requireInternalRead(ctx, true); err != nil {
		return GroupDetail{}, err
	}
	return s.detail(ctx, id, language, true)
}

func (s *Store) requireInternalRead(ctx context.Context, include bool) error {
	if !include {
		return nil
	}
	staff, err := s.staff(ctx)
	if err != nil {
		return err
	}
	if !staff {
		return ErrForbidden
	}
	return nil
}

func (s *Store) detail(ctx context.Context, id GroupID, language string, includeInternal bool) (GroupDetail, error) {
	var d GroupDetail
	if err := validateID(id); err != nil {
		return d, err
	}
	err := s.read(ctx, func(q Querier) error {
		for hop := 0; ; hop++ {
			found, err := s.byIDs(ctx, q, []string{string(id)})
			if err != nil {
				return err
			}
			g, ok := found[id]
			if !ok || g.State == StateDeleted || (!includeInternal && g.Internal) || hop == maxMergeHops {
				return ErrNotFound
			}
			if g.State == StateMerged {
				id = g.MergedInto
				continue
			}
			d.Group = g
			break
		}
		c, err := s.compile(ListOptions{Language: language, IncludeInternal: includeInternal}, []string{string(id)})
		if err != nil {
			return err
		}
		self, err := s.cards(ctx, q, c)
		if err != nil || len(self.Groups) != 1 {
			return errors.Join(err, ErrNotFound)
		}
		card := self.Groups[0]
		d.ParentID, d.ScopeID = card.ParentID, card.ScopeID
		d.Name, d.NameLanguage, d.Code, d.Path = card.Name, card.NameLanguage, card.Code, card.Path
		names, err := s.names(ctx, q, []string{string(id)})
		if err != nil {
			return err
		}
		if d.Names = names[id]; d.Names == nil {
			d.Names = []Name{}
		}
		if d.Text, err = s.text(ctx, q, string(id)); err != nil {
			return err
		}
		var links []string
		for _, l := range []GroupID{d.ParentID, d.ScopeID} {
			if l != "" {
				links = append(links, string(l))
			}
		}
		if len(links) > 0 {
			if c, err = s.compile(ListOptions{Language: language, IncludeInternal: includeInternal}, links); err != nil {
				return err
			}
			linked, err := s.cards(ctx, q, c)
			if err != nil {
				return err
			}
			for i := range linked.Groups {
				switch g := &linked.Groups[i]; g.ID {
				case d.ParentID:
					d.Parent = g
				case d.ScopeID:
					d.Scope = g
				}
			}
		}
		if c, err = s.compile(ListOptions{Parent: id, Language: language, Limit: 200, IncludeInternal: includeInternal}, nil); err != nil {
			return err
		}
		children, err := s.cards(ctx, q, c)
		if err != nil {
			return err
		}
		d.Children = children.Groups
		if d.Edges, err = s.edges(ctx, q, []string{string(id)}); err != nil {
			return err
		}
		if !includeInternal {
			links := make([]string, 0, len(d.Edges)*2)
			for _, edge := range d.Edges {
				links = append(links, string(edge.From), string(edge.To))
			}
			groups, err := s.byIDs(ctx, q, links)
			if err != nil {
				return err
			}
			edges := d.Edges[:0]
			for _, edge := range d.Edges {
				if !groups[edge.From].Internal && !groups[edge.To].Internal {
					edges = append(edges, edge)
				}
			}
			d.Edges = edges
		}
		counts, err := s.counts(ctx, q, []string{string(id)})
		if d.Counts = counts[id]; d.Counts == nil {
			d.Counts = []Count{}
		}
		return err
	})
	if err != nil {
		return d, err
	}
	self := shown{kind: d.Kind, id: d.ID, cover: d.Cover, avatar: &d.Avatar}
	if !d.Internal {
		self.access = &d.Access
	}
	shows := []shown{self}
	for _, g := range append([]*GroupCard{d.Parent, d.Scope}, pointers(d.Children)...) {
		if g != nil {
			shows = append(shows, cardShown(g))
		}
	}
	if err := s.fill(ctx, shows, language); err != nil {
		return d, err
	}
	d.Offer = s.currentOffer(ctx, d.Group)
	return d, nil
}

func pointers(cards []GroupCard) []*GroupCard {
	out := make([]*GroupCard, len(cards))
	for i := range cards {
		out[i] = &cards[i]
	}
	return out
}

func (s *Store) text(ctx context.Context, q Querier, id string) ([]Text, error) {
	rows, err := q.Query(ctx, fmt.Sprintf(`SELECT language, description, sort_key FROM %s WHERE tenant_id = $1 AND group_id = $2 ORDER BY language`,
		s.table("content_group_text")), s.tenant, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Text{}
	for rows.Next() {
		var t Text
		if err := rows.Scan(&t.Language, &t.Description, &t.SortKey); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
