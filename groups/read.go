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
	// Parent keeps the children of one group (sub-series of a series).
	Parent GroupID
	// Scope keeps the groups scoped by one group (characters of a series).
	Scope GroupID
	Slug  string
	// Letter keeps groups whose sort name starts with it (a-z), or with
	// anything else ("#").
	Letter string
	// Language resolves names, sort keys and slugs, scopes counts and
	// prefixes paths. Empty: names in the first configured language, counts
	// over every language, unprefixed paths.
	Language string
	// ContentKind scopes counts to one content kind; empty sums them.
	ContentKind string
	Sort        Sort
	// Cursor is the previous page's NextCursor under the same Sort.
	Cursor string
	// Limit defaults to 50 and is capped at 200.
	Limit int
}

// GroupCard is a listed group: what a directory page or a chip needs.
type GroupCard struct {
	ID           GroupID        `json:"id"`
	Kind         string         `json:"kind"`
	Slug         string         `json:"slug"`
	ScopeID      GroupID        `json:"scope_id,omitempty"`
	ParentID     GroupID        `json:"parent_id,omitempty"`
	Name         string         `json:"name"`
	NameLanguage string         `json:"name_language,omitempty"`
	Code         string         `json:"code,omitempty"`
	Path         string         `json:"path,omitempty"`
	Cover        *Cover         `json:"cover,omitempty"`
	Count        int            `json:"count"`
	Attributes   map[string]any `json:"attributes"`
	CreatedAt    time.Time      `json:"created_at"`
}

// GroupPage is one page of a listing.
type GroupPage struct {
	Groups []GroupCard `json:"groups"`
	// NextCursor pages on; empty on the last page.
	NextCursor string `json:"next_cursor,omitempty"`
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
	c.sql = fmt.Sprintf(`SELECT g.id, g.kind, g.slug, g.scope_id, coalesce(g.parent_id, ''), coalesce(nm.name, ''), coalesce(nm.language, ''),
 coalesce(cd.code, ''), coalesce(cd.slugs->>@language, cd.slug, ''), g.cover, coalesce(cc.n, 0), g.attributes, g.created_at, %[6]s
FROM %[1]s g
LEFT JOIN LATERAL (SELECT nn.name, nn.language, nn.normalized FROM %[2]s nn
  WHERE nn.tenant_id = g.tenant_id AND nn.group_id = g.id AND nn.kind = 'name'
  ORDER BY CASE WHEN nn.language = @language THEN 0 WHEN nn.language = 'en' THEN 1 ELSE 2 + coalesce(array_position(@fallback::text[], nn.language), 1000) END, nn.language
  LIMIT 1) nm ON true
LEFT JOIN %[3]s t ON t.tenant_id = g.tenant_id AND t.group_id = g.id AND t.language = @language
LEFT JOIN %[4]s cd ON cd.tenant_id = g.tenant_id AND cd.content_kind = g.kind AND cd.content_id = g.id
LEFT JOIN LATERAL (SELECT sum(gc.content_count)::int AS n FROM %[5]s gc
  WHERE gc.tenant_id = g.tenant_id AND gc.group_id = g.id AND gc.language = @count_language%[7]s) cc ON true
WHERE %[8]s ORDER BY %[9]s LIMIT @limit`,
		s.table("content_groups"), s.table("content_group_names"), s.table("content_group_text"), s.table("content_codes"),
		s.table("content_group_counts"), sortName, countKind, strings.Join(where, " AND "), order)
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
		var code, slug, name string
		if err := rows.Scan(&g.ID, &g.Kind, &g.Slug, &g.ScopeID, &g.ParentID, &g.Name, &g.NameLanguage, &code, &slug, &g.Cover, &g.Count,
			&g.Attributes, &g.CreatedAt, &name); err != nil {
			return page, err
		}
		if g.Attributes == nil {
			g.Attributes = map[string]any{}
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
	return contenturl.Path(language, route, contenturl.Code(code), slug)
}

// List pages active groups with their display name, code, path, cover and
// count.
func (s *Store) List(ctx context.Context, opts ListOptions) (GroupPage, error) {
	c, err := s.compile(opts, nil)
	if err != nil {
		return GroupPage{Groups: []GroupCard{}}, err
	}
	var page GroupPage
	err = s.read(ctx, func(q Querier) error {
		page, err = s.cards(ctx, q, c)
		return err
	})
	if err != nil {
		return page, err
	}
	slots := make([]coverSlot, len(page.Groups))
	for i := range page.Groups {
		slots[i] = coverSlot{page.Groups[i].Kind, page.Groups[i].ID, page.Groups[i].Cover}
	}
	return page, s.fillCovers(ctx, slots)
}

// CardOptions resolve Cards.
type CardOptions struct {
	// Language resolves names and paths as ListOptions.Language does.
	Language string
	// Covers fills covers' renditions; chips leave them out.
	Covers bool
}

// Cards returns the active groups among ids as cards, resolved as List
// resolves them, in no particular order.
func (s *Store) Cards(ctx context.Context, ids []GroupID, o CardOptions) ([]GroupCard, error) {
	want, err := uniqueIDs(ids)
	if err != nil || len(want) == 0 {
		return []GroupCard{}, err
	}
	c, err := s.compile(ListOptions{Language: o.Language}, want)
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
		slots := make([]coverSlot, len(page.Groups))
		for i := range page.Groups {
			slots[i] = coverSlot{page.Groups[i].Kind, page.Groups[i].ID, page.Groups[i].Cover}
		}
		if err := s.fillCovers(ctx, slots); err != nil {
			return nil, err
		}
	}
	return page.Groups, nil
}

// coverSlot is a cover to fill in place.
type coverSlot struct {
	kind  string
	id    GroupID
	cover *Cover
}

// fillCovers resolves every slot's renditions in one call.
func (s *Store) fillCovers(ctx context.Context, slots []coverSlot) error {
	var asked []CoverOf
	var at []*Cover
	for _, sl := range slots {
		if s.covers != nil && sl.cover != nil {
			asked, at = append(asked, CoverOf{Kind: sl.kind, ID: sl.id, Cover: *sl.cover}), append(at, sl.cover)
		}
	}
	if len(asked) == 0 {
		return nil
	}
	got, err := s.covers.Covers(ctx, asked)
	if err != nil {
		return err
	}
	for i, c := range at {
		c.Renditions = got[i]
	}
	return nil
}

const maxMergeHops = 8

// Detail returns an active group with its names, text, parent, scope,
// children, edges and counts, resolved for language as List does. A merged
// group answers with its survivor; a deleted one is ErrNotFound.
func (s *Store) Detail(ctx context.Context, id GroupID, language string) (GroupDetail, error) {
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
			if !ok || g.State == StateDeleted || hop == maxMergeHops {
				return ErrNotFound
			}
			if g.State == StateMerged {
				id = g.MergedInto
				continue
			}
			d.Group = g
			break
		}
		c, err := s.compile(ListOptions{Language: language}, []string{string(id)})
		if err != nil {
			return err
		}
		self, err := s.cards(ctx, q, c)
		if err != nil || len(self.Groups) != 1 {
			return errors.Join(err, ErrNotFound)
		}
		card := self.Groups[0]
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
			if c, err = s.compile(ListOptions{Language: language}, links); err != nil {
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
		if c, err = s.compile(ListOptions{Parent: id, Language: language, Limit: 200}, nil); err != nil {
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
		counts, err := s.counts(ctx, q, []string{string(id)})
		if d.Counts = counts[id]; d.Counts == nil {
			d.Counts = []Count{}
		}
		return err
	})
	if err != nil {
		return d, err
	}
	slots := []coverSlot{{d.Kind, d.ID, d.Cover}}
	for _, g := range append([]*GroupCard{d.Parent, d.Scope}, pointers(d.Children)...) {
		if g != nil {
			slots = append(slots, coverSlot{g.Kind, g.ID, g.Cover})
		}
	}
	return d, s.fillCovers(ctx, slots)
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
