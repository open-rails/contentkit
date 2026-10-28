package entitystore

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"

	entities "github.com/open-rails/contentkit/entities"
)

var relationRE = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

type edgeRow struct {
	From     string `json:"from"`
	Relation string `json:"relation"`
	To       string `json:"to"`
}

func validateEdges(edges []entities.Edge) ([]edgeRow, error) {
	rows := make([]edgeRow, 0, len(edges))
	seen := map[edgeRow]bool{}
	for _, e := range edges {
		if err := validateID(e.From); err != nil {
			return nil, err
		}
		if err := validateID(e.To); err != nil {
			return nil, err
		}
		rel := strings.TrimSpace(e.Relation)
		if !relationRE.MatchString(rel) {
			return nil, fmt.Errorf("%w: relation %q must match %s", entities.ErrInvalid, rel, relationRE)
		}
		if e.From == e.To {
			return nil, fmt.Errorf("%w: edge %s %s %s is a self loop", entities.ErrInvalid, e.From, rel, e.To)
		}
		row := edgeRow{string(e.From), rel, string(e.To)}
		if !seen[row] {
			seen[row] = true
			rows = append(rows, row)
		}
	}
	return rows, nil
}

// AddEdges adds edges between active entities of the tenant; an endpoint that
// is missing or not active is ErrNotFound.
func (s *Store) AddEdges(ctx context.Context, edges []entities.Edge) error {
	rows, err := validateEdges(edges)
	if err != nil || len(rows) == 0 {
		return err
	}
	data, err := json.Marshal(rows)
	if err != nil {
		return err
	}
	ids := make([]string, 0, 2*len(rows))
	for _, r := range rows {
		ids = append(ids, r.From, r.To)
	}
	slices.Sort(ids)
	return s.run(ctx, func(q Querier) error {
		// Hold the endpoints against a merge or delete retiring one of them
		// until the edge is in; ordered like their FOR UPDATE.
		if _, err := q.Exec(ctx, fmt.Sprintf(`SELECT 1 FROM %s g WHERE g.tenant_id = $1 AND g.id = ANY($2::text[]) ORDER BY g.id FOR SHARE`,
			s.table("content_entities")), s.tenant, slices.Compact(ids)); err != nil {
			return err
		}
		var inactive int
		if err := q.QueryRow(ctx, fmt.Sprintf(`SELECT count(*) FROM jsonb_to_recordset($2::jsonb) AS r("from" text, "to" text), LATERAL unnest(ARRAY[r."from", r."to"]) AS e(id)
 WHERE NOT EXISTS (SELECT 1 FROM %s g WHERE g.tenant_id = $1 AND g.id = e.id AND g.state = 'active' AND g.kind<>'channel')`, s.table("content_entities")), s.tenant, data).Scan(&inactive); err != nil {
			return err
		}
		if inactive > 0 {
			return fmt.Errorf("%w: an edge names a group that is not active", ErrNotFound)
		}
		_, err := q.Exec(ctx, fmt.Sprintf(`INSERT INTO %s (tenant_id, from_id, relation, to_id)
 SELECT $1, r."from", r.relation, r."to" FROM jsonb_to_recordset($2::jsonb) AS r("from" text, relation text, "to" text)
 ON CONFLICT DO NOTHING`, s.table("content_entity_edges")), s.tenant, data)
		return err
	})
}

// RemoveEdges deletes edges; unknown edges are ignored.
func (s *Store) RemoveEdges(ctx context.Context, edges []entities.Edge) error {
	rows, err := validateEdges(edges)
	if err != nil || len(rows) == 0 {
		return err
	}
	data, err := json.Marshal(rows)
	if err != nil {
		return err
	}
	return s.run(ctx, func(q Querier) error {
		_, err := q.Exec(ctx, fmt.Sprintf(`DELETE FROM %s e USING jsonb_to_recordset($2::jsonb) AS r("from" text, relation text, "to" text)
 WHERE e.tenant_id = $1 AND e.from_id = r."from" AND e.relation = r.relation AND e.to_id = r."to"`, s.table("content_entity_edges")), s.tenant, data)
		return err
	})
}

// edges returns every edge touching the given entities, in either direction.
func (s *Store) edges(ctx context.Context, q Querier, ids []string) ([]entities.Edge, error) {
	rows, err := q.Query(ctx, fmt.Sprintf(`SELECT from_id, relation, to_id FROM %s
 WHERE tenant_id = $1 AND (from_id = ANY($2::text[]) OR to_id = ANY($2::text[])) ORDER BY from_id, relation, to_id`, s.table("content_entity_edges")), s.tenant, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []entities.Edge{}
	for rows.Next() {
		var e entities.Edge
		if err := rows.Scan(&e.From, &e.Relation, &e.To); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
