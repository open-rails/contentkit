package taxonomy

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

// NodeInput creates one node with its initial names.
type NodeInput struct {
	// TaxonomyID is optional; omitted ids are generated.
	TaxonomyID     TaxonomyID `json:"taxonomy_id,omitempty"`
	Kind           string     `json:"kind"`
	Slug           string     `json:"slug"`
	Names          []Name     `json:"names,omitempty"`
	SourceRevision int64      `json:"source_revision"`
}

// NodeUpdate changes a node's slug or state; nil fields are unchanged.
type NodeUpdate struct {
	Slug           *string `json:"slug,omitempty"`
	State          *State  `json:"state,omitempty"`
	SourceRevision *int64  `json:"source_revision,omitempty"`
}

// NodeDetail is a node with its names, edges in both directions and counts.
type NodeDetail struct {
	Node
	Names  []Name  `json:"names"`
	Edges  []Edge  `json:"edges"`
	Counts []Count `json:"counts"`
}

// Sort orders a node page. Every order is made total by taxonomy_id, so a page
// boundary never repeats or drops a row.
type Sort string

const (
	// SortID orders by taxonomy_id ascending. The default, and the only order
	// ListOptions.Cursor can page.
	SortID Sort = ""
	// SortName orders by the resolved display name; nameless nodes come last.
	SortName Sort = "name"
	// SortCount orders by content count, largest first.
	SortCount Sort = "count"
	// SortCreated orders by creation time, newest first.
	SortCreated Sort = "created"
	// SortOldest orders by creation time, oldest first.
	SortOldest Sort = "oldest"
	// SortUpdated orders by last change, most recent first.
	SortUpdated Sort = "updated"
)

var sorts = map[Sort]string{SortID: "", SortName: "nm.normalized ASC NULLS LAST", SortCount: "coalesce(cc.total, 0) DESC", SortCreated: "n.created_at DESC", SortOldest: "n.created_at ASC", SortUpdated: "n.updated_at DESC"}

// LanguageMode decides what a node without a canonical name in the request
// language gets.
type LanguageMode string

const (
	// LanguageFallback takes the store's configured Languages in order, then
	// any language the node has. The default.
	LanguageFallback LanguageMode = ""
	// LanguageStrict lists the node with an empty Name.
	LanguageStrict LanguageMode = "strict"
	// LanguageRequired drops the node from the page.
	LanguageRequired LanguageMode = "required"
)

// ListOptions filters, orders and pages ListNodes. The zero value lists the
// tenant's active nodes by taxonomy_id, which is what a full admin sync wants;
// a catalog index page sets Language, Sort, ContentKind and Offset.
type ListOptions struct {
	// Kind selects one registered node kind; empty lists every kind.
	Kind string
	// States lists nodes in any of these states; empty means active only.
	States []State
	// IDs selects exactly these nodes; empty does not filter.
	IDs []TaxonomyID
	// Slug selects one slug exactly.
	Slug string
	// Related keeps nodes with an edge pointing at this node; with Relation,
	// only that relation. The characters of a series are
	// {Related: seriesID, Relation: RelationMemberOf}.
	Related TaxonomyID
	// Relation narrows Related; empty accepts any relation.
	Relation Relation

	// Language resolves NodeRow.Name and scopes the counts Count, MinCount and
	// SortCount read. Empty uses the store's first configured language.
	Language string
	// LanguageMode decides the display name when the node has none in Language.
	LanguageMode LanguageMode
	// NamePrefix keeps nodes whose display name starts with it: the A-Z index.
	// Matched normalized, so "e" also matches "Étude".
	NamePrefix string
	// Query keeps nodes with a name or alias containing it, in any language.
	// Matched normalized; % and _ are literal.
	Query string
	// ContentKind scopes the count to one host content kind; empty sums them.
	ContentKind string
	// MinCount keeps nodes whose Count is at least this. 1 hides empty nodes.
	MinCount int

	// FilterSQL is trusted host SQL appended as AND (<FilterSQL>) against
	// node alias n. Use it for host-owned sidecar policy, never request SQL.
	// FilterArgs binds pgx @name placeholders. Library parameter names are
	// reserved even when their corresponding option is unset.
	FilterSQL  string
	FilterArgs map[string]any

	// OrderSQL is a trusted host ORDER BY expression for host-owned directory
	// ordering, such as phonetic names stored in a sidecar. It excludes Sort
	// and Cursor; use Offset. Node alias n is available, FilterArgs binds values,
	// and taxonomy_id is appended as a stable tiebreak. Never accept request SQL.
	OrderSQL string

	// Sort orders the page.
	Sort Sort

	// Cursor is the previous page's NextCursor. Keyset paging: it requires the
	// default Sort, excludes Offset, and skips the total — a full sync pages in
	// constant time instead of paying for a count per page.
	Cursor string
	// Offset pages from the start of the ordered result.
	Offset int
	// Limit defaults to 50 and is capped at 500.
	Limit int
}

// NodeRow is a listed node with its display name and content count.
type NodeRow struct {
	Node
	// Name is the canonical name resolved for ListOptions.Language, empty when
	// the node has none (see LanguageMode).
	Name string `json:"name,omitempty"`
	// NameLanguage is the language Name came from.
	NameLanguage string `json:"name_language,omitempty"`
	// Count is the number of distinct works of ContentKind with an eligible
	// document in Language whose effective assignments include the node.
	Count int `json:"count"`
}

// NodePage is one page of nodes.
type NodePage struct {
	Nodes []NodeRow `json:"nodes"`
	// NextCursor pages the default Sort; empty on the last page and under any
	// other order.
	NextCursor string `json:"next_cursor,omitempty"`
	// Total is the unpaged match count, zero on a Cursor page.
	Total int `json:"total"`
}

type nodeRow struct {
	Ordinal        int    `json:"ordinal"`
	TaxonomyID     string `json:"taxonomy_id"`
	Kind           string `json:"kind"`
	Slug           string `json:"slug"`
	SourceRevision int64  `json:"source_revision"`
}

type nameRow struct {
	Ordinal        int    `json:"ordinal"`
	TaxonomyID     string `json:"taxonomy_id"`
	Language       string `json:"language"`
	Kind           string `json:"kind"`
	Name           string `json:"name"`
	SourceRevision int64  `json:"source_revision"`
}

const nodeColumns = `taxonomy_id, tenant_id, kind, slug, state, source_revision, created_at, updated_at`

// nodeColumnsN is nodeColumns qualified for ListNodes' joined query.
const nodeColumnsN = `n.taxonomy_id, n.tenant_id, n.kind, n.slug, n.state, n.source_revision, n.created_at, n.updated_at`

func scanNodeRow(row rowScanner) (NodeRow, error) {
	var r NodeRow
	err := row.Scan(&r.TaxonomyID, &r.TenantID, &r.Kind, &r.Slug, &r.State, &r.SourceRevision, &r.CreatedAt, &r.UpdatedAt, &r.Name, &r.NameLanguage, &r.Count)
	return r, err
}

func scanNode(row rowScanner) (Node, error) {
	var n Node
	err := row.Scan(&n.TaxonomyID, &n.TenantID, &n.Kind, &n.Slug, &n.State, &n.SourceRevision, &n.CreatedAt, &n.UpdatedAt)
	if err == pgx.ErrNoRows {
		return n, ErrNotFound
	}
	return n, err
}

func (s *Store) validateNames(id TaxonomyID, names []Name) ([]nameRow, error) {
	rows := make([]nameRow, 0, len(names))
	canonical := map[string]string{}
	for _, n := range names {
		lang, err := normalizeLanguage(n.Language)
		if err != nil {
			return nil, err
		}
		name := strings.TrimSpace(n.Name)
		if name == "" || utf8.RuneCountInString(name) > 512 {
			return nil, fmt.Errorf("%w: name %q must be 1-512 characters", ErrInvalid, name)
		}
		kind := n.Kind
		if kind == "" {
			kind = NameAlias
		}
		if kind != NameCanonical && kind != NameAlias {
			return nil, fmt.Errorf("%w: name kind %q", ErrInvalid, kind)
		}
		if kind == NameCanonical {
			if prev, dup := canonical[lang]; dup && prev != name {
				return nil, fmt.Errorf("%w: two canonical names for %s: %q and %q", ErrInvalid, lang, prev, name)
			}
			canonical[lang] = name
		}
		rows = append(rows, nameRow{Ordinal: len(rows), TaxonomyID: string(id), Language: lang, Kind: string(kind), Name: name, SourceRevision: n.SourceRevision})
	}
	return rows, nil
}

// insertNames upserts names. A new canonical name demotes the previous one of
// that language to an alias, so renames keep the old name searchable.
func (s *Store) insertNames(ctx context.Context, q querier, rows []nameRow) error {
	if len(rows) == 0 {
		return nil
	}
	data, err := json.Marshal(rows)
	if err != nil {
		return err
	}
	const recordset = `jsonb_to_recordset($2::jsonb) AS r(ordinal int, taxonomy_id text, language text, kind text, name text, source_revision bigint)`
	if _, err := q.Exec(ctx, fmt.Sprintf(`UPDATE %s n SET kind='alias' FROM %s
 WHERE r.kind='name' AND n.tenant_id=$1 AND n.taxonomy_id=r.taxonomy_id AND n.language=r.language AND n.kind='name'
   AND n.normalized <> %s.contentkit_keyword_normalize(r.name)`, s.table("content_node_names"), recordset, s.qs), s.tenant, data); err != nil {
		return err
	}
	// Equal normalized forms collapse to one row; a canonical name wins
	// over an alias, and the last input of the same kind wins.
	_, err = q.Exec(ctx, fmt.Sprintf(`INSERT INTO %s AS existing (tenant_id, taxonomy_id, language, kind, name, source_revision)
 SELECT DISTINCT ON (r.taxonomy_id, r.language, %s.contentkit_keyword_normalize(r.name)) $1, r.taxonomy_id, r.language, r.kind, r.name, r.source_revision
 FROM %s ORDER BY r.taxonomy_id, r.language, %s.contentkit_keyword_normalize(r.name), (r.kind='name') DESC, r.ordinal DESC
 ON CONFLICT (tenant_id, taxonomy_id, language, normalized) DO UPDATE SET kind=EXCLUDED.kind, name=EXCLUDED.name, source_revision=EXCLUDED.source_revision
 WHERE existing.kind <> 'name' OR EXCLUDED.kind = 'name'`,
		s.table("content_node_names"), s.qs, recordset, s.qs), s.tenant, data)
	return err
}

// CreateNodes creates nodes with their names in one transaction. A taken slug
// or id is ErrConflict; an unregistered kind is ErrInvalid.
func (s *Store) CreateNodes(ctx context.Context, inputs []NodeInput) ([]Node, error) {
	if len(inputs) == 0 {
		return []Node{}, nil
	}
	rows := make([]nodeRow, 0, len(inputs))
	for i, in := range inputs {
		kind := strings.TrimSpace(in.Kind)
		if err := s.requireKind(kind); err != nil {
			return nil, err
		}
		slug, err := validateSlug(in.Slug)
		if err != nil {
			return nil, err
		}
		if in.TaxonomyID != "" {
			if err := validateID(in.TaxonomyID); err != nil {
				return nil, err
			}
		}
		rows = append(rows, nodeRow{Ordinal: i, TaxonomyID: string(in.TaxonomyID), Kind: kind, Slug: slug, SourceRevision: in.SourceRevision})
	}
	data, err := json.Marshal(rows)
	if err != nil {
		return nil, err
	}
	out := make([]Node, len(inputs))
	err = s.run(ctx, func(q querier) error {
		res, err := q.Query(ctx, fmt.Sprintf(`INSERT INTO %s (tenant_id, taxonomy_id, kind, slug, state, source_revision)
 SELECT $1, coalesce(nullif(r.taxonomy_id, ''), gen_random_uuid()::text), r.kind, r.slug, 'active', r.source_revision
 FROM jsonb_to_recordset($2::jsonb) AS r(ordinal int, taxonomy_id text, kind text, slug text, source_revision bigint) ORDER BY r.ordinal
 RETURNING `+nodeColumns, s.table("content_nodes")), s.tenant, data)
		if err != nil {
			return err
		}
		created, err := collectRows(res, func(row rowScanner) (Node, error) { return scanNode(row) })
		if err != nil {
			return err
		}
		if len(created) != len(inputs) {
			return fmt.Errorf("taxonomy: created %d of %d nodes", len(created), len(inputs))
		}
		// RETURNING order is not guaranteed; (kind, slug) is unique in a batch that inserted.
		bySlug := make(map[[2]string]Node, len(created))
		for _, n := range created {
			bySlug[[2]string{n.Kind, n.Slug}] = n
		}
		var names []nameRow
		var marks []nodeKey
		for i, row := range rows {
			n, ok := bySlug[[2]string{row.Kind, row.Slug}]
			if !ok {
				return fmt.Errorf("taxonomy: node %s/%s was not returned", row.Kind, row.Slug)
			}
			out[i] = n
			rows, err := s.validateNames(n.TaxonomyID, inputs[i].Names)
			if err != nil {
				return err
			}
			for _, row := range rows {
				row.Ordinal = len(names)
				names = append(names, row)
			}
			marks = append(marks, nodeKey{n.Kind, n.TaxonomyID})
		}
		if err := s.insertNames(ctx, q, names); err != nil {
			return err
		}
		return s.markDirty(ctx, q, marks, false)
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// UpdateNode changes slug and state. Deleting a node removes its counts and
// documents; restoring rebuilds them. A merged node is never updated.
func (s *Store) UpdateNode(ctx context.Context, id TaxonomyID, update NodeUpdate) (Node, error) {
	if err := validateID(id); err != nil {
		return Node{}, err
	}
	var slug *string
	if update.Slug != nil {
		v, err := validateSlug(*update.Slug)
		if err != nil {
			return Node{}, err
		}
		slug = &v
	}
	if update.State != nil && *update.State != StateActive && *update.State != StateDeleted {
		return Node{}, fmt.Errorf("%w: state %q is not assignable", ErrInvalid, *update.State)
	}
	var out Node
	err := s.run(ctx, func(q querier) error {
		current, err := scanNode(q.QueryRow(ctx, fmt.Sprintf(`SELECT %s FROM %s WHERE tenant_id=$1 AND taxonomy_id=$2 FOR UPDATE`, nodeColumns, s.table("content_nodes")), s.tenant, string(id)))
		if err != nil {
			return err
		}
		if current.State == StateMerged {
			return fmt.Errorf("%w: node %s is merged", ErrConflict, id)
		}
		next := current
		if slug != nil {
			next.Slug = *slug
		}
		if update.State != nil {
			next.State = *update.State
		}
		if update.SourceRevision != nil {
			next.SourceRevision = *update.SourceRevision
		}
		out, err = scanNode(q.QueryRow(ctx, fmt.Sprintf(`UPDATE %s SET slug=$3, state=$4, source_revision=$5, updated_at=now()
 WHERE tenant_id=$1 AND taxonomy_id=$2 RETURNING `+nodeColumns, s.table("content_nodes")), s.tenant, string(id), next.Slug, next.State, next.SourceRevision))
		if err != nil {
			return err
		}
		if current.State != next.State {
			if err := s.recountNodes(ctx, q, []string{string(id)}); err != nil {
				return err
			}
		}
		return s.markDirty(ctx, q, []nodeKey{{out.Kind, out.TaxonomyID}}, out.State != StateActive)
	})
	return out, err
}

// Node returns one node of the tenant with names, edges and counts.
func (s *Store) Node(ctx context.Context, id TaxonomyID) (NodeDetail, error) {
	if err := validateID(id); err != nil {
		return NodeDetail{}, err
	}
	var d NodeDetail
	err := s.read(ctx, func(q querier) error {
		n, err := scanNode(q.QueryRow(ctx, fmt.Sprintf(`SELECT %s FROM %s WHERE tenant_id=$1 AND taxonomy_id=$2`, nodeColumns, s.table("content_nodes")), s.tenant, string(id)))
		if err != nil {
			return err
		}
		d.Node = n
		names, err := s.names(ctx, q, []string{string(id)})
		if err != nil {
			return err
		}
		d.Names = names[id]
		if d.Names == nil {
			d.Names = []Name{}
		}
		if d.Edges, err = s.edges(ctx, q, []string{string(id)}); err != nil {
			return err
		}
		counts, err := s.counts(ctx, q, []string{string(id)})
		if err != nil {
			return err
		}
		d.Counts = counts[id]
		if d.Counts == nil {
			d.Counts = []Count{}
		}
		return nil
	})
	return d, err
}

// Nodes returns the requested nodes of the tenant that exist, in input order.
func (s *Store) Nodes(ctx context.Context, ids []TaxonomyID) ([]Node, error) {
	list, err := uniqueIDs(ids)
	if err != nil {
		return nil, err
	}
	out := []Node{}
	if len(list) == 0 {
		return out, nil
	}
	err = s.read(ctx, func(q querier) error {
		rows, err := q.Query(ctx, fmt.Sprintf(`SELECT %s FROM %s WHERE tenant_id=$1 AND taxonomy_id=ANY($2::text[])`, nodeColumns, s.table("content_nodes")), s.tenant, list)
		if err != nil {
			return err
		}
		found, err := collectRows(rows, func(row rowScanner) (Node, error) { return scanNode(row) })
		if err != nil {
			return err
		}
		byID := map[TaxonomyID]Node{}
		for _, n := range found {
			byID[n.TaxonomyID] = n
		}
		for _, id := range list {
			if n, ok := byID[TaxonomyID(id)]; ok {
				out = append(out, n)
			}
		}
		return nil
	})
	return out, err
}

// listQuery is one compiled ListNodes request.
type listQuery struct {
	from, where, order string
	args               pgx.NamedArgs
	limit              int
	// cursored: the order is pageable by keyset, so the page carries NextCursor.
	cursored bool
	// total: count the unpaged matches. A cursor page does not, which is what
	// keeps a full sync O(page) instead of paying a count per page.
	total bool
}

// compileList validates the options and builds the shared FROM/WHERE. Both
// laterals carry tenant_id, as does every predicate: a node of another tenant
// can neither be listed nor lend its names or counts to one that is.
func (s *Store) compileList(opts ListOptions) (listQuery, error) {
	q := listQuery{args: pgx.NamedArgs{"tenant": s.tenant}, cursored: opts.Sort == SortID, total: opts.Cursor == ""}
	order, ok := sorts[opts.Sort]
	if !ok {
		return q, fmt.Errorf("%w: sort %q", ErrInvalid, opts.Sort)
	}
	if hostOrder := strings.TrimSpace(opts.OrderSQL); hostOrder != "" {
		if opts.Sort != SortID || opts.Cursor != "" {
			return q, fmt.Errorf("%w: OrderSQL excludes Sort and Cursor", ErrInvalid)
		}
		order = hostOrder
		q.cursored = false
	}
	if opts.Cursor != "" && (opts.Sort != SortID || opts.Offset != 0) {
		return q, fmt.Errorf("%w: Cursor pages the default sort from its own position; it excludes Sort and Offset", ErrInvalid)
	}
	q.limit = opts.Limit
	if q.limit <= 0 {
		q.limit = 50
	}
	if q.limit > 500 {
		q.limit = 500
	}
	language := strings.TrimSpace(opts.Language)
	if language == "" {
		language = s.languages[0]
	}
	language, err := normalizeLanguage(language)
	if err != nil {
		return q, err
	}
	q.args["language"], q.args["fallback"] = language, s.languages

	states := opts.States
	if len(states) == 0 {
		states = []State{StateActive}
	}
	list := make([]string, 0, len(states))
	for _, st := range states {
		if st != StateActive && st != StateMerged && st != StateDeleted {
			return q, fmt.Errorf("%w: state %q", ErrInvalid, st)
		}
		list = append(list, string(st))
	}
	q.args["states"] = list
	where := []string{"n.tenant_id = @tenant", "n.state = ANY(@states::text[])"}

	if kind := strings.TrimSpace(opts.Kind); kind != "" {
		if err := s.requireKind(kind); err != nil {
			return q, err
		}
		where = append(where, "n.kind = @kind")
		q.args["kind"] = kind
	}
	if slug := strings.TrimSpace(opts.Slug); slug != "" {
		where = append(where, "n.slug = @slug")
		q.args["slug"] = slug
	}
	if opts.Related != "" {
		if err := validateID(opts.Related); err != nil {
			return q, err
		}
		relation := ""
		if opts.Relation != "" {
			if _, ok := relations[opts.Relation]; !ok {
				return q, fmt.Errorf("%w: relation %q", ErrInvalid, opts.Relation)
			}
			relation = " AND e.relation = @relation"
			q.args["relation"] = string(opts.Relation)
		}
		where = append(where, fmt.Sprintf(`EXISTS (SELECT 1 FROM %s e
   WHERE e.tenant_id = n.tenant_id AND e.from_taxonomy_id = n.taxonomy_id AND e.to_taxonomy_id = @related%s)`,
			s.table("content_edges"), relation))
		q.args["related"] = string(opts.Related)
	} else if opts.Relation != "" {
		return q, fmt.Errorf("%w: Relation narrows Related and needs it", ErrInvalid)
	}
	if len(opts.IDs) > 0 {
		ids := make([]string, 0, len(opts.IDs))
		for _, id := range opts.IDs {
			if err := validateID(id); err != nil {
				return q, err
			}
			ids = append(ids, string(id))
		}
		where = append(where, "n.taxonomy_id = ANY(@ids::text[])")
		q.args["ids"] = ids
	}

	// The name lateral pins the language for strict and required; fallback
	// prefers the request language, then the store's configured order.
	pin := ""
	if opts.LanguageMode == LanguageStrict || opts.LanguageMode == LanguageRequired {
		pin = " AND nn.language = @language"
	} else if opts.LanguageMode != LanguageFallback {
		return q, fmt.Errorf("%w: language mode %q", ErrInvalid, opts.LanguageMode)
	}
	countPin := ""
	if ck := strings.TrimSpace(opts.ContentKind); ck != "" {
		countPin = " AND cn.content_kind = @content_kind"
		q.args["content_kind"] = ck
	}
	q.from = fmt.Sprintf(`FROM %s n
 LEFT JOIN LATERAL (SELECT nn.name, nn.language, nn.normalized FROM %s nn
   WHERE nn.tenant_id = n.tenant_id AND nn.taxonomy_id = n.taxonomy_id AND nn.kind = 'name'%s
   ORDER BY CASE WHEN nn.language = @language THEN 0 ELSE 1 + coalesce(array_position(@fallback::text[], nn.language), 1000) END, nn.language
   LIMIT 1) nm ON true
 LEFT JOIN LATERAL (SELECT sum(cn.content_count)::int AS total FROM %s cn
   WHERE cn.tenant_id = n.tenant_id AND cn.taxonomy_id = n.taxonomy_id AND cn.language = @language%s) cc ON true`,
		s.table("content_nodes"), s.table("content_node_names"), pin, s.table("content_node_counts"), countPin)

	if opts.LanguageMode == LanguageRequired {
		where = append(where, "nm.name IS NOT NULL")
	}
	if prefix := strings.TrimSpace(opts.NamePrefix); prefix != "" {
		where = append(where, "nm.normalized LIKE "+s.likePattern("@prefix", false))
		q.args["prefix"] = prefix
	}
	if query := strings.TrimSpace(opts.Query); query != "" {
		where = append(where, fmt.Sprintf(`EXISTS (SELECT 1 FROM %s qn
   WHERE qn.tenant_id = n.tenant_id AND qn.taxonomy_id = n.taxonomy_id AND qn.normalized LIKE %s)`,
			s.table("content_node_names"), s.likePattern("@query", true)))
		q.args["query"] = query
	}
	if opts.MinCount > 0 {
		where = append(where, "coalesce(cc.total, 0) >= @min_count")
		q.args["min_count"] = opts.MinCount
	}
	if opts.Cursor != "" {
		where = append(where, "n.taxonomy_id > @cursor")
		q.args["cursor"] = opts.Cursor
	}
	if filter := strings.TrimSpace(opts.FilterSQL); filter != "" {
		where = append(where, "("+filter+")")
	}
	for name, value := range opts.FilterArgs {
		if !identRE.MatchString(name) {
			return q, fmt.Errorf("%w: invalid FilterArgs name %q", ErrInvalid, name)
		}
		switch name {
		case "tenant", "language", "fallback", "states", "kind", "slug", "related", "relation", "ids", "content_kind", "prefix", "query", "min_count", "cursor", "limit", "offset":
			return q, fmt.Errorf("%w: FilterArgs name %q is reserved", ErrInvalid, name)
		}
		q.args[name] = value
	}
	q.where = strings.Join(where, " AND ")

	if order != "" {
		order += ", "
	}
	q.order = order + "n.taxonomy_id"
	return q, nil
}

// likePattern normalizes an argument the way the stored normalized column is
// generated, then escapes LIKE's wildcards so a user's % or _ stays literal.
func (s *Store) likePattern(arg string, contains bool) string {
	escaped := fmt.Sprintf(`replace(replace(replace(%s.contentkit_keyword_normalize(%s), '\', '\\'), '%%', '\%%'), '_', '\_')`, s.qs, arg)
	if contains {
		return `'%' || ` + escaped + ` || '%'`
	}
	return escaped + ` || '%'`
}

// ListNodes pages the tenant's nodes with their display name in the request
// language and their content count. The zero ListOptions keeps the historical
// behavior: active nodes ordered by taxonomy_id, paged by Cursor.
func (s *Store) ListNodes(ctx context.Context, opts ListOptions) (NodePage, error) {
	page := NodePage{Nodes: []NodeRow{}}
	q, err := s.compileList(opts)
	if err != nil {
		return page, err
	}
	q.args["limit"], q.args["offset"] = q.limit, max(opts.Offset, 0)
	if q.cursored {
		q.args["limit"] = q.limit + 1 // one extra row decides whether a next cursor exists
	}
	err = s.read(ctx, func(db querier) error {
		if q.total {
			if err := db.QueryRow(ctx, "SELECT count(*) "+q.from+" WHERE "+q.where, q.args).Scan(&page.Total); err != nil {
				return err
			}
		}
		rows, err := db.Query(ctx, fmt.Sprintf(`SELECT %s, coalesce(nm.name, ''), coalesce(nm.language, ''), coalesce(cc.total, 0) %s WHERE %s ORDER BY %s LIMIT @limit OFFSET @offset`, nodeColumnsN, q.from, q.where, q.order), q.args)
		if err != nil {
			return err
		}
		nodes, err := collectRows(rows, scanNodeRow)
		if err != nil {
			return err
		}
		if q.cursored && len(nodes) > q.limit {
			nodes = nodes[:q.limit]
			page.NextCursor = string(nodes[q.limit-1].TaxonomyID)
		}
		page.Nodes = nodes
		return nil
	})
	return page, err
}

func (s *Store) names(ctx context.Context, q querier, ids []string) (map[TaxonomyID][]Name, error) {
	rows, err := q.Query(ctx, fmt.Sprintf(`SELECT taxonomy_id, language, kind, name, normalized, source_revision FROM %s
 WHERE tenant_id=$1 AND taxonomy_id=ANY($2::text[]) ORDER BY taxonomy_id, language, kind DESC, normalized`, s.table("content_node_names")), s.tenant, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[TaxonomyID][]Name{}
	for rows.Next() {
		var id string
		var n Name
		if err := rows.Scan(&id, &n.Language, &n.Kind, &n.Name, &n.Normalized, &n.SourceRevision); err != nil {
			return nil, err
		}
		out[TaxonomyID(id)] = append(out[TaxonomyID(id)], n)
	}
	return out, rows.Err()
}

func (s *Store) lockNode(ctx context.Context, q querier, id TaxonomyID) (Node, error) {
	if err := validateID(id); err != nil {
		return Node{}, err
	}
	return scanNode(q.QueryRow(ctx, fmt.Sprintf(`SELECT %s FROM %s WHERE tenant_id=$1 AND taxonomy_id=$2 FOR UPDATE`, nodeColumns, s.table("content_nodes")), s.tenant, string(id)))
}

// SetNames replaces every name and alias of the node.
func (s *Store) SetNames(ctx context.Context, id TaxonomyID, names []Name) error {
	rows, err := s.validateNames(id, names)
	if err != nil {
		return err
	}
	return s.run(ctx, func(q querier) error {
		n, err := s.lockNode(ctx, q, id)
		if err != nil {
			return err
		}
		if _, err := q.Exec(ctx, fmt.Sprintf(`DELETE FROM %s WHERE tenant_id=$1 AND taxonomy_id=$2`, s.table("content_node_names")), s.tenant, string(id)); err != nil {
			return err
		}
		if err := s.insertNames(ctx, q, rows); err != nil {
			return err
		}
		return s.touch(ctx, q, n)
	})
}

// AddNames upserts names and aliases; a new canonical name demotes the
// previous canonical name of that language to an alias.
func (s *Store) AddNames(ctx context.Context, id TaxonomyID, names []Name) error {
	rows, err := s.validateNames(id, names)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return nil
	}
	return s.run(ctx, func(q querier) error {
		n, err := s.lockNode(ctx, q, id)
		if err != nil {
			return err
		}
		if err := s.insertNames(ctx, q, rows); err != nil {
			return err
		}
		return s.touch(ctx, q, n)
	})
}

// RemoveNames deletes the given names (matched by language and normalized
// form); unknown names are ignored.
func (s *Store) RemoveNames(ctx context.Context, id TaxonomyID, names []Name) error {
	rows, err := s.validateNames(id, names)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return nil
	}
	data, err := json.Marshal(rows)
	if err != nil {
		return err
	}
	return s.run(ctx, func(q querier) error {
		n, err := s.lockNode(ctx, q, id)
		if err != nil {
			return err
		}
		if _, err := q.Exec(ctx, fmt.Sprintf(`DELETE FROM %s n USING jsonb_to_recordset($3::jsonb) AS r(language text, name text)
 WHERE n.tenant_id=$1 AND n.taxonomy_id=$2 AND n.language=r.language AND n.normalized=%s.contentkit_keyword_normalize(r.name)`, s.table("content_node_names"), s.qs), s.tenant, string(id), data); err != nil {
			return err
		}
		return s.touch(ctx, q, n)
	})
}

// touch bumps updated_at and queues the node's documents.
func (s *Store) touch(ctx context.Context, q querier, n Node) error {
	if _, err := q.Exec(ctx, fmt.Sprintf(`UPDATE %s SET updated_at=now() WHERE tenant_id=$1 AND taxonomy_id=$2`, s.table("content_nodes")), s.tenant, string(n.TaxonomyID)); err != nil {
		return err
	}
	return s.markDirty(ctx, q, []nodeKey{{n.Kind, n.TaxonomyID}}, n.State != StateActive)
}

// MergeReport summarizes one merge.
type MergeReport struct {
	From              TaxonomyID `json:"from_taxonomy_id"`
	Into              TaxonomyID `json:"into_taxonomy_id"`
	AssignmentsMoved  int64      `json:"assignments_moved"`
	NamesMoved        int64      `json:"names_moved"`
	EdgesRewritten    int64      `json:"edges_rewritten"`
	AssignmentsMerged int64      `json:"assignments_merged"`
}

// Merge folds node from into node into of the same kind in one transaction:
// assignments are rewritten (duplicates collapse), names become aliases of
// into, edges are re-pointed, from becomes merged with an alias_of edge to
// into, counts and documents of both are rebuilt.
func (s *Store) Merge(ctx context.Context, from, into TaxonomyID) (MergeReport, error) {
	report := MergeReport{From: from, Into: into}
	if err := validateID(from); err != nil {
		return report, err
	}
	if err := validateID(into); err != nil {
		return report, err
	}
	if from == into {
		return report, fmt.Errorf("%w: cannot merge a node into itself", ErrInvalid)
	}
	err := s.run(ctx, func(q querier) error {
		// Lock in id order so concurrent merges never deadlock.
		order := []TaxonomyID{from, into}
		sort.Slice(order, func(i, j int) bool { return order[i] < order[j] })
		nodes := map[TaxonomyID]Node{}
		for _, id := range order {
			n, err := s.lockNode(ctx, q, id)
			if err != nil {
				return fmt.Errorf("%w: node %s", ErrNotFound, id)
			}
			nodes[id] = n
		}
		src, dst := nodes[from], nodes[into]
		if src.State != StateActive || dst.State != StateActive {
			return fmt.Errorf("%w: both nodes must be active", ErrConflict)
		}
		if src.Kind != dst.Kind {
			return fmt.Errorf("%w: cannot merge kind %s into %s", ErrConflict, src.Kind, dst.Kind)
		}
		a := s.table("content_assignments")
		// Folding an accepted assignment into a proposed duplicate must not
		// make the accepted content classification disappear.
		if _, err := q.Exec(ctx, fmt.Sprintf(`UPDATE %s dst SET state='active', source_revision=greatest(dst.source_revision, src.source_revision)
 FROM %s src WHERE src.tenant_id=$1 AND src.taxonomy_id=$2 AND src.state='active'
 AND dst.tenant_id=src.tenant_id AND dst.taxonomy_id=$3 AND dst.content_kind=src.content_kind AND dst.content_id=src.content_id
 AND dst.content_version_id IS NOT DISTINCT FROM src.content_version_id AND dst.relation=src.relation AND dst.state='proposed'`, a, a), s.tenant, string(from), string(into)); err != nil {
			return err
		}
		tag, err := q.Exec(ctx, fmt.Sprintf(`INSERT INTO %s (tenant_id, content_kind, content_id, content_version_id, taxonomy_id, relation, source_revision, state)
 SELECT tenant_id, content_kind, content_id, content_version_id, $3, relation, source_revision, state FROM %s WHERE tenant_id=$1 AND taxonomy_id=$2
 ON CONFLICT (tenant_id, content_kind, content_id, content_version_id, taxonomy_id, relation) DO NOTHING`, a, a), s.tenant, string(from), string(into))
		if err != nil {
			return err
		}
		report.AssignmentsMoved = tag.RowsAffected()
		if tag, err = q.Exec(ctx, fmt.Sprintf(`DELETE FROM %s WHERE tenant_id=$1 AND taxonomy_id=$2`, a), s.tenant, string(from)); err != nil {
			return err
		}
		report.AssignmentsMerged = tag.RowsAffected() - report.AssignmentsMoved
		names := s.table("content_node_names")
		if tag, err = q.Exec(ctx, fmt.Sprintf(`INSERT INTO %s (tenant_id, taxonomy_id, language, kind, name, source_revision)
 SELECT tenant_id, $3, language, 'alias', name, source_revision FROM %s WHERE tenant_id=$1 AND taxonomy_id=$2
 ON CONFLICT (tenant_id, taxonomy_id, language, normalized) DO NOTHING`, names, names), s.tenant, string(from), string(into)); err != nil {
			return err
		}
		report.NamesMoved = tag.RowsAffected()
		if _, err = q.Exec(ctx, fmt.Sprintf(`DELETE FROM %s WHERE tenant_id=$1 AND taxonomy_id=$2`, names), s.tenant, string(from)); err != nil {
			return err
		}
		edges := s.table("content_edges")
		for _, col := range []string{"from_taxonomy_id", "to_taxonomy_id"} {
			other := "to_taxonomy_id"
			if col == "to_taxonomy_id" {
				other = "from_taxonomy_id"
			}
			tag, err = q.Exec(ctx, fmt.Sprintf(`INSERT INTO %s (tenant_id, from_taxonomy_id, relation, to_taxonomy_id, source_revision)
 SELECT tenant_id, CASE WHEN '%s'='from_taxonomy_id' THEN $3 ELSE from_taxonomy_id END, relation, CASE WHEN '%s'='to_taxonomy_id' THEN $3 ELSE to_taxonomy_id END, source_revision
 FROM %s WHERE tenant_id=$1 AND %s=$2 AND %s<>$3 ON CONFLICT DO NOTHING`, edges, col, col, edges, col, other), s.tenant, string(from), string(into))
			if err != nil {
				return err
			}
			report.EdgesRewritten += tag.RowsAffected()
		}
		if _, err = q.Exec(ctx, fmt.Sprintf(`DELETE FROM %s WHERE tenant_id=$1 AND (from_taxonomy_id=$2 OR to_taxonomy_id=$2)`, edges), s.tenant, string(from)); err != nil {
			return err
		}
		if _, err = q.Exec(ctx, fmt.Sprintf(`INSERT INTO %s (tenant_id, from_taxonomy_id, relation, to_taxonomy_id) VALUES ($1, $2, 'alias_of', $3)`, edges), s.tenant, string(from), string(into)); err != nil {
			return err
		}
		if _, err = q.Exec(ctx, fmt.Sprintf(`UPDATE %s SET state='merged', updated_at=now() WHERE tenant_id=$1 AND taxonomy_id=$2`, s.table("content_nodes")), s.tenant, string(from)); err != nil {
			return err
		}
		if _, err = q.Exec(ctx, fmt.Sprintf(`UPDATE %s SET updated_at=now() WHERE tenant_id=$1 AND taxonomy_id=$2`, s.table("content_nodes")), s.tenant, string(into)); err != nil {
			return err
		}
		if err := s.recountNodes(ctx, q, []string{string(from), string(into)}); err != nil {
			return err
		}
		if err := s.markDirty(ctx, q, []nodeKey{{src.Kind, from}}, true); err != nil {
			return err
		}
		return s.markDirty(ctx, q, []nodeKey{{dst.Kind, into}}, false)
	})
	return report, err
}
