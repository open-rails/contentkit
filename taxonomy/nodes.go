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

// ListOptions filters and pages ListNodes; the page is ordered by taxonomy_id.
type ListOptions struct {
	Kind string
	// State defaults to active.
	State State
	// Slug selects one slug exactly.
	Slug string
	// Cursor is the NextCursor of the previous page.
	Cursor string
	// Limit defaults to 50 and is capped at 500.
	Limit int
}

// NodePage is one page of nodes.
type NodePage struct {
	Nodes      []Node `json:"nodes"`
	NextCursor string `json:"next_cursor,omitempty"`
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

func scanNode(row pgx.Row) (Node, error) {
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
		created, err := pgx.CollectRows(res, func(row pgx.CollectableRow) (Node, error) { return scanNode(row) })
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
		found, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Node, error) { return scanNode(row) })
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

// ListNodes pages the tenant's nodes by taxonomy_id.
func (s *Store) ListNodes(ctx context.Context, opts ListOptions) (NodePage, error) {
	page := NodePage{Nodes: []Node{}}
	state := opts.State
	if state == "" {
		state = StateActive
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	kind := strings.TrimSpace(opts.Kind)
	if kind != "" {
		if err := s.requireKind(kind); err != nil {
			return page, err
		}
	}
	args := pgx.NamedArgs{"tenant": s.tenant, "state": state, "kind": kind, "slug": strings.TrimSpace(opts.Slug), "cursor": opts.Cursor, "limit": limit + 1}
	err := s.read(ctx, func(q querier) error {
		rows, err := q.Query(ctx, fmt.Sprintf(`SELECT %s FROM %s WHERE tenant_id=@tenant AND state=@state
 AND (@kind='' OR kind=@kind) AND (@slug='' OR slug=@slug) AND taxonomy_id > @cursor ORDER BY taxonomy_id LIMIT @limit`, nodeColumns, s.table("content_nodes")), args)
		if err != nil {
			return err
		}
		nodes, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Node, error) { return scanNode(row) })
		if err != nil {
			return err
		}
		if len(nodes) > limit {
			nodes = nodes[:limit]
			page.NextCursor = string(nodes[limit-1].TaxonomyID)
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
