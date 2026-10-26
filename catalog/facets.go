package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/entities"
	"github.com/open-rails/contentkit/search"
)

// RefreshFacets rewrites the content_facets rows of every version of the
// content items, and queues their search documents, in q's transaction.
// live_from is when a version's release completed, kept through deletion so
// holders of a deleted item still read what was live; deleted marks either
// deletion; unplayable marks a gated player version whose media does not
// play yet (playable false; unknown lists).
// Every
// write to a content item, its versions, memberships, access, listing,
// restrictions, traits or attributes calls it before committing; the entities
// module calls it after membership changes.
func (s *Store) RefreshFacets(ctx context.Context, q Querier, contentIDs []string) error {
	ids := slices.Compact(slices.Sorted(slices.Values(contentIDs)))
	if len(ids) == 0 {
		return nil
	}
	rows, err := q.Query(ctx, `SELECT v.id, c.access, CASE WHEN v.released THEN v.released_access ELSE v.access END FROM `+s.t.versions+` v JOIN `+s.t.items+` c ON c.tenant_id = v.tenant_id AND c.id = v.content_id
 WHERE v.tenant_id = $1 AND v.content_id = ANY($2) ORDER BY v.id`, s.tenant, ids)
	if err != nil {
		return err
	}
	// versionFacet is a version's access facet (access.Rule.Facet of its
	// effective rule) and effective entities.
	type versionFacet struct {
		VersionID string           `json:"version_id"`
		Level     access.Level     `json:"level"`
		Tiers     []string         `json:"tiers"`
		EntityID  *string          `json:"entity_id"`
		Releases  []access.Release `json:"releases"`
		Entities  []string         `json:"entities"`
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
		facets = append(facets, versionFacet{VersionID: id, Level: f.Level, Tiers: f.Tiers, EntityID: f.EntityID, Releases: f.Releases, Entities: []string{}})
		versions = append(versions, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if s.entities != nil && len(versions) > 0 {
		entities, err := s.entities.EffectiveEntities(ctx, q, versions)
		if err != nil {
			return err
		}
		for i := range facets {
			if gs := entities[facets[i].VersionID]; len(gs) > 0 {
				facets[i].Entities = slices.Compact(slices.Sorted(slices.Values(gs)))
			}
		}
	}
	data, err := json.Marshal(facets)
	if err != nil {
		return err
	}
	if _, err := q.Exec(ctx, `INSERT INTO `+s.t.facets+` AS f (tenant_id, content_id, version_id, kind, live_from, deleted, unplayable, languages,
 access_level, access_tiers, access_entity_id, access_releases, restrictions, entity_ids, traits, owner_entity_id, position, published_at, attributes,
 updated_at)
 SELECT c.tenant_id, c.id, v.id, c.kind,
  CASE WHEN (c.state = 'published' OR (c.state = 'deleted' AND c.published_at IS NOT NULL))
   AND (v.state = 'published' OR (v.state = 'deleted' AND v.published_at IS NOT NULL))
   AND v.released AND c.published_at IS NOT NULL AND v.published_at IS NOT NULL
   THEN greatest(c.published_at, v.published_at) END,
  c.state = 'deleted' OR v.state = 'deleted', c.kind = ANY($4::text[]) AND v.playable IS FALSE, v.languages,
  a.level, a.tiers, a.entity_id, coalesce(a.releases, '[]'::jsonb), c.restrictions, a.entities, v.traits, c.owner_entity_id, v.position, c.published_at,
  c.attributes || v.attributes, now()
 FROM jsonb_to_recordset($3::jsonb) AS a(version_id text, level text, tiers text[], entity_id text, releases jsonb, entities text[])
 JOIN `+s.t.versions+` v ON v.tenant_id = $1 AND v.id = a.version_id
 JOIN `+s.t.items+` c ON c.tenant_id = v.tenant_id AND c.id = v.content_id
 WHERE c.id = ANY($2)
 ON CONFLICT (tenant_id, version_id) DO UPDATE SET content_id = EXCLUDED.content_id, kind = EXCLUDED.kind, live_from = EXCLUDED.live_from,
  deleted = EXCLUDED.deleted, unplayable = EXCLUDED.unplayable, languages = EXCLUDED.languages, access_level = EXCLUDED.access_level,
  access_tiers = EXCLUDED.access_tiers, access_entity_id = EXCLUDED.access_entity_id, access_releases = EXCLUDED.access_releases,
  restrictions = EXCLUDED.restrictions, entity_ids = EXCLUDED.entity_ids, traits = EXCLUDED.traits, owner_entity_id = EXCLUDED.owner_entity_id,
  position = EXCLUDED.position, published_at = EXCLUDED.published_at, attributes = EXCLUDED.attributes,
  updated_at = EXCLUDED.updated_at`,
		s.tenant, ids, string(data), s.playerKinds()); err != nil {
		return err
	}
	if err := s.recordAccessHistory(ctx, q, ids); err != nil {
		return err
	}
	if tx, ok := q.(pgx.Tx); ok {
		if _, err := s.recordReleases(ctx, tx, ids); err != nil {
			return err
		}
	}
	return s.markDocuments(ctx, q, ids)
}

// playerKinds are the kinds whose versions list once their media plays
// (Options.PlayableKinds).
func (s *Store) playerKinds() []string { return append([]string{}, s.playable...) }

// gated reports a kind whose versions list only once their media plays.
func (s *Store) gated(kind string) bool { return slices.Contains(s.playable, kind) }

// startsUnplayable is a new version's playable fact: false for a gated kind,
// until its media records otherwise; unknown (NULL) for others.
func (s *Store) startsUnplayable(kind string) *bool {
	if !s.gated(kind) {
		return nil
	}
	f := false
	return &f
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

// markDocuments marks each edition's text plus existing documents that may
// need removal. Parent-scoped documents are retired during the hard cut.
func (s *Store) markDocuments(ctx context.Context, q Querier, ids []string) error {
	rows, err := q.Query(ctx, `SELECT c.kind,c.id,v.id,l.language FROM `+s.t.items+` c
 JOIN `+s.t.versions+` v ON v.tenant_id=c.tenant_id AND v.content_id=c.id
 CROSS JOIN LATERAL (SELECT t.language FROM `+s.t.versionText+` t WHERE t.tenant_id=v.tenant_id AND t.version_id=v.id
 UNION SELECT unnest(v.languages)) l
 WHERE c.tenant_id=$1 AND c.id=ANY($2)
 UNION SELECT d.content_kind,d.content_id,d.content_version_id,d.language FROM `+s.t.docs+` d
 WHERE d.tenant_id=$1 AND d.content_id=ANY($2) AND EXISTS(SELECT 1 FROM `+s.t.items+` c WHERE c.tenant_id=d.tenant_id AND c.id=d.content_id AND c.kind=d.content_kind)`, s.tenant, ids)
	if err != nil {
		return err
	}
	var marks []search.DirtyMark
	for rows.Next() {
		var kind, id, language string
		var version *string
		if err := rows.Scan(&kind, &id, &version, &language); err != nil {
			rows.Close()
			return err
		}
		ref := s.Ref(kind, id)
		ref.ContentVersionID = version
		marks = append(marks, search.DirtyMark{DocumentKey: search.DocumentKey{ContentRef: ref, Language: language}, Deleted: version == nil, Reason: "catalog"})
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

// EntitiesRenamed reindexes editions whose effective shared/version metadata
// contains the renamed entity, using the normal transaction-local admission.
func (s *Store) EntitiesRenamed(ctx context.Context, q entities.Querier, entityIDs []string) error {
	rows, err := q.Query(ctx, `SELECT DISTINCT f.content_id FROM `+s.t.facets+` f WHERE f.tenant_id=$1 AND f.entity_ids && $2::text[] ORDER BY f.content_id`, s.tenant, entityIDs)
	if err != nil {
		return err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil || len(ids) == 0 {
		return err
	}
	return s.markDocuments(ctx, q, ids)
}

// ContentChanged is entities.Options.ContentChanged: after the entities module
// re-derived the items' entity_ids in q's transaction, it re-derives their
// decorative paths (a path entity may have changed), queues their search
// documents (entity names are keywords) and records the change.
func (s *Store) ContentChanged(ctx context.Context, q entities.Querier, contentIDs []string) error {
	tx, ok := q.(pgx.Tx)
	if !ok {
		return errors.New("catalog: ContentChanged requires the mutation transaction")
	}
	ids := slices.Compact(slices.Sorted(slices.Values(contentIDs)))
	if len(ids) == 0 {
		return nil
	}
	if err := s.putCodes(ctx, q, ids); err != nil {
		return err
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
	return s.emit(ctx, tx, events...)
}
