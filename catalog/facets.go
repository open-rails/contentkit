package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/groups"
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
	rows, err := q.Query(ctx, `SELECT v.id, c.access, v.access FROM `+s.t.versions+` v JOIN `+s.t.items+` c ON c.tenant_id = v.tenant_id AND c.id = v.content_id
 WHERE v.tenant_id = $1 AND v.content_id = ANY($2) ORDER BY v.id`, s.tenant, ids)
	if err != nil {
		return err
	}
	// versionFacet is a version's access facet (access.Rule.Facet of its
	// effective rule) and effective groups.
	type versionFacet struct {
		VersionID string       `json:"version_id"`
		Level     access.Level `json:"level"`
		Tiers     []string     `json:"tiers"`
		GroupID   *string      `json:"group_id"`
		FreeAfter *time.Time   `json:"free_after"`
		Groups    []string     `json:"groups"`
	}
	var facets []versionFacet
	var versions []string
	for rows.Next() {
		var id string
		var content, override []byte
		if err := rows.Scan(&id, &content, &override); err != nil {
			rows.Close()
			return err
		}
		rule, err := effectiveRule(content, override)
		if err != nil {
			rows.Close()
			return fmt.Errorf("catalog: version %s: %w", id, err)
		}
		f := rule.Facet()
		facets = append(facets, versionFacet{VersionID: id, Level: f.Level, Tiers: f.Tiers, GroupID: f.GroupID, FreeAfter: f.FreeAfter, Groups: []string{}})
		versions = append(versions, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if s.groups != nil && len(versions) > 0 {
		groups, err := s.groups.EffectiveGroups(ctx, q, versions)
		if err != nil {
			return err
		}
		for i := range facets {
			if gs := groups[facets[i].VersionID]; len(gs) > 0 {
				facets[i].Groups = slices.Compact(slices.Sorted(slices.Values(gs)))
			}
		}
	}
	data, err := json.Marshal(facets)
	if err != nil {
		return err
	}
	if _, err := q.Exec(ctx, `INSERT INTO `+s.t.facets+` AS f (tenant_id, content_id, version_id, kind, live_from, deleted, listing, languages,
 access_level, access_tiers, access_group_id, access_free_after, restrictions, group_ids, traits, owner_group_id, position, published_at, attributes,
 view_baseline, updated_at)
 SELECT c.tenant_id, c.id, v.id, c.kind,
  CASE WHEN (c.state = 'published' OR (c.state = 'deleted' AND c.published_at IS NOT NULL))
   AND (v.state = 'published' OR (v.state = 'deleted' AND v.published_at IS NOT NULL))
   AND (c.moderation = 'approved' OR c.published_snapshot IS NOT NULL)
   THEN greatest(coalesce(c.published_at, c.publish_at), coalesce(v.published_at, v.publish_at)) END,
  c.state = 'deleted' OR v.state = 'deleted', c.listing, v.languages,
  a.level, a.tiers, a.group_id, a.free_after, c.restrictions, a.groups, v.traits, c.owner_group_id, v.position, coalesce(c.published_at, c.publish_at),
  c.attributes || v.attributes, c.view_baseline, clock_timestamp()
 FROM jsonb_to_recordset($3::jsonb) AS a(version_id text, level text, tiers text[], group_id text, free_after timestamptz, groups text[])
 JOIN `+s.t.versions+` v ON v.tenant_id = $1 AND v.id = a.version_id
 JOIN `+s.t.items+` c ON c.tenant_id = v.tenant_id AND c.id = v.content_id
 WHERE c.id = ANY($2)
 ON CONFLICT (tenant_id, version_id) DO UPDATE SET content_id = EXCLUDED.content_id, kind = EXCLUDED.kind, live_from = EXCLUDED.live_from,
  deleted = EXCLUDED.deleted, listing = EXCLUDED.listing, languages = EXCLUDED.languages, access_level = EXCLUDED.access_level,
  access_tiers = EXCLUDED.access_tiers, access_group_id = EXCLUDED.access_group_id, access_free_after = EXCLUDED.access_free_after,
  restrictions = EXCLUDED.restrictions, group_ids = EXCLUDED.group_ids, traits = EXCLUDED.traits, owner_group_id = EXCLUDED.owner_group_id,
  position = EXCLUDED.position, published_at = EXCLUDED.published_at, attributes = EXCLUDED.attributes, view_baseline = EXCLUDED.view_baseline,
  updated_at = EXCLUDED.updated_at,
  free_applied = f.free_applied AND f.access_free_after IS NOT DISTINCT FROM EXCLUDED.access_free_after`,
		s.tenant, ids, string(data)); err != nil {
		return err
	}
	if err := s.recordAccessHistory(ctx, q, ids); err != nil {
		return err
	}
	return s.markDocuments(ctx, q, ids)
}

// effectiveRule decodes a version's effective rule: its override, else its
// content's.
func effectiveRule(content, override []byte) (access.Rule, error) {
	var c access.Rule
	if err := json.Unmarshal(content, &c); err != nil {
		return c, err
	}
	if override == nil {
		return c, nil
	}
	var o access.Rule
	if err := json.Unmarshal(override, &o); err != nil {
		return c, err
	}
	return access.Effective(c, &o), nil
}

// markDocuments queues the content items' keyword documents in every language
// they have text or a document in; the worker rebuilds or deletes each.
func (s *Store) markDocuments(ctx context.Context, q Querier, ids []string) error {
	rows, err := q.Query(ctx, `SELECT c.kind, c.id, l.language FROM `+s.t.items+` c CROSS JOIN LATERAL (
 SELECT t.language FROM `+s.t.text+` t WHERE t.tenant_id = c.tenant_id AND t.content_id = c.id
 UNION SELECT d.language FROM `+s.t.docs+` d WHERE d.tenant_id = c.tenant_id AND d.content_kind = c.kind AND d.content_id = c.id AND d.content_version_id IS NULL) l
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
	if err := search.MarkDirty(ctx, q, s.schema, marks); err != nil || len(marks) == 0 || s.documents == nil {
		return err
	}
	return s.documents(ctx, q)
}

// ContentChanged is groups.Options.ContentChanged: after the groups module
// re-derived the items' group_ids in q's transaction, it queues their search
// documents (group names are keywords) and records the change.
func (s *Store) ContentChanged(ctx context.Context, q groups.Querier, contentIDs []string) error {
	ids := slices.Compact(slices.Sorted(slices.Values(contentIDs)))
	if len(ids) == 0 {
		return nil
	}
	if err := s.markDocuments(ctx, q, ids); err != nil {
		return err
	}
	// Catalog revisions track association changes independently of the imported
	// source timestamp. Advancing in this transaction lets optimistic editors
	// and background publication observe them under the same content-row lock.
	rows, err := q.Query(ctx, `WITH changed AS (UPDATE `+s.t.items+` SET revision=revision+1
 WHERE tenant_id = $1 AND id = ANY($2) RETURNING id, kind) SELECT id,kind FROM changed ORDER BY id`, s.tenant, ids)
	if err != nil {
		return err
	}
	var events []Event
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ContentID, &e.Kind); err != nil {
			rows.Close()
			return err
		}
		e.Type = EventContentUpdated
		events = append(events, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	return s.emit(ctx, q, events...)
}
