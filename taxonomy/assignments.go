package taxonomy

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/search"
)

// AssignOptions controls count maintenance of one write.
type AssignOptions struct {
	// SuppressCounts skips the per-node recount (bulk loads); run
	// RebuildCounts afterwards.
	SuppressCounts bool
}

type assignmentRow struct {
	Ordinal          int     `json:"ordinal"`
	ContentKind      string  `json:"content_kind"`
	ContentID        string  `json:"content_id"`
	ContentVersionID *string `json:"content_version_id"`
	TaxonomyID       string  `json:"taxonomy_id"`
	Relation         string  `json:"relation"`
	State            string  `json:"state"`
	SourceRevision   int64   `json:"source_revision"`
}

const assignmentRecordset = `jsonb_to_recordset($2::jsonb) AS r(ordinal int, content_kind text, content_id text, content_version_id text, taxonomy_id text, relation text, state text, source_revision bigint)`

func (s *Store) validateAssignments(assignments []Assignment) ([]assignmentRow, []string, error) {
	rows := make([]assignmentRow, 0, len(assignments))
	var ids []TaxonomyID
	for _, a := range assignments {
		if err := s.requireTenant(a.ContentRef); err != nil {
			return nil, nil, err
		}
		if err := validateID(a.TaxonomyID); err != nil {
			return nil, nil, err
		}
		relation, err := validateRelationName(a.Relation)
		if err != nil {
			return nil, nil, err
		}
		state := a.State
		if state == "" {
			state = AssignmentActive
		}
		if state != AssignmentActive && state != AssignmentProposed {
			return nil, nil, fmt.Errorf("%w: assignment state %q", ErrInvalid, state)
		}
		rows = append(rows, assignmentRow{Ordinal: len(rows), ContentKind: a.ContentKind, ContentID: a.ContentID, ContentVersionID: a.ContentVersionID, TaxonomyID: string(a.TaxonomyID), Relation: relation, State: string(state), SourceRevision: a.SourceRevision})
		ids = append(ids, a.TaxonomyID)
	}
	list, _ := uniqueIDs(ids)
	return rows, list, nil
}

// Assign upserts assignments to active nodes of the tenant. A node that is
// missing, merged, deleted or of another tenant is ErrNotFound. Hosts mark
// their content documents dirty in the same transaction.
func (s *Store) Assign(ctx context.Context, assignments []Assignment, opts AssignOptions) error {
	rows, ids, err := s.validateAssignments(assignments)
	if err != nil || len(rows) == 0 {
		return err
	}
	data, err := json.Marshal(rows)
	if err != nil {
		return err
	}
	return s.run(ctx, func(q querier) error {
		if err := s.lockNodes(ctx, q, ids); err != nil {
			return err
		}
		var active int
		if err := q.QueryRow(ctx, fmt.Sprintf(`SELECT count(*) FROM %s WHERE tenant_id=$1 AND taxonomy_id=ANY($2::text[]) AND state='active'`, s.table("content_nodes")), s.tenant, ids).Scan(&active); err != nil {
			return err
		}
		if active != len(ids) {
			return fmt.Errorf("%w: %d of %d nodes are not active nodes of tenant %s", ErrNotFound, len(ids)-active, len(ids), s.tenant)
		}
		if _, err := q.Exec(ctx, fmt.Sprintf(`INSERT INTO %s (tenant_id, content_kind, content_id, content_version_id, taxonomy_id, relation, state, source_revision)
 SELECT DISTINCT ON (r.content_kind, r.content_id, r.content_version_id, r.taxonomy_id, coalesce(nullif(r.relation,''), n.kind)) $1, r.content_kind, r.content_id, r.content_version_id, r.taxonomy_id, coalesce(nullif(r.relation,''), n.kind), r.state, r.source_revision
 FROM %s JOIN %s n ON n.tenant_id=$1 AND n.taxonomy_id=r.taxonomy_id
 ORDER BY r.content_kind, r.content_id, r.content_version_id, r.taxonomy_id, coalesce(nullif(r.relation,''), n.kind), r.ordinal DESC
 ON CONFLICT (tenant_id, content_kind, content_id, content_version_id, taxonomy_id, relation) DO UPDATE SET state=EXCLUDED.state, source_revision=EXCLUDED.source_revision`,
			s.table("content_assignments"), assignmentRecordset, s.table("content_nodes")), s.tenant, data); err != nil {
			return err
		}
		if opts.SuppressCounts {
			return nil
		}
		return s.recountNodes(ctx, q, ids)
	})
}

// Unassign deletes assignments matched by content reference, node and
// relation (an empty relation matches the node's kind); unknown ones are
// ignored.
func (s *Store) Unassign(ctx context.Context, assignments []Assignment, opts AssignOptions) error {
	rows, ids, err := s.validateAssignments(assignments)
	if err != nil || len(rows) == 0 {
		return err
	}
	data, err := json.Marshal(rows)
	if err != nil {
		return err
	}
	return s.run(ctx, func(q querier) error {
		if err := s.lockNodes(ctx, q, ids); err != nil {
			return err
		}
		if _, err := q.Exec(ctx, fmt.Sprintf(`DELETE FROM %s a USING %s JOIN %s n ON n.tenant_id=$1 AND n.taxonomy_id=r.taxonomy_id
 WHERE a.tenant_id=$1 AND a.content_kind=r.content_kind AND a.content_id=r.content_id AND a.content_version_id IS NOT DISTINCT FROM r.content_version_id
   AND a.taxonomy_id=r.taxonomy_id AND a.relation=coalesce(nullif(r.relation,''), n.kind)`,
			s.table("content_assignments"), assignmentRecordset, s.table("content_nodes")), s.tenant, data); err != nil {
			return err
		}
		if opts.SuppressCounts {
			return nil
		}
		return s.recountNodes(ctx, q, ids)
	})
}

func (s *Store) lockNodes(ctx context.Context, q querier, ids []string) error {
	// Sorted row locks serialize recounts of one node without deadlocks.
	_, err := q.Exec(ctx, fmt.Sprintf(`SELECT 1 FROM %s WHERE tenant_id=$1 AND taxonomy_id=ANY($2::text[]) ORDER BY taxonomy_id FOR UPDATE`, s.table("content_nodes")), s.tenant, ids)
	return err
}

type refRow struct {
	Ordinal          int    `json:"ordinal"`
	ContentKind      string `json:"content_kind"`
	ContentID        string `json:"content_id"`
	ContentVersionID string `json:"content_version_id"`
}

func (s *Store) refRows(refs []contentref.ContentRef) ([]byte, error) {
	rows := make([]refRow, 0, len(refs))
	for i, ref := range refs {
		if err := s.requireTenant(ref); err != nil {
			return nil, err
		}
		rows = append(rows, refRow{i, ref.ContentKind, ref.ContentID, ref.Version()})
	}
	return json.Marshal(rows)
}

// Assignments returns the stored assignments of each reference exactly as
// scoped: a work reference returns work rows, a version reference returns
// that version's rows. See EffectiveTags for the union.
func (s *Store) Assignments(ctx context.Context, refs []contentref.ContentRef) (map[contentref.ContentKey][]Assignment, error) {
	out := map[contentref.ContentKey][]Assignment{}
	if len(refs) == 0 {
		return out, nil
	}
	data, err := s.refRows(refs)
	if err != nil {
		return nil, err
	}
	err = s.read(ctx, func(q querier) error {
		rows, err := q.Query(ctx, fmt.Sprintf(`SELECT a.content_kind, a.content_id, a.content_version_id, a.taxonomy_id, a.relation, a.state, a.source_revision
 FROM %s a JOIN jsonb_to_recordset($2::jsonb) AS r(content_kind text, content_id text, content_version_id text)
   ON r.content_kind=a.content_kind AND r.content_id=a.content_id AND r.content_version_id=coalesce(a.content_version_id,'')
 WHERE a.tenant_id=$1 ORDER BY a.content_kind, a.content_id, a.content_version_id, a.relation, a.taxonomy_id`, s.table("content_assignments")), s.tenant, data)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var a Assignment
			var version *string
			if err := rows.Scan(&a.ContentKind, &a.ContentID, &version, &a.TaxonomyID, &a.Relation, &a.State, &a.SourceRevision); err != nil {
				return err
			}
			a.TenantID = s.tenant
			a.ContentVersionID = version
			out[a.Key()] = append(out[a.Key()], a)
		}
		return rows.Err()
	})
	return out, err
}

// EffectiveTags returns, per reference, the deduplicated union of the work's
// active assignments and, for a version reference, that version's own. Only
// active nodes are effective; a work row wins over a version row for the same
// node and relation.
func (s *Store) EffectiveTags(ctx context.Context, refs []contentref.ContentRef) (map[contentref.ContentKey][]EffectiveTag, error) {
	out := map[contentref.ContentKey][]EffectiveTag{}
	if len(refs) == 0 {
		return out, nil
	}
	data, err := s.refRows(refs)
	if err != nil {
		return nil, err
	}
	err = s.read(ctx, func(q querier) error {
		rows, err := q.Query(ctx, fmt.Sprintf(`SELECT DISTINCT ON (r.ordinal, a.taxonomy_id, a.relation) r.ordinal, a.taxonomy_id, n.kind, n.slug, a.relation, a.content_version_id IS NULL
 FROM jsonb_to_recordset($2::jsonb) AS r(ordinal int, content_kind text, content_id text, content_version_id text)
 JOIN %s a ON a.tenant_id=$1 AND a.content_kind=r.content_kind AND a.content_id=r.content_id AND (a.content_version_id IS NULL OR a.content_version_id=r.content_version_id) AND a.state='active'
 JOIN %s n ON n.tenant_id=a.tenant_id AND n.taxonomy_id=a.taxonomy_id AND n.state='active'
 ORDER BY r.ordinal, a.taxonomy_id, a.relation, a.content_version_id IS NULL DESC`, s.table("content_assignments"), s.table("content_nodes")), s.tenant, data)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var ordinal int
			var t EffectiveTag
			var work bool
			if err := rows.Scan(&ordinal, &t.TaxonomyID, &t.Kind, &t.Slug, &t.Relation, &work); err != nil {
				return err
			}
			t.Scope = ScopeVersion
			if work {
				t.Scope = ScopeContent
			}
			k := refs[ordinal].Key()
			out[k] = append(out[k], t)
		}
		return rows.Err()
	})
	return out, err
}

// RequireAll returns a FilterSQL fragment and its args for search.Options and
// Browse: every node id must be effective (work or that version) on the
// candidate document sd, so a multi-node filter holds on one version. Combine
// with the host's own FilterSQL using AND. Reserved arg: taxonomy_ids.
func RequireAll(schema string, ids []TaxonomyID) (string, map[string]any, error) {
	qs, err := search.QuoteSchema(schema)
	if err != nil {
		return "", nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	list, err := uniqueIDs(ids)
	if err != nil {
		return "", nil, err
	}
	if len(list) == 0 {
		return "", nil, fmt.Errorf("%w: RequireAll needs at least one node", ErrInvalid)
	}
	return requireAllSQL(qs), map[string]any{"taxonomy_ids": list}, nil
}

func requireAllSQL(qs string) string {
	return fmt.Sprintf(`(SELECT count(DISTINCT a.taxonomy_id) FROM %s.content_assignments a JOIN %s.content_nodes n ON n.tenant_id=a.tenant_id AND n.taxonomy_id=a.taxonomy_id AND n.state='active'
 WHERE a.tenant_id=sd.tenant_id AND a.content_kind=sd.content_kind AND a.content_id=sd.content_id AND coalesce(a.content_version_id,'') IN ('', sd.content_version_id)
   AND a.state='active' AND a.taxonomy_id=ANY(@taxonomy_ids::text[])) = cardinality(@taxonomy_ids::text[])`, qs, qs)
}

// BrowseOptions lists works of one kind that have an eligible document in
// Language on which every RequireAll node is effective.
type BrowseOptions struct {
	ContentKind string
	Language    string
	RequireAll  []TaxonomyID
	// Eligibility is the request's host join (see search.Eligibility).
	// Reserved arg names: tenant, kind, language, limit, offset, taxonomy_ids.
	Eligibility *search.Eligibility
	// Limit defaults to 50 and is capped at search.MaxCandidateLimit.
	Limit  int
	Offset int
}

// BrowseHit is one work represented by its lowest-priority eligible version.
type BrowseHit struct {
	contentref.ContentRef
	Language string `json:"language"`
	Priority int32  `json:"priority"`
}

// BrowsePage is one page of works ordered by content id.
type BrowsePage struct {
	Hits    []BrowseHit `json:"hits"`
	HasMore bool        `json:"has_more"`
}

// Browse runs the same per-document join as keyword search over the tenant's
// documents: the host decides eligibility of each version row, RequireAll
// holds on that very row, and works are grouped before paging.
func (s *Store) Browse(ctx context.Context, opts BrowseOptions) (BrowsePage, error) {
	page := BrowsePage{Hits: []BrowseHit{}}
	kind := strings.TrimSpace(opts.ContentKind)
	if kind == "" || s.isKind(kind) {
		return page, fmt.Errorf("%w: ContentKind %q must be a host content kind", ErrInvalid, kind)
	}
	language, err := normalizeLanguage(opts.Language)
	if err != nil {
		return page, err
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > search.MaxCandidateLimit || opts.Offset < 0 {
		return page, fmt.Errorf("%w: Limit must not exceed %d and Offset must not be negative", ErrInvalid, search.MaxCandidateLimit)
	}
	filter, filterArgs, err := RequireAll(s.schema, opts.RequireAll)
	if err != nil {
		return page, err
	}
	args := pgx.NamedArgs{"tenant": s.tenant, "kind": kind, "language": language, "limit": limit + 1, "offset": opts.Offset}
	for k, v := range filterArgs {
		args[k] = v
	}
	join, priority, err := search.EligibilityJoin(opts.Eligibility, args)
	if err != nil {
		return page, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	err = s.read(ctx, func(q querier) error {
		rows, err := q.Query(ctx, fmt.Sprintf(`SELECT content_id, content_version_id, priority FROM (
 SELECT DISTINCT ON (sd.content_id) sd.content_id, sd.content_version_id, %s AS priority
 FROM %s.content_search_documents sd%s
 WHERE sd.tenant_id=@tenant AND sd.content_kind=@kind AND sd.language=@language AND %s
 ORDER BY sd.content_id, %s, sd.content_version_id) w ORDER BY content_id LIMIT @limit OFFSET @offset`, priority, s.qs, join, filter, priority), args)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id, version string
			var pr int32
			if err := rows.Scan(&id, &version, &pr); err != nil {
				return err
			}
			page.Hits = append(page.Hits, BrowseHit{ContentRef: contentref.New(s.tenant, kind, id).WithVersion(version), Language: language, Priority: pr})
		}
		return rows.Err()
	})
	if err != nil {
		return page, err
	}
	if len(page.Hits) > limit {
		page.Hits = page.Hits[:limit]
		page.HasMore = true
	}
	return page, nil
}
