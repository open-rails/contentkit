package taxonomy

import (
	"context"
	"encoding/json"
	"fmt"
)

type edgeRow struct {
	From           string `json:"from"`
	Relation       string `json:"relation"`
	To             string `json:"to"`
	SourceRevision int64  `json:"source_revision"`
}

// validateEdges validates and dedupes edges; the last duplicate wins.
func validateEdges(edges []Edge) ([]edgeRow, error) {
	rows := make([]edgeRow, 0, len(edges))
	index := map[Edge]int{}
	for _, e := range edges {
		if err := validateID(e.From); err != nil {
			return nil, err
		}
		if err := validateID(e.To); err != nil {
			return nil, err
		}
		if _, ok := relations[e.Relation]; !ok {
			return nil, fmt.Errorf("%w: relation %q", ErrInvalid, e.Relation)
		}
		if e.From == e.To {
			return nil, fmt.Errorf("%w: edge %s %s %s is a self loop", ErrInvalid, e.From, e.Relation, e.To)
		}
		row := edgeRow{string(e.From), string(e.Relation), string(e.To), e.SourceRevision}
		key := Edge{From: e.From, Relation: e.Relation, To: e.To}
		if i, dup := index[key]; dup {
			rows[i] = row
			continue
		}
		index[key] = len(rows)
		rows = append(rows, row)
	}
	return rows, nil
}

// AddEdges upserts edges between nodes of the tenant; an endpoint outside the
// tenant is ErrNotFound.
func (s *Store) AddEdges(ctx context.Context, edges []Edge) error {
	rows, err := validateEdges(edges)
	if err != nil || len(rows) == 0 {
		return err
	}
	data, err := json.Marshal(rows)
	if err != nil {
		return err
	}
	return s.run(ctx, func(q querier) error {
		_, err := q.Exec(ctx, fmt.Sprintf(`INSERT INTO %s (tenant_id, from_taxonomy_id, relation, to_taxonomy_id, source_revision)
 SELECT $1, r."from", r.relation, r."to", r.source_revision FROM jsonb_to_recordset($2::jsonb) AS r("from" text, relation text, "to" text, source_revision bigint)
 ON CONFLICT (tenant_id, from_taxonomy_id, relation, to_taxonomy_id) DO UPDATE SET source_revision=EXCLUDED.source_revision`, s.table("content_edges")), s.tenant, data)
		return err
	})
}

// RemoveEdges deletes edges; unknown edges are ignored.
func (s *Store) RemoveEdges(ctx context.Context, edges []Edge) error {
	rows, err := validateEdges(edges)
	if err != nil || len(rows) == 0 {
		return err
	}
	data, err := json.Marshal(rows)
	if err != nil {
		return err
	}
	return s.run(ctx, func(q querier) error {
		_, err := q.Exec(ctx, fmt.Sprintf(`DELETE FROM %s e USING jsonb_to_recordset($2::jsonb) AS r("from" text, relation text, "to" text)
 WHERE e.tenant_id=$1 AND e.from_taxonomy_id=r."from" AND e.relation=r.relation AND e.to_taxonomy_id=r."to"`, s.table("content_edges")), s.tenant, data)
		return err
	})
}

// Edges returns every edge touching the given nodes, in either direction.
func (s *Store) Edges(ctx context.Context, ids []TaxonomyID) ([]Edge, error) {
	list, err := uniqueIDs(ids)
	if err != nil {
		return nil, err
	}
	out := []Edge{}
	if len(list) == 0 {
		return out, nil
	}
	err = s.read(ctx, func(q querier) error {
		out, err = s.edges(ctx, q, list)
		return err
	})
	return out, err
}

func (s *Store) edges(ctx context.Context, q querier, ids []string) ([]Edge, error) {
	rows, err := q.Query(ctx, fmt.Sprintf(`SELECT from_taxonomy_id, relation, to_taxonomy_id, source_revision FROM %s
 WHERE tenant_id=$1 AND (from_taxonomy_id=ANY($2::text[]) OR to_taxonomy_id=ANY($2::text[]))
 ORDER BY from_taxonomy_id, relation, to_taxonomy_id`, s.table("content_edges")), s.tenant, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Edge{}
	for rows.Next() {
		var e Edge
		if err := rows.Scan(&e.From, &e.Relation, &e.To, &e.SourceRevision); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
