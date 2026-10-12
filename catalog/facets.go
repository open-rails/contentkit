package catalog

import (
	"context"
	"encoding/json"
	"slices"

	"github.com/open-rails/contentkit/search"
)

// RefreshFacets rewrites the content_facets rows of every version of the
// content items, and queues their search documents, in q's transaction.
// live_from is when a version went (or goes) live, kept through deletion so
// holders of a deleted item still read what was live; deleted marks either
// deletion. Every
// write to a content item, its versions, memberships, access, listing,
// restrictions, traits or attributes calls it before committing; the groups
// module calls it after membership changes.
func (s *Store) RefreshFacets(ctx context.Context, q Querier, contentIDs []string) error {
	ids := slices.Compact(slices.Sorted(slices.Values(contentIDs)))
	if len(ids) == 0 {
		return nil
	}
	groups := map[string][]string{}
	if s.groups != nil {
		var versions []string
		rows, err := q.Query(ctx, `SELECT id FROM `+s.t.versions+` WHERE tenant_id = $1 AND content_id = ANY($2) ORDER BY id`, s.tenant, ids)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			versions = append(versions, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if len(versions) > 0 {
			if groups, err = s.groups(ctx, q, versions); err != nil {
				return err
			}
		}
	}
	type versionGroups struct {
		VersionID string   `json:"version_id"`
		IDs       []string `json:"ids"`
	}
	vg := make([]versionGroups, 0, len(groups))
	for v, gs := range groups {
		vg = append(vg, versionGroups{v, slices.Compact(slices.Sorted(slices.Values(gs)))})
	}
	data, err := json.Marshal(vg)
	if err != nil {
		return err
	}
	if _, err := q.Exec(ctx, `INSERT INTO `+s.t.facets+` AS f (tenant_id, content_id, version_id, kind, live_from, deleted, listing, languages,
 access_level, access_tiers, access_group_id, restrictions, group_ids, traits, owner_group_id, position, published_at, attributes, updated_at)
 SELECT c.tenant_id, c.id, v.id, c.kind,
  CASE WHEN (c.state = 'published' OR (c.state = 'deleted' AND c.published_at IS NOT NULL))
   AND (v.state = 'published' OR (v.state = 'deleted' AND v.published_at IS NOT NULL))
   AND (c.moderation = 'approved' OR c.published_snapshot IS NOT NULL)
   THEN greatest(coalesce(c.published_at, c.publish_at), coalesce(v.published_at, v.publish_at)) END,
  c.state = 'deleted' OR v.state = 'deleted', c.listing, v.languages,
  r.rule->>'level', ARRAY(SELECT jsonb_array_elements_text(coalesce(r.rule->'tiers', '[]'))), nullif(r.rule->>'group_id', ''),
  c.restrictions, coalesce(g.ids, '{}'), v.traits, c.owner_group_id, v.position, coalesce(c.published_at, c.publish_at),
  c.attributes || v.attributes, clock_timestamp()
 FROM `+s.t.items+` c JOIN `+s.t.versions+` v ON v.tenant_id = c.tenant_id AND v.content_id = c.id
 CROSS JOIN LATERAL (SELECT coalesce(v.access, c.access) AS rule) r
 LEFT JOIN jsonb_to_recordset($3::jsonb) AS g(version_id text, ids text[]) ON g.version_id = v.id
 WHERE c.tenant_id = $1 AND c.id = ANY($2)
 ON CONFLICT (tenant_id, version_id) DO UPDATE SET content_id = EXCLUDED.content_id, kind = EXCLUDED.kind, live_from = EXCLUDED.live_from,
  deleted = EXCLUDED.deleted, listing = EXCLUDED.listing, languages = EXCLUDED.languages, access_level = EXCLUDED.access_level,
  access_tiers = EXCLUDED.access_tiers, access_group_id = EXCLUDED.access_group_id, restrictions = EXCLUDED.restrictions,
  group_ids = EXCLUDED.group_ids, traits = EXCLUDED.traits, owner_group_id = EXCLUDED.owner_group_id, position = EXCLUDED.position,
  published_at = EXCLUDED.published_at, attributes = EXCLUDED.attributes, updated_at = EXCLUDED.updated_at`, s.tenant, ids, string(data)); err != nil {
		return err
	}
	return s.markDocuments(ctx, q, ids)
}

// markDocuments queues the content items' keyword documents in every language
// they have text or a document in; the worker rebuilds or deletes each.
func (s *Store) markDocuments(ctx context.Context, q Querier, ids []string) error {
	rows, err := q.Query(ctx, `SELECT c.kind, c.id, l.language FROM `+s.t.items+` c CROSS JOIN LATERAL (
 SELECT t.language FROM `+s.t.text+` t WHERE t.tenant_id = c.tenant_id AND t.content_id = c.id
 UNION SELECT d.language FROM `+s.t.docs+` d WHERE d.tenant_id = c.tenant_id AND d.content_kind = c.kind AND d.content_id = c.id AND d.content_version_id = '') l
 WHERE c.tenant_id = $1 AND c.id = ANY($2)`, s.tenant, ids)
	if err != nil {
		return err
	}
	var marks []search.DirtyMark
	for rows.Next() {
		var kind, id, language string
		if err := rows.Scan(&kind, &id, &language); err != nil {
			rows.Close()
			return err
		}
		marks = append(marks, search.DirtyMark{DocumentKey: search.DocumentKey{ContentRef: s.Ref(kind, id), Language: language}, Reason: "catalog"})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	return search.MarkDirty(ctx, q, s.schema, marks)
}
